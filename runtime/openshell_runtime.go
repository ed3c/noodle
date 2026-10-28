package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"github.com/poteto/noodle/skill"
	"github.com/poteto/noodle/worktree"
)

type OpenShellRuntimeConfig struct {
	RuntimeDir, ContextID string
	Gateway               *OpenShellGateway
	Profile               OpenShellProfile
	SkillResolver         skill.Resolver
	EventSink             dispatcher.SessionEventSink
	MaxConcurrent         int
}

type OpenShellRuntime struct {
	config OpenShellRuntimeConfig
	mu     sync.Mutex
	active map[string]*openShellSession
}

func NewOpenShellRuntime(cfg OpenShellRuntimeConfig) (*OpenShellRuntime, error) {
	if cfg.Gateway == nil || cfg.ContextID == "" || !filepath.IsAbs(cfg.RuntimeDir) || cfg.MaxConcurrent < 0 {
		return nil, fmt.Errorf("openshell runtime owner/context/directory/concurrency invalid")
	}
	if cfg.Profile.CLI == "" || cfg.Profile.PayloadDirectory == "" || len(cfg.Profile.Routes) == 0 {
		return nil, fmt.Errorf("openshell execution profile incomplete")
	}
	if err := os.MkdirAll(cfg.RuntimeDir, 0700); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(cfg.RuntimeDir)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeDir = canonical
	return &OpenShellRuntime{config: cfg, active: make(map[string]*openShellSession)}, nil
}
func (r *OpenShellRuntime) CapacityAvailable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.config.MaxConcurrent == 0 || len(r.active) < r.config.MaxConcurrent
}
func (r *OpenShellRuntime) Terminate(s SessionHandle) error { return s.Terminate() }
func (r *OpenShellRuntime) ForceKill(s SessionHandle) error { return s.ForceKill() }

