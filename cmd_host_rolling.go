package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

func newHostRollingCmd() *cobra.Command {
	var binary string
	var inspect bool
	var maintenancePlan bool
	var bootstrap bool
	var requestFiles []string
	var handoverRoot, failureRecovery, sourcePlan string
	cmd := &cobra.Command{Use: "switch", Short: "Select the next Host version; keep active workers on their current generation", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if maintenancePlan && (inspect || bootstrap) {
			return errors.New("maintenance-plan is read-only and cannot be combined with status or initial-maintenance")
		}
		if handoverRoot != "" && !maintenancePlan {
			return errors.New("handover-root requires the read-only maintenance-plan entry")
		}
		if sourcePlan != "" && failureRecovery == "" {
			return errors.New("source-plan-file requires qualified failure-recovery-file")
		}
		if failureRecovery != "" && (!maintenancePlan || handoverRoot == "") {
			return errors.New("failure-recovery-file requires handover-root with maintenance-plan")
		}
		var root string
		var err error
		if maintenancePlan {
			root, err = existingHostDirectory()
		} else {
			root, err = hostDirectory()
		}
		if err != nil {
			return err
		}
		if inspect {
			if b, readErr := readHostBootstrap(root); readErr == nil {
				if b.Phase != "active" {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(b)
				}
			} else if !os.IsNotExist(readErr) {
				return readErr
			}
			p, err := readHostPool(root)
			if os.IsNotExist(err) {
				legacy, readErr := legacyHostVersionReadback(cmd.Context(), root)
				if readErr != nil {
					return readErr
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(legacy)
			}
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		if !filepath.IsAbs(binary) {
			return errors.New("target binary must be absolute")
		}
		info, err := os.Lstat(binary)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("target is not a regular executable")
		}
		binary, err = filepath.EvalSymlinks(binary)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(binary)
		if err != nil {
			return err
		}
		digest := hostDigest(data)
		if err = verifyRollingTarget(cmd.Context(), binary, digest); err != nil {
			return err
		}
		if maintenancePlan {
			if handoverRoot != "" {
				plan, planErr := planGatewayHandoverWithRecovery(cmd.Context(), root, handoverRoot, binary, digest, requestFiles, failureRecovery, sourcePlan)
				if planErr != nil {
					return planErr
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
			}
			plan, planErr := planHostMaintenance(cmd.Context(), root, binary, digest, requestFiles)
			if planErr != nil {
				return planErr
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
		}
		if bootstrap {
			return bootstrapLegacyHost(cmd.Context(), root, binary, digest, requestFiles, json.NewEncoder(cmd.OutOrStdout()))
		}
		if waiting := maintenanceWaitResponse(root); waiting != nil {
			return errors.New(waiting.Error)
		}
		if err = ensureRollingHost(cmd.Context(), binary, root); err != nil {
			if _, poolErr := os.Stat(filepath.Join(root, "pool.json")); os.IsNotExist(poolErr) {
				if legacy, readErr := legacyHostVersionReadback(cmd.Context(), root); readErr == nil {
					legacy["target_binary"] = binary
					legacy["error"] = err.Error()
					return json.NewEncoder(cmd.OutOrStdout()).Encode(legacy)
				}
			}
			return err
		}
		var result hostVersionRequest
		err = updateHostPool(root, func(p *hostPool) error {
			for _, r := range p.Queue {
				if r.Binary == binary && r.SHA == digest && (r.State == "queued" || r.State == "starting" || r.State == "blocked") {
					result = r
					return nil
				}
			}
			active := p.Generations[p.Active]
			if active.Binary == binary && active.SHA == digest {
				result = hostVersionRequest{Binary: binary, SHA: digest, State: "active", Generation: active.ID}
				return nil
			}
			id, err := hostID()
			if err != nil {
				return err
			}
			result = hostVersionRequest{ID: id, Binary: binary, SHA: digest, State: "queued"}
			p.Queue = append(p.Queue, result)
			return nil
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&binary, "binary", "", "Selected immutable Noodle executable")
	cmd.Flags().BoolVar(&inspect, "status", false, "Read original generation, owner and version-queue records")
	cmd.Flags().BoolVar(&maintenancePlan, "maintenance-plan", false, "Read source-bound maintenance steps and blockers; never send control or rewrite owner records")
	cmd.Flags().StringVar(&handoverRoot, "handover-root", "", "Retire a completed private owner into this existing shared Host; read-only with maintenance-plan")
	cmd.Flags().StringVar(&sourcePlan, "source-plan-file", "", "Original CLI plan with kernel source identity, only when that generation is already absent")
	cmd.Flags().StringVar(&failureRecovery, "failure-recovery-file", "", "Original qualified storage failure recovery plan; resource retirement only")
	cmd.Flags().BoolVar(&bootstrap, "initial-maintenance", false, "Drain the verified legacy Host through its original controls and migrate once")
	cmd.Flags().StringArrayVar(&requestFiles, "request-file", nil, "Original accepted attach request (repeat for contexts without an external config path)")
	return cmd
}

func legacyHostVersionReadback(ctx context.Context, root string) (map[string]any, error) {
	raw, err := nativeHostRPC(ctx, root, hostRequest{Operation: "resources"})
	if err != nil {
		return nil, err
	}
	var observed struct {
		Protocol string `json:"protocol"`
		Instance string `json:"host_instance_id"`
		PID      int    `json:"host_pid"`
		Rolling  int    `json:"rolling_host_version"`
	}
	if err = json.Unmarshal(raw, &observed); err != nil {
		return nil, err
	}
	if observed.Protocol != hostProtocol || observed.Instance == "" || observed.PID <= 0 || observed.Rolling != 0 {
		return nil, errors.New("original legacy Host state is not confirmed")
	}
	return map[string]any{"schema": 1, "state": "maintenance_required", "reason": "original_live_host_has_no_bound_rolling_generation", "native_resources": json.RawMessage(raw), "effects": []string{}, "next": nil}, nil
}

func verifyRollingTarget(ctx context.Context, binary, digest string) error {
	info, err := os.Lstat(binary)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("target is not a regular executable")
	}
	data, err := os.ReadFile(binary)
	if err != nil || hostDigest(data) != digest {
		return errors.New("target binary changed")
	}
	cmd := hostCapabilityCommand(ctx, binary)
	raw, err := cmd.Output()
	if err != nil {
		return err
	}
	var caps struct {
		Protocol    string `json:"protocol"`
		Rolling     int    `json:"rolling_host_version"`
		Quota       int    `json:"shared_worker_quota"`
		Native      int    `json:"codex_native_runtime"`
		Warning     int    `json:"task_warning_grant"`
		Maintenance int    `json:"restricted_host_maintenance"`
		Policy      string `json:"worker_admission_policy"`
	}
	if json.Unmarshal(raw, &caps) != nil || caps.Protocol != hostProtocol || caps.Rolling != 1 || caps.Quota != 1 || caps.Native != 1 || caps.Warning != 1 || caps.Maintenance != 1 || caps.Policy != admission.WorkerAdmissionPolicy {
		return errors.New("target lacks compatible rolling Host and shared-quota contracts")
	}
	review, err := exec.CommandContext(ctx, binary, "host", "task", "review-capabilities").Output()
	if err != nil {
		return err
	}
	var effects struct {
		Directory int `json:"local_task_directory_scope"`
		Roles     int `json:"local_task_roles"`
	}
	if json.Unmarshal(review, &effects) != nil || effects.Directory != 1 || effects.Roles != 1 {
		return errors.New("target lacks the existing local-task directory/role contracts")
	}
	return nil
}

func (h *noodleHost) joinRolling() error {
	root, id := os.Getenv("NOODLE_HOST_POOL_ROOT"), os.Getenv("NOODLE_HOST_GENERATION")
	if root == "" && id == "" {
		return nil
	}
	if !filepath.IsAbs(root) || !validPoolID(id) || h.dir != filepath.Join(root, "g", id) || h.instanceID != id {
		return errors.New("native Host generation identity differs")
	}
	p, err := readHostPool(root)
	if err != nil {
		return err
	}
	g, ok := p.Generations[id]
	if !ok || g.Binary != h.binaryPath || g.SHA != h.binarySHA {
		return errors.New("generation does not match admitted executable")
	}
	fence, err := lockfile.TryLockShared(filepath.Join(root, "host.lock"))
	if err != nil {
		return err
	}
	if err = h.budget.BindSharedQuota(filepath.Join(root, "quota"), id); err != nil {
		fence.Close()
		return err
	}
	h.rolling = &hostRolling{root: root, generation: id, retiring: p.Active != "" && p.Active != id && g.State != "starting", carry: map[string]rollingCarry{}, fence: fence}
	return nil
}

func (h *noodleHost) attachRolling(ctx context.Context, req hostRequest, state *loop.HostContinuation) hostResponse {
	if h.rolling != nil {
		if err := updateHostPool(h.rolling.root, func(p *hostPool) error {
			owner, exists := p.Owners[req.RequestID]
			if exists && owner.MaintenanceHold != "" {
				return errors.New("original task is held by restricted maintenance; business admission is disabled")
			}
			if exists && owner.Current.HostInstanceID != h.instanceID && owner.State != "ready" {
				return errors.New("task already has a different native owner")
			}
			if !exists {
				originalSHA, originalPath := req.AdmissionSHA, req.AdmissionPath
				if originalSHA == "" {
					originalSHA = req.BinarySHA256
				}
				if originalPath == "" {
					for _, g := range p.Generations {
						if g.SHA == originalSHA {
							if originalPath != "" && originalPath != g.Binary {
								return errors.New("admission binary provenance is ambiguous")
							}
							originalPath = g.Binary
						}
					}
				}
				known := false
				for _, g := range p.Generations {
					if g.SHA == originalSHA && g.Binary == originalPath {
						known = true
					}
				}
				if !known {
					return errors.New("admission binary is outside the native version family")
				}
				r := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: os.Getpid(), HostInstanceID: h.instanceID, ProjectDir: req.ProjectDir, Mode: req.Mode, ConfigPath: req.ConfigPath, ConfigSHA256: req.ConfigSHA256, BinaryPath: originalPath, BinarySHA256: originalSHA, RequestID: req.RequestID, HostDir: h.controlRoot(), CleanupCompleted: req.CleanupCompleted}
				owner = hostTaskOwner{AdmissionOnly: originalSHA != h.binarySHA || originalPath != h.binaryPath, Original: r, Current: r, EnvironmentSHA: hostDigest([]byte(strings.Join(req.Environment, "\x00")))}
			}
			if !exists {
				owner.Current.BinarySHA256 = h.binarySHA
				owner.Current.BinaryPath = h.binaryPath
			}
			owner.State = "admitting"
			owner.ControllerOnly = req.ControllerOnly
			p.Owners[req.RequestID] = owner
			return nil
		}); err != nil {
			return hostError("unknown_effect", err)
		}
	}
	response := h.attachContinuation(ctx, req, state)
	if h.rolling == nil {
		return response
	}
	if response.Status != "ok" {
		// Keep the original rejection for reconciliation; absence of an ACK is not
		// evidence that a second execution is safe.
		if err := updateHostPool(h.rolling.root, func(p *hostPool) error {
			owner := p.Owners[req.RequestID]
			owner.LastAdmission = &response
			p.Owners[req.RequestID] = owner
			return nil
		}); err != nil {
			return hostError("unknown_effect", errors.Join(errors.New(response.Error), err))
		}
		return response
	}
	err := updateHostPool(h.rolling.root, func(p *hostPool) error {
		owner, exists := p.Owners[req.RequestID]
		if exists && owner.Current.HostInstanceID != h.instanceID && owner.State != "ready" && owner.State != "admitting" {
			return errors.New("original task still belongs to another live generation")
		}
		if !exists || (owner.Original.ContextID == "" && !owner.AdmissionOnly) {
			owner.Original = response.hostReceipt
			owner.EnvironmentSHA = hostDigest([]byte(strings.Join(req.Environment, "\x00")))
		}
		if exists && owner.Current.ContextID != response.ContextID {
			owner.Previous = append(owner.Previous, owner.Current)
		}
		owner.Current = response.hostReceipt
		owner.State = "owned"
		owner.ControllerOnly = req.ControllerOnly
		owner.RuntimeEnvironmentSHA = hostDigest([]byte(strings.Join(req.Environment, "\x00")))
		if owner.ContextInputs == nil && !req.ControllerOnly {
			inputs, inputErr := captureHostContextInputs(req)
			if inputErr != nil {
				owner.RebuildError = inputErr.Error()
			} else {
				owner.ContextInputs = &inputs
				owner.RebuildError = ""
			}
		}
		p.Owners[req.RequestID] = owner
		return nil
	})
	if err != nil {
		return hostResponse{response.hostReceipt, "unknown_effect", err.Error()}
	}
	return response
}

func (h *noodleHost) rollingRound(ctx context.Context, req hostRequest) hostResponse {
	if h.rolling == nil || req.RoundState == nil {
		return hostError("refused", errors.New("native round state missing"))
	}
	p, err := readHostPool(h.rolling.root)
	if err != nil {
		return hostError("unknown_effect", err)
	}
	owner, ok := p.Owners[req.RequestID]
	if ok && owner.MaintenanceHold != "" {
		return hostError("busy", errors.New("original round is held by restricted maintenance"))
	}
	if !ok || p.Active != h.instanceID {
		return hostError("refused", errors.New("next round has no selected native owner"))
	}
	if owner.Current.HostInstanceID == h.instanceID && owner.State == "owned" {
		req.Operation = "status"
		req.BinarySHA256 = h.binarySHA
		req.ContextID = ""
		req.InstanceID = ""
		return h.handle(ctx, req)
	}
	expectedEnvironment := owner.RuntimeEnvironmentSHA
	if expectedEnvironment == "" {
		expectedEnvironment = owner.EnvironmentSHA
	}
	if owner.State != "ready" || owner.Current.HostInstanceID != req.RoundSource || owner.Current.ContextState != "detached" || owner.Current.TerminalCause != loop.TerminalMigrationDetach || !hostReceiptMatches(req, owner.Original) || hostDigest([]byte(strings.Join(req.Environment, "\x00"))) != expectedEnvironment {
		return hostError("refused", errors.New("round boundary or original task binding differs"))
	}
	req.Operation = "attach"
	req.BinarySHA256 = h.binarySHA
	// Same request + the project lock make a lost reply readable without another writer.
	for i := len(h.receipts) - 1; i >= 0; i-- {
		if hostReceiptMatches(req, h.receipts[i]) && h.receipts[i].BinarySHA256 == h.binarySHA {
			return h.attachRollingReadback(req, h.receipts[i])
		}
	}
	return h.attachRolling(ctx, req, req.RoundState)
}
func (h *noodleHost) attachRollingReadback(req hostRequest, r hostReceipt) hostResponse {
	if err := updateHostPool(h.rolling.root, func(p *hostPool) error {
		o := p.Owners[req.RequestID]
		if o.Original.ContextID == "" && !o.AdmissionOnly {
			o.Original = r
		}
		if o.Current.HostInstanceID != r.HostInstanceID {
			o.Previous = append(o.Previous, o.Current)
		}
		o.Current = r
		o.State = "owned"
		if r.ContextState != "active" {
			o.State = "terminal"
		}
		p.Owners[req.RequestID] = o
		return nil
	}); err != nil {
		return hostResponse{r, "unknown_effect", err.Error()}
	}
	return hostResponse{r, "ok", ""}
}

func (h *noodleHost) advanceRolling(ctx context.Context) bool {
	r := h.rolling
	if r.failure != "" {
		if len(r.carry) == 0 {
			return false
		}
		p, err := readHostPool(r.root)
		if err != nil {
			return false
		}
		target := p.Generations[p.Active]
		for id, carry := range r.carry {
			req := carry.request
			req.Operation = "round_reconcile"
			raw, err := nativeHostRPC(ctx, target.Directory, req)
			if err != nil {
				return false
			}
			if err = confirmRollingRound(r.root, req, target, raw); err != nil {
				return false
			}
			delete(r.carry, id)
		}
		r.failure = ""
	}
	p, err := readHostPool(r.root)
	if err != nil {
		r.failure = err.Error()
		return false
	}
	for _, c := range h.contexts {
		owner, ok := p.Owners[c.receipt.RequestID]
		if ok && owner.State == "admitting" && !c.failed && !h.blocked && c.receipt.ContextState == "active" {
			select {
			case <-c.runtime.Ready():
				h.attachRollingReadback(c.request, c.receipt)
			default:
			}
		}
	}
	if p.Active == h.instanceID {
		h.rebuildReleasedRounds(ctx, p)
		if err = repairPoolSocket(r.root, h.instanceID); err != nil {
			r.failure = err.Error()
			return false
		}
		h.observeRetiredGenerations(&p)
		for _, q := range p.Queue {
			if q.State != "queued" {
				continue
			}
			// A generation switch must not create a competing gateway owner.
			// Workers finish normally; callers retain the queued version and task.
			if h.openShell != nil {
				if h.openShellContextsActive() {
					return false
				}
				if err = h.closeIdleOpenShell(); err != nil {
					return false
				}
			}
			count := 0
			for _, g := range p.Generations {
				if generationLive(g) {
					count++
				}
			}
			if count >= 2 {
				return false
			}
			if err = verifyRollingTarget(ctx, q.Binary, q.SHA); err != nil {
				h.failVersion(q.ID, err)
				return false
			}
			if err = updateHostPool(r.root, func(p *hostPool) error {
				for i := range p.Queue {
					if p.Queue[i].ID == q.ID {
						p.Queue[i].State = "starting"
					}
				}
				return nil
			}); err != nil {
				return false
			}
			g, launchErr := launchHostGeneration(ctx, r.root, q.Binary, q.SHA)
			if launchErr != nil {
				h.failVersion(q.ID, launchErr)
				return false
			}
			err = updateHostPool(r.root, func(p *hostPool) error {
				if p.Active != h.instanceID {
					return errors.New("active version changed before selection")
				}
				current := p.Generations[h.instanceID]
				current.State = "retiring"
				p.Generations[current.ID] = current
				g.State = "active"
				p.Generations[g.ID] = g
				p.Active = g.ID
				for i := range p.Queue {
					if p.Queue[i].ID == q.ID {
						p.Queue[i].State = "active"
						p.Queue[i].Generation = g.ID
					}
				}
				return nil
			})
			if err != nil {
				r.failure = err.Error()
				return false
			}
			if err = publishPoolSocket(r.root, g.Directory); err != nil {
				r.failure = err.Error()
				return false
			}
			r.retiring = true
			h.budget.HoldForSwitch()
			break
		}
	}
	p, err = readHostPool(r.root)
	if err != nil {
		return false
	}
	if p.Active == h.instanceID || p.Generations[h.instanceID].State == "starting" {
		return false
	}
	r.retiring = true
	h.budget.HoldForSwitch()
	target := p.Generations[p.Active]
	for _, c := range h.contexts {
		if c.failed || c.done == nil {
			continue
		}
		release, ok := c.runtime.(interface {
			ReleaseForSwitch(context.Context) (loop.HostContinuation, error)
		})
		if !ok {
			r.failure = "runtime has no native round boundary"
			return false
		}
		roundCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, releaseErr := release.ReleaseForSwitch(roundCtx)
		if errors.Is(releaseErr, loop.ErrReleaseBusy) {
			cancel()
			continue
		}
		if errors.Is(releaseErr, context.DeadlineExceeded) {
			cancel()
			continue
		}
		if releaseErr != nil {
			cancel()
			r.failure = releaseErr.Error()
			return false
		}
		select {
		case runErr := <-c.done:
			h.finish(c, runErr)
			if runErr != nil {
				r.failure = runErr.Error()
			}
		case <-roundCtx.Done():
			r.failure = roundCtx.Err().Error()
		}
		cancel()
		if r.failure != "" || h.blocked {
			return false
		}
	}
	for id, carry := range r.carry {
		raw, callErr := nativeHostRPC(ctx, target.Directory, carry.request)
		if callErr != nil {
			r.failure = "next-round admission unknown: " + callErr.Error()
			return false
		}
		if err = confirmRollingRound(r.root, carry.request, target, raw); err != nil {
			r.failure = err.Error()
			return false
		}
		delete(r.carry, id)
	}
	if len(h.contexts) != 0 || len(r.carry) != 0 {
		return false
	}
	quota := h.budget.Snapshot()
	if quota.SharedQuota == nil || quota.SharedQuota.Error != "" || quota.OwnedAllocations != 0 {
		return false
	}
	if h.residents != nil {
		wait, cancel := context.WithTimeout(ctx, time.Second)
		err = h.residents.WaitClosed(wait)
		cancel()
		if err != nil {
			return false
		}
	}
	if err = updateHostPool(r.root, func(p *hostPool) error {
		g := p.Generations[h.instanceID]
		g.State = "exiting"
		p.Generations[g.ID] = g
		return nil
	}); err != nil {
		return false
	}
	return true
}

func confirmRollingRound(root string, req hostRequest, target hostGeneration, raw json.RawMessage) error {
	var reply hostResponse
	if err := json.Unmarshal(raw, &reply); err != nil {
		return err
	}
	if reply.Status != "ok" {
		return fmt.Errorf("next-round native response %s: %s", reply.Status, reply.Error)
	}
	if !hostReceiptMatches(req, reply.hostReceipt) || reply.HostInstanceID != target.ID || reply.BinarySHA256 != target.SHA || reply.ContextID == "" {
		return errors.New("next-round ACK does not confirm the original native request")
	}
	p, err := readHostPool(root)
	if err != nil {
		return err
	}
	owner, ok := p.Owners[req.RequestID]
	if !ok || (owner.State != "owned" && owner.State != "terminal") || owner.Current.HostInstanceID != target.ID || owner.Current.ContextID != reply.ContextID || !hostReceiptMatches(req, owner.Current) || owner.Current.BinarySHA256 != target.SHA {
		return errors.New("next-round native ownership persistence is not confirmed")
	}
	return nil
}
func (h *noodleHost) failVersion(id string, failure error) {
	if err := updateHostPool(h.rolling.root, func(p *hostPool) error {
		for i := range p.Queue {
			if p.Queue[i].ID == id {
				p.Queue[i].State = "blocked"
				p.Queue[i].Error = failure.Error()
			}
		}
		return nil
	}); err != nil {
		h.rolling.failure = errors.Join(failure, err).Error()
	}
}
func (h *noodleHost) observeRetiredGenerations(p *hostPool) {
	for id, g := range p.Generations {
		if (g.State != "exiting" && g.State != "retiring") || g.Process.PID <= 0 {
			continue
		}
		if g.State == "retiring" {
			if h.budget == nil {
				continue
			}
			quota := h.budget.Snapshot().SharedQuota
			if quota == nil || quota.Error != "" || quota.Allocations != 0 {
				continue
			}
		}
		absent, err := g.Process.GroupAbsent()
		if err != nil || !absent {
			continue
		}
		if err := updateHostPool(h.rolling.root, func(p *hostPool) error {
			current := p.Generations[id]
			if current.State == "exiting" || current.State == "retiring" {
				// Physical exit does not discharge a task, continuation or quota lease.
				current.State = "exited"
				p.Generations[id] = current
			}
			return nil
		}); err != nil {
			h.rolling.failure = err.Error()
		}
	}
}

func hostCapabilityCommand(ctx context.Context, binary string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, "host", "capabilities")
	return cmd
}

func (h *noodleHost) reconcileRollingRound(req hostRequest) hostResponse {
	if h.rolling == nil {
		return hostError("refused", errors.New("no native version context"))
	}
	p, err := readHostPool(h.rolling.root)
	if err != nil {
		return hostError("unknown_effect", err)
	}
	owner, ok := p.Owners[req.RequestID]
	if !ok || !hostReceiptMatches(req, owner.Original) {
		return hostError("refused", errors.New("original round identity changed"))
	}
	allowed := owner.Current.HostInstanceID == req.RoundSource || owner.Current.HostInstanceID == h.instanceID
	if !allowed {
		return hostError("refused", errors.New("round belongs to another generation"))
	}
	for i := len(h.receipts) - 1; i >= 0; i-- {
		r := h.receipts[i]
		if hostReceiptMatches(req, r) && r.BinarySHA256 == h.binarySHA {
			if owner.State == "admitting" && historicalHostContext(owner, r.ContextID) {
				continue
			}
			if h.blocked || r.ContextState == "failed" {
				return hostResponse{r, "unknown_effect", "native execution needs readback"}
			}
			return h.attachRollingReadback(req, r)
		}
	}
	if owner.LastAdmission != nil {
		return *owner.LastAdmission
	}
	return hostError("unknown_effect", errors.New("no accepted native round observed; no replay attempted"))
}

func (h *noodleHost) recordRollingBoundary(c *hostContext) error {
	producer, ok := c.runtime.(interface {
		HostRoundBoundary() (loop.HostContinuation, bool)
	})
	if !ok {
		return errors.New("native round producer readback unavailable")
	}
	saved, observed := producer.HostRoundBoundary()
	if !observed {
		return errors.New("native round completion state unknown")
	}
	p, err := readHostPool(h.rolling.root)
	if err != nil {
		return err
	}
	target := p.Generations[p.Active]
	owner, exists := p.Owners[c.receipt.RequestID]
	if !exists {
		return errors.New("original native task owner missing")
	}
	req := c.request
	req.Environment = c.execution.Environ()
	req.RoundSource = h.instanceID
	req.RoundState = &saved
	req.Operation = "round_admit"
	req.BinarySHA256 = target.SHA
	req.AdmissionSHA = owner.Original.BinarySHA256
	req.AdmissionPath = owner.Original.BinaryPath
	if target.ID != h.instanceID {
		h.rolling.carry[c.receipt.ContextID] = rollingCarry{request: req}
	}
	return updateHostPool(h.rolling.root, func(p *hostPool) error {
		owner := p.Owners[req.RequestID]
		owner.Current = c.receipt
		owner.State = "ready"
		owner.RoundState = &saved
		p.Owners[req.RequestID] = owner
		return nil
	})
}