// Local preparation fails before native submission. Once registered, the
// session owns every possible remote effect, including a lost Create response.
func (r *OpenShellRuntime) Dispatch(ctx context.Context, req DispatchRequest) (SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.config.MaxConcurrent > 0 && len(r.active) >= r.config.MaxConcurrent {
		return nil, fmt.Errorf("openshell concurrency limit reached")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	route, ok := r.config.Profile.Routes[req.Provider]
	if !ok || route.Spec == nil {
		return nil, fmt.Errorf("openshell harness %q not configured", req.Provider)
	}
	if req.Provider == "pi" && req.Model != "openrouter/deepseek/deepseek-v4.1-flash-1017" {
		return nil, fmt.Errorf("openshell Pi model differs from selected DeepSeek route")
	}
	for key := range req.EnvVars {
		switch key {
		case "NOODLE_PROJECT_DIR", "NOODLE_ORDER_ID", "NOODLE_STAGE_INDEX":
		default:
			return nil, fmt.Errorf("openshell task environment override %q unsupported", key)
		}
	}
	if !req.AllowPrimaryCheckout {
		canonical, err := worktree.ValidateLinkedCheckout(req.WorktreePath)
		if err != nil {
			return nil, fmt.Errorf("openshell worktree enforcement: %w", err)
		}
		req.WorktreePath = canonical
	}
	if err := VerifyOpenShellPayload(r.config.Profile); err != nil {
		return nil, err
	}
	if err := workspaceartifacts.RequireClean(ctx, req.WorktreePath); err != nil {
		return nil, err
	}
	plan, err := dispatcher.PrepareRemoteHarness(r.config.RuntimeDir, r.config.SkillResolver, req, route.Binary, "/sandbox/noodle-sessions")
	if err != nil {
		return nil, err
	}
	baseline, err := workspaceartifacts.Capture(ctx, req.WorktreePath, filepath.Join(plan.Directory, "repo"))
	if err != nil {
		return nil, err
	}
	order := req.EnvVars["NOODLE_ORDER_ID"]
	if order == "" {
		order = req.Name
	}
	attempt, err := openShellUUID()
	if err != nil {
		return nil, err
	}
	s := newOpenShellSession(r, plan.ID, plan.Directory)
	s.receipt = openShellDispatchReceipt{Version: 1, ContextID: r.config.ContextID, OrderID: order, AttemptID: attempt, SessionID: plan.ID, Target: req.WorktreePath, Baseline: baseline, Phase: "prepared", Outcome: SessionOutcome{Status: StatusFailed, Reason: "OpenShell attempt has not completed", ExitCode: -1}}
	if err := s.persist(); err != nil {
		return nil, err
	}
	r.active[s.id] = s
	go s.run(ctx, plan, req, route)
	return s, nil
}

type openShellDispatchReceipt struct {
	CandidateHead                                                  string
	Version                                                        int
	ContextID, OrderID, AttemptID, SessionID, Target, Phase, Error string
	Baseline                                                       *workspaceartifacts.Baseline
	Worker                                                         *OpenShellWorkerRecord
	Outcome                                                        SessionOutcome
	Delivery                                                       *workspaceartifacts.Result
}

type openShellSession struct {
	owner         *OpenShellRuntime
	id, dir       string
	mu            sync.Mutex
	receipt       openShellDispatchReceipt
	custody       *OpenShellCustody
	worker        *OpenShellWorker
	model         dispatcher.Session
	exec          *OpenShellOwnedExec
	cancelExec    context.CancelFunc
	stopRequested chan struct{}
	stopOnce      sync.Once
	boundaryOnce  sync.Once
	boundaryDone  chan struct{}
	boundaryError error
	done          chan struct{}
	doneOnce      sync.Once
}

func newOpenShellSession(r *OpenShellRuntime, id, dir string) *openShellSession {
	return &openShellSession{owner: r, id: id, dir: dir, stopRequested: make(chan struct{}), boundaryDone: make(chan struct{}), done: make(chan struct{})}
}
func (s *openShellSession) ID() string            { return s.id }
func (s *openShellSession) Done() <-chan struct{} { return s.done }
func (s *openShellSession) Outcome() SessionOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receipt.Outcome
}
func (s *openShellSession) TotalCost() float64 {
	s.mu.Lock()
	m := s.model
	s.mu.Unlock()
	if m != nil {
		return m.TotalCost()
	}
	return 0
}
func (s *openShellSession) VerdictPath() string {
	return filepath.Join(s.owner.config.RuntimeDir, "quality", s.id+".json")
}
func (s *openShellSession) Controller() AgentController { return NoopController() }
func (s *openShellSession) Terminate() error {
	s.stopOnce.Do(func() { close(s.stopRequested) })
	return nil
}
func (s *openShellSession) ForceKill() error { return s.Terminate() }
func (s *openShellSession) interrupted() bool {
	select {
	case <-s.stopRequested:
		return true
	default:
		return false
	}
}
func (s *openShellSession) persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}
func (s *openShellSession) persistLocked() error {
	if s.worker != nil {
		v := s.worker.Record()
		s.receipt.Worker = &v
	}
	b, e := json.Marshal(s.receipt)
	if e != nil {
		return e
	}
	return filex.WriteFileAtomicDurable(filepath.Join(s.dir, "openshell-dispatch.json"), b)
}
func (s *openShellSession) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receipt.Error == "" {
		s.receipt.Error = err.Error()
	} else {
		s.receipt.Error = errors.Join(errors.New(s.receipt.Error), err).Error()
	}
	s.receipt.Outcome = SessionOutcome{Status: StatusFailed, Reason: s.receipt.Error, ExitCode: -1}
	if e := s.persistLocked(); e != nil {
		s.receipt.Error = errors.Join(err, e).Error()
		s.receipt.Outcome.Reason = s.receipt.Error
	}
}
func (s *openShellSession) complete() {
	s.doneOnce.Do(func() { s.owner.mu.Lock(); delete(s.owner.active, s.id); s.owner.mu.Unlock(); close(s.done) })
}
func (s *openShellSession) phase(phase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipt.Phase = phase
	return s.persistLocked()
}

func (s *openShellSession) run(ctx context.Context, plan *dispatcher.RemoteHarness, req DispatchRequest, route OpenShellRoute) {
	// Cancellation requests the original VM stop. It never substitutes a local
	// stream cancellation for proof that the remote worker stopped.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Terminate()
		case <-s.done:
		}
	}()
	if ctx.Err() != nil || s.interrupted() {
		s.fail(fmt.Errorf("openshell dispatch cancelled before submission"))
		s.complete()
		return
	}
	op, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	custody, err := s.owner.config.Gateway.Acquire(op, s.owner.config.ContextID)
	cancel()
	if err != nil {
		s.fail(err)
		s.complete()
		return
	}
	s.custody = custody
	if err = s.phase("creating"); err != nil {
		s.fail(err)
		s.complete()
		return
	}
	op, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	name, err := custody.Create(op, OpenShellTask{OrderID: s.receipt.OrderID, AttemptID: s.receipt.AttemptID, SessionID: s.id}, route.Spec)
	cancel()
	if err != nil {
		s.fail(err)
		if name == "" {
			s.complete()
		}
		return
	}
	var identity OpenShellIdentity
	for _, record := range custody.Snapshot().Records {
		if record.Name == name {
			identity = record.Worker.Identity
			break
		}
	}
	worker, err := NewOpenShellWorker(identity)
	if err != nil {
		s.fail(err)
		return
	}
	s.mu.Lock()
	s.worker = worker
	s.mu.Unlock()
	go func() {
		select {
		case <-s.stopRequested:
			s.stopBoundary()
		case <-s.done:
		}
	}()
	if err = s.phase("provisioning"); err == nil {
		err = s.waitReady()
	}
	if err == nil && !s.interrupted() {
		err = s.transfer("upload", plan.Directory+string(filepath.Separator)+"repo", "/sandbox")
	}
	if err == nil && !s.interrupted() {
		err = VerifyOpenShellPayload(s.owner.config.Profile)
	}
	if err == nil && !s.interrupted() {
		err = s.transfer("upload", s.owner.config.Profile.PayloadDirectory, "/sandbox")
	}
	if err == nil && !s.interrupted() {
		err = s.phase("executing")
	}
	if err != nil || s.interrupted() {
		if err == nil {
			err = fmt.Errorf("openshell dispatch stopped before model launch")
		}
		s.fail(err)
		s.finishStopped()
		return
	}
	requestID, err := openShellUUID()
	if err != nil {
		s.fail(err)
		s.finishStopped()
		return
	}
	env := map[string]string{"NOODLE_PROJECT_DIR": "/sandbox/repo", "NOODLE_ORDER_ID": s.receipt.OrderID, "NOODLE_STAGE_INDEX": req.EnvVars["NOODLE_STAGE_INDEX"], "NOODLE_SESSION_DIR": plan.GuestSessionDir}
	execCtx, cancelExec := context.WithCancel(context.Background())
	defer cancelExec()
	s.mu.Lock()
	s.cancelExec = cancelExec
	s.mu.Unlock()
	if s.interrupted() {
		cancelExec()
	}
	owned, launchErr := custody.StartOwned(execCtx, identity.Sandbox, s.receipt.AttemptID, OpenShellCommand{RequestID: requestID, Argv: plan.Command(), Workdir: "/sandbox/repo", Env: env})
	if owned == nil {
		s.fail(launchErr)
		s.finishStopped()
		return
	}
	s.mu.Lock()
	s.exec = owned
	s.mu.Unlock()
	process := newOpenShellHarnessProcess(owned, s.Terminate)
	model, err := plan.Start(context.Background(), process, s.owner.config.EventSink)
	if err != nil {
		s.fail(err)
		_ = owned.Close()
		s.finishStopped()
		return
	}
	s.mu.Lock()
	s.model = model
	s.mu.Unlock()
	if launchErr != nil {
		s.fail(launchErr)
		_ = s.Terminate()
	}
	<-model.Done()
	result := process.result()
	worker.RecordExecResult(result)
	outcome := model.Outcome()
	worker.RecordModelOutcome(outcome)
	s.mu.Lock()
	s.receipt.Outcome = outcome
	s.mu.Unlock()
	if launchErr != nil || !result.ExitObserved || result.ExitCode != 0 || result.InputError != "" || result.StreamError != "" {
		s.fail(fmt.Errorf("openshell native execution incomplete: %s; input=%s; stream=%s", result.Reason, result.InputError, result.StreamError))
	}
	if err = s.phase("returning"); err != nil {
		s.fail(err)
		s.finishStopped()
		return
	}
	// Native SSH cannot connect after VM Stopped. Download into evidence first;
	// apply only after the original VM is conclusively stopped.
	if !s.interrupted() {
		if err = s.downloadArchive("noodle-result.tar", filepath.Join(s.dir, "returned")); err != nil {
			s.fail(err)
		}
		if err == nil {
			err = s.downloadArchive("noodle-session.tar", filepath.Join(s.dir, "guest-session"))
			if err != nil {
				s.fail(err)
			}
		}
	} else {
		err = fmt.Errorf("openshell stopped before complete artifact return")
		s.fail(err)
	}
	s.stopBoundary()
	if s.boundaryError != nil {
		s.fail(s.boundaryError)
		return
	}
	if err == nil && s.Outcome().Status == StatusCompleted && !s.interrupted() {
		if err = s.phase("delivery-in-progress"); err != nil {
			s.fail(err)
			return
		}
		delivery, applyErr := workspaceartifacts.Apply(context.Background(), req.WorktreePath, s.receipt.Baseline, filepath.Join(s.dir, "returned"), filepath.Join(s.dir, "delivery"))
		s.mu.Lock()
		s.receipt.Delivery = delivery
		s.mu.Unlock()
		if applyErr != nil {
			s.fail(applyErr)
			if delivery != nil && delivery.State == "unknown" {
				_ = s.phase("delivery-unknown")
				return
			}
		}
		if applyErr == nil && delivery.Applied {
			if err = s.phase("publishing"); err != nil {
				s.fail(err)
				return
			}
			message := fmt.Sprintf("feat: retain OpenShell worker output\n\nRecord the validated result for order %s from native session %s after its original VM stopped. The candidate remains subject to the existing review process.", s.receipt.OrderID, s.id)
			candidate, publishErr := workspaceartifacts.Publish(context.Background(), req.WorktreePath, s.receipt.Baseline, delivery, message)
			if publishErr != nil {
				s.fail(publishErr)
				return
			} else {
				s.mu.Lock()
				s.receipt.CandidateHead = candidate
				s.mu.Unlock()
			}
		}
	}
	if err = s.phase("settled"); err != nil {
		s.fail(err)
		return
	}
	s.complete()
}

func (s *openShellSession) waitReady() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.interrupted() {
			return fmt.Errorf("openshell provisioning interrupted")
		}
		if err := s.worker.Readback(ctx, s.custody); err != nil {
			return err
		}
		if s.worker.Record().Boundary == OpenShellBoundaryReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("openshell VM readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
func (s *openShellSession) finishStopped() {
	s.stopBoundary()
	if s.boundaryError != nil {
		s.fail(s.boundaryError)
		return
	}
	if err := s.phase("settled"); err != nil {
		s.fail(err)
		return
	}
	s.complete()
}
func (s *openShellSession) stopBoundary() {
	s.boundaryOnce.Do(func() {
		defer close(s.boundaryDone)
		s.mu.Lock()
		cancelExec := s.cancelExec
		s.mu.Unlock()
		if s.interrupted() && cancelExec != nil {
			cancelExec()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := s.worker.Stop(ctx, s.custody, s.custody)
		for err == nil && s.worker.Record().Boundary != OpenShellBoundaryStopped {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(250 * time.Millisecond):
				err = s.worker.Readback(ctx, s.custody)
			}
		}
		s.boundaryError = err
		s.mu.Lock()
		e := s.exec
		s.mu.Unlock()
		if e != nil && s.interrupted() {
			_ = e.Close()
		}
		if p := s.persist(); p != nil {
			s.boundaryError = errors.Join(err, p)
		}
	})
	<-s.boundaryDone
}

func (r *OpenShellRuntime) Recover(ctx context.Context) ([]RecoveredSession, error) {
	paths, err := filepath.Glob(filepath.Join(r.config.RuntimeDir, "sessions", "*", "openshell-dispatch.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	custody, err := r.config.Gateway.Acquire(ctx, r.config.ContextID)
	if err != nil {
		return nil, err
	}
	var recovered []RecoveredSession
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var receipt openShellDispatchReceipt
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return nil, err
		}
		if receipt.Version != 1 {
			return nil, fmt.Errorf("openshell saved dispatch version unsupported")
		}
		if receipt.Phase == "settled" {
			continue
		}
		if receipt.ContextID != r.config.ContextID {
			return nil, fmt.Errorf("openshell saved dispatch binding conflicts with context")
		}
		s := newOpenShellSession(r, receipt.SessionID, filepath.Dir(path))
		s.receipt = receipt
		s.custody = custody
		var original *OpenShellCustodyRecord
		for _, v := range custody.Snapshot().Records {
			if v.Task.SessionID == s.id && v.Task.AttemptID == receipt.AttemptID && v.Task.OrderID == receipt.OrderID {
				copy := v
				original = &copy
				break
			}
		}
		r.mu.Lock()
		r.active[s.id] = s
		r.mu.Unlock()
		if original == nil {
			s.fail(fmt.Errorf("interrupted before native create; explicit new attempt required"))
			s.complete()
		} else if original.CreateAcknowledged {
			w, readErr := custody.RecoverWorker(ctx, original.Worker.Identity.Sandbox)
			s.worker = w
			s.fail(errors.Join(fmt.Errorf("interrupted attempt retained at %s; inspect original VM and local delivery evidence before an explicit new attempt; no operation replayed", receipt.Phase), readErr))
			if w != nil && readErr == nil && w.Record().Boundary == OpenShellBoundaryStopped && !openShellDeliveryPending(receipt.Phase) {
				s.complete()
			} else if w != nil && !openShellDeliveryPending(receipt.Phase) {
				go func() {
					select {
					case <-s.stopRequested:
						s.finishStopped()
					case <-s.done:
					}
				}()
			}
		} else {
			s.fail(fmt.Errorf("original native create acknowledgement unresolved"))
		}
		recovered = append(recovered, RecoveredSession{OrderID: receipt.OrderID, SessionHandle: s, RuntimeName: "openshell", Reason: s.Outcome().Reason})
	}
	return recovered, nil
}

func openShellDeliveryPending(phase string) bool {
	return phase == "delivery-in-progress" || phase == "delivery-unknown" || phase == "publishing"
}
