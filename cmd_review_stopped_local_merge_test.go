package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

// Git, custody, plan/intent persistence and CAS are real. Host/native responses
// below are isolated protocol fixtures, not a live VM or completed model claim.
type stoppedMergeFixture struct {
	t                                                           *testing.T
	root, storage, admission, request, host, generation, worker string
	candidate                                                   stoppedMergeRef
	native                                                      hostSessionReadback
	receipt                                                     hostReceipt
	ref                                                         stoppedMergeRef
	detaches                                                    atomic.Int32
	requests                                                    chan string
}

func (f *stoppedMergeFixture) git(root string, args ...string) string {
	f.t.Helper()
	c := exec.Command("git", args...)
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func stoppedTestPut(t *testing.T, path string, v any) stoppedMergeRef {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeOwnerJSON(t, path, v)
	r, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func stoppedTestBytes(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func newStoppedMergeFixture(t *testing.T) *stoppedMergeFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &stoppedMergeFixture{t: t, root: filepath.Join(base, "project"), storage: filepath.Join(base, "storage"), admission: filepath.Join(base, "admission"), requests: make(chan string, 20)}
	if err = os.MkdirAll(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	f.git(f.root, "init", "-q", "-b", "control")
	stoppedTestBytes(t, filepath.Join(f.root, ".gitignore"), []byte(".noodle/\n.worktrees/\n"))
	cfg := []byte("mode=\"manual\"\n[runtime]\nadmitted_orders=[\"original\"]\n")
	stoppedTestBytes(t, filepath.Join(f.root, ".noodle.toml"), cfg)
	stoppedTestBytes(t, filepath.Join(f.root, "original.txt"), []byte("original\n"))
	stoppedTestBytes(t, filepath.Join(f.root, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"))
	f.git(f.root, "add", ".")
	f.git(f.root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Original control root")
	head := f.git(f.root, "rev-parse", "HEAD")
	for _, name := range []string{".noodle", ".worktrees"} {
		if err = os.MkdirAll(filepath.Join(f.storage, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(filepath.Join(f.storage, name), filepath.Join(f.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	common := f.git(f.root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	source := map[string]string{"project": f.root, "common_dir": common, "storage_root": f.storage}
	sourceRef := stoppedTestPut(t, filepath.Join(f.storage, "source.json"), source)
	raw, _ := os.ReadFile(sourceRef.Path)
	stoppedTestBytes(t, filepath.Join(common, "noodle-contexts", hostDigest([]byte(f.root)), "source.json"), raw)
	name := "original-0-local-task"
	f.worker = filepath.Join(f.storage, ".worktrees", name)
	f.git(f.root, "worktree", "add", "-q", "-b", name, f.worker)
	stoppedTestBytes(t, filepath.Join(f.worker, "result.txt"), []byte("reviewed result\n"))
	f.git(f.worker, "add", "result.txt")
	f.git(f.worker, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Original reviewed result")
	candidateHead := f.git(f.worker, "rev-parse", "HEAD")
	// The review owner can commit reviewed files after the model's original HEAD.
	stoppedTestBytes(t, filepath.Join(f.worker, "result.txt"), []byte("reviewed owner commit\n"))
	logical := filepath.Join(f.root, ".worktrees", name)
	admin := f.git(f.worker, "rev-parse", "--absolute-git-dir")
	stoppedTestPut(t, filepath.Join(f.storage, "worktree-bindings", hostDigest([]byte(logical))+".json"), map[string]string{"original": logical, "physical": f.worker, "git_admin": admin})
	configRef := stoppedTestPut(t, filepath.Join(f.admission, "placeholder.json"), map[string]string{"placeholder": "not config"})
	_ = configRef
	stoppedTestBytes(t, filepath.Join(f.admission, "noodle.toml"), cfg)
	configRef, _ = loopruntime.ReadSessionReadbackFile(filepath.Join(f.admission, "noodle.toml"), nil)
	profile := stoppedTestPut(t, filepath.Join(f.admission, "profile.json"), map[string]string{"fixture": "original profile"})
	envelope := stoppedTestPut(t, filepath.Join(f.admission, "task-envelope.json"), map[string]any{"schema": 2, "control_root": f.root, "base_head": head, "profile": profile, "order_id": "original"})
	session := filepath.Join(f.storage, ".noodle", "sessions", "session")
	sources := map[string]stoppedMergeRef{}
	for _, name := range []string{"spawn.json", "openshell-dispatch.json", "delivery/result.json", "delivery/publish.json", "delivery/changes.patch"} {
		sources[name] = stoppedTestPut(t, filepath.Join(session, name), map[string]string{"fixture": name})
	}
	execution := loopruntime.CompletedSessionReadback{CandidateHead: candidateHead, CommandSHA256: strings.Repeat("c", 64), NativeAttemptID: "native-attempt", MainProcessInstanceID: "original-process", NativePhase: "Stopped", StopAcknowledged: true, Sources: sources}
	resultRef, _ := loopruntime.ReadSessionReadbackFile(filepath.Join(f.worker, "result.txt"), nil)
	c := stoppedMergeCandidate{Schema: 1, TaskEnvelope: envelope, OrderID: "original", SessionID: "session", AttemptID: "attempt", Worktree: logical, Head: candidateHead, SpawnSHA: sources["spawn.json"].SHA256, Native: execution}
	c.Files = map[string]*struct {
		SHA        string `json:"sha256"`
		Executable bool   `json:"executable"`
	}{"result.txt": {SHA: resultRef.SHA256}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	f.candidate = stoppedTestPut(t, filepath.Join(f.admission, "candidates", hostDigest(b)+".json"), c)
	// The real candidate producer addresses the final serialized bytes.
	if filepath.Base(f.candidate.Path) != f.candidate.SHA256+".json" {
		target := filepath.Join(filepath.Dir(f.candidate.Path), f.candidate.SHA256+".json")
		if err = os.Rename(f.candidate.Path, target); err != nil {
			t.Fatal(err)
		}
		f.candidate.Path = target
	}
	f.git(f.worker, "add", "result.txt")
	f.git(f.worker, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Apply original reviewed local task")
	boundHead := f.git(f.worker, "rev-parse", "HEAD")
	tree := f.git(f.worker, "rev-parse", "HEAD^{tree}")
	extra, _ := json.Marshal(map[string]any{"candidate": f.candidate, "head": boundHead, "tree": tree})
	stage := state.StageNode{StageIndex: 0, TaskKey: "local-task", Skill: "local-task", Runtime: "openshell", Provider: "codex", Model: "fixture", Status: state.StageReview, Extra: map[string]json.RawMessage{"local_task_merge": extra}, Attempts: []state.AttemptNode{{AttemptID: "attempt", SessionID: "session", Status: state.AttemptCompleted, WorktreeName: name}}}
	reason := "merge failed: merge " + name + ": root checkout has uncommitted changes — commit or stash before merging"
	s := reducer.DurableSnapshot{OrderRevision: strings.Repeat("b", 32), State: state.State{SchemaVersion: statever.Current, Mode: state.RunModeManual, LastEventID: "10", Orders: map[string]state.OrderNode{"original": {OrderID: "original", Status: state.OrderActive, Stages: []state.StageNode{stage}}}, PendingReviews: map[string]state.PendingReviewNode{"original": {OrderID: "original", StageIndex: 0, SessionID: "session", WorktreeName: name, WorktreePath: logical, Reason: reason}}}, GeneratedAt: time.Now().UTC()}
	payload, _ := json.Marshal(map[string]any{"order_id": "original", "stage_index": 0, "worktree_name": name, "merge_mode": "local", "merge_branch": ""})
	s.EffectLedger = []reducer.EffectLedgerRecord{{EffectID: "event-8-effect-0", Effect: reducer.Effect{EffectID: "event-8-effect-0", Type: reducer.EffectMerge, Payload: payload}, Status: reducer.EffectLedgerPending}}
	snapshotRef := stoppedTestPut(t, filepath.Join(f.storage, ".noodle", "state.snapshot.json"), s)
	stoppedTestPut(t, filepath.Join(f.storage, ".noodle", "integration-branch.json"), map[string]string{"branch": "control"})
	stoppedTestBytes(t, filepath.Join(f.storage, ".noodle", "noodle.lock"), nil)
	stoppedTestBytes(t, filepath.Join(f.storage, ".noodle", "control.ndjson"), nil)
	cmd := loop.ControlCommand{ID: "original-review", Action: "merge-local-task", OrderID: "original", Value: f.candidate.Path, Target: f.candidate.SHA256}
	stoppedTestPut(t, filepath.Join(f.admission, "task-owner.json"), map[string]any{"review": cmd, "review_selections": map[string]any{"review": map[string]any{"candidate": f.candidate, "decision": "approve"}}})
	ack, _ := json.Marshal(loop.ControlAck{ID: cmd.ID, Action: cmd.Action, Status: "ok"})
	stoppedTestBytes(t, filepath.Join(f.storage, ".noodle", "control-ack.ndjson"), append(ack, '\n'))
	stoppedTestBytes(t, filepath.Join(f.root, ".gitignore"), []byte(".noodle/\n.worktrees/\n.worktrees/\n"))
	host, err := os.MkdirTemp("/tmp", "nsm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(host) })
	f.host, _ = filepath.EvalSymlinks(host)
	id := strings.Repeat("a", 32)
	f.generation = filepath.Join(f.host, "g", id)
	if err = os.MkdirAll(f.generation, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(f.host, "original-noodle")
	stoppedTestBytes(t, old, []byte("isolated original executor fixture"))
	oldRef, err := hostReadbackBinaryRef(old)
	if err != nil {
		t.Fatal(err)
	}
	f.receipt = hostReceipt{Schema: 1, Protocol: hostProtocol, ProjectDir: f.root, HostDir: f.host, RequestID: "request", HostPID: 123, HostInstanceID: id, ContextID: "context", ContextState: "active", ConfigPath: configRef.Path, ConfigSHA256: configRef.SHA256, Mode: "manual", BinaryPath: oldRef.Path, BinarySHA256: oldRef.SHA256}
	f.request = filepath.Join(f.admission, "host-request.json")
	stoppedTestPut(t, f.request, hostRequestBinding{Schema: 1, Operation: "attach", ProjectDir: f.root, HostDir: f.host, RequestID: "request", ConfigPath: configRef.Path, ConfigSHA256: configRef.SHA256, Mode: "manual", BinarySHA256: oldRef.SHA256, EnvironmentSHA256: "fixture-environment", Status: "accepted"})
	stoppedTestPut(t, filepath.Join(f.generation, "receipts.json"), []hostReceipt{f.receipt})
	if err = updateHostPool(f.host, func(p *hostPool) error {
		p.Active = id
		p.Generations[id] = hostGeneration{ID: id, Directory: f.generation, Binary: oldRef.Path, SHA: oldRef.SHA256, State: "active"}
		p.Owners["request"] = hostTaskOwner{Original: f.receipt, Current: f.receipt, State: "owned", EnvironmentSHA: "fixture-environment"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	controllerPath, _ := os.Executable()
	controller, err := hostReadbackBinaryRef(controllerPath)
	if err != nil {
		t.Fatal(err)
	}
	allSources := map[string]stoppedMergeRef{}
	for k, v := range sources {
		allSources[k] = v
	}
	allSources["canonical_snapshot"] = snapshotRef
	execution.Sources = allSources
	f.native = hostSessionReadback{SchemaVersion: 2, Status: "observed", HostInstanceID: id, ContextID: "context", RequestID: "request", Observer: &hostSessionObserver{Binary: controller, ExecutionBinary: oldRef}, Sources: map[string]stoppedMergeRef{"profile": profile}, Observation: &loop.SessionReadback{SchemaVersion: 1, Status: "observed", OrderID: "original", StageIndex: 0, AttemptID: "attempt", SessionID: "session", Runtime: "openshell", Provider: "codex", Model: "fixture", AttemptStatus: "completed", WorktreePath: logical, Execution: execution}, Effects: []string{}}
	return f
}
func (f *stoppedMergeFixture) prepare() stoppedMergeResult {
	f.t.Helper()
	r, err := prepareStoppedMerge(f.t.Context(), f.root, f.candidate.Path, func(context.Context, string, string, string, string) (hostSessionReadback, error) {
		return f.native, nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.ref = r.Plan
	return r
}
func (f *stoppedMergeFixture) serve(lost bool) {
	f.t.Helper()
	l, err := net.Listen("unix", filepath.Join(f.generation, "host.sock"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			func() {
				defer c.Close()
				var req hostRequest
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				f.requests <- req.Operation
				if req.Operation == "detach" {
					f.detaches.Add(1)
					r := f.receipt
					r.ContextState = "detached"
					r.TerminalCause = loop.TerminalMigrationDetach
					b, _ := json.Marshal([]hostReceipt{r})
					_ = os.WriteFile(filepath.Join(f.generation, "receipts.json"), b, 0600)
					_ = updateHostPool(f.host, func(p *hostPool) error {
						o := p.Owners["request"]
						o.Current = r
						o.State = "terminal"
						p.Owners["request"] = o
						return nil
					})
					if lost {
						return
					}
				}
				var receipts []hostReceipt
				b, _ := os.ReadFile(filepath.Join(f.generation, "receipts.json"))
				_ = json.Unmarshal(b, &receipts)
				_ = json.NewEncoder(c).Encode(hostResponse{hostReceipt: receipts[0], Status: "ok"})
			}()
		}
	}()
}
func (f *stoppedMergeFixture) detach() {
	f.t.Helper()
	f.serve(false)
	if _, err := stoppedMergeOperation(f.t.Context(), f.root, f.ref.Path, f.ref.SHA256, "detach", nil); err != nil {
		f.t.Fatal(err)
	}
}

func TestStoppedLocalMergeContinuesOriginalEffectWithoutModelOrInstaller(t *testing.T) {
	f := newStoppedMergeFixture(t)
	installer := stoppedMergeInstaller(t)
	r := f.prepare()
	if r.Status != "prepared" || r.Next["operation"] != "detach" {
		t.Fatal(r)
	}
	if r.Head == f.native.Observation.Execution.CandidateHead {
		t.Fatal("fixture lost the original owner staged-commit boundary")
	}
	planBytes, _ := os.ReadFile(f.ref.Path)
	candidateBytes, _ := os.ReadFile(f.candidate.Path)
	if _, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", nil); err == nil {
		t.Fatal("continue accepted active Context")
	}
	f.detach()
	result, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.EffectID != "event-8-effect-0" || result.AuthorizesLanding {
		t.Fatal(result)
	}
	if f.git(f.root, "rev-parse", "HEAD") != result.Head || f.git(f.root, "rev-parse", "HEAD^{tree}") != result.Tree {
		t.Fatal("Git landing differs")
	}
	var s reducer.DurableSnapshot
	b, _ := os.ReadFile(filepath.Join(f.storage, ".noodle", "state.snapshot.json"))
	if json.Unmarshal(b, &s) != nil || s.State.Orders["original"].Status != state.OrderCompleted || s.State.Orders["original"].Stages[0].Attempts[0].Status != state.AttemptCompleted || len(s.EffectLedger) != 1 || s.EffectLedger[0].EffectID != result.EffectID || s.EffectLedger[0].Status != reducer.EffectLedgerDone {
		t.Fatal("original same-effect completion differs", string(b))
	}
	for i := 0; i < 2; i++ {
		if r, err = stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", nil); err != nil || r.Status != "completed" || len(r.Effects) != 0 {
			t.Fatal("completed request replay", r, err)
		}
	}
	if f.detaches.Load() != 1 {
		t.Fatal("detach repeated")
	}
	if _, err = os.Stat(installer); !os.IsNotExist(err) {
		t.Fatal("Git-only continuation invoked dependency installation", err)
	}
	if cached, err := prepareStoppedMerge(t.Context(), f.root, f.candidate.Path, func(context.Context, string, string, string, string) (hostSessionReadback, error) {
		t.Fatal("completed continuation requested a new gateway readback")
		return hostSessionReadback{}, nil
	}); err != nil || cached.Status != "completed" {
		t.Fatal("cached preparation lost original stopped proof", cached, err)
	}
	gotPlan, _ := os.ReadFile(f.ref.Path)
	gotCandidate, _ := os.ReadFile(f.candidate.Path)
	if string(planBytes) != string(gotPlan) || string(candidateBytes) != string(gotCandidate) {
		t.Fatal("Frozen plan/candidate changed")
	}
}

func stoppedMergeInstaller(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	marker := filepath.Join(bin, "installer-called")
	t.Setenv("NOODLE_TEST_INSTALLER_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "pnpm"), []byte("#!/bin/sh\nprintf 'called' > \"$NOODLE_TEST_INSTALLER_MARKER\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestStoppedLocalMergeKeepsPublicMergeInstaller(t *testing.T) {
	f := newStoppedMergeFixture(t)
	installer := stoppedMergeInstaller(t)
	r := f.prepare()
	var p stoppedMergePlan
	if err := stoppedMergeRead(r.Plan, &p); err != nil {
		t.Fatal(err)
	}
	stoppedTestBytes(t, filepath.Join(f.root, ".gitignore"), p.Root.IgnoreHead)
	e, err := execution.New(f.root, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := worktree.PrepareStorageExcludes(t.Context(), e); err != nil || !ok {
		t.Fatal(ok, err)
	}
	app := &worktree.App{Root: f.root, CommonDir: p.Root.Custody.CommonDir, Execution: e, Quiet: true, IntegrationBranch: p.Root.Branch}
	if err := app.Merge(filepath.Base(f.worker), worktree.MergeOpts{ExpectedHead: p.Head, ExpectedTree: p.Tree, RetainWorktree: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installer); err != nil {
		t.Fatal("public Merge lost its existing installer behavior", err)
	}
}

func TestStoppedLocalMergeDetachUnknownReadsStatusOnly(t *testing.T) {
	f := newStoppedMergeFixture(t)
	f.prepare()
	f.serve(true)
	if _, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "detach", nil); err == nil {
		t.Fatal("lost ACK accepted")
	}
	if _, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "detach", nil); err != nil {
		t.Fatal(err)
	}
	if f.detaches.Load() != 1 {
		t.Fatal("unknown detach resent")
	}
	if <-f.requests != "detach" || <-f.requests != "status" {
		t.Fatal("unknown detach did not use original status")
	}
}

func TestStoppedLocalMergeUnknownNeverRemergesAndLandedCrashCompletes(t *testing.T) {
	for _, at := range []string{"after_root_repair", "before_git", "after_git", "before_canonical", "after_canonical"} {
		t.Run(at, func(t *testing.T) {
			f := newStoppedMergeFixture(t)
			f.prepare()
			f.detach()
			boom := errors.New("fixture response lost")
			if _, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", func(s string) error {
				if s == at {
					return boom
				}
				return nil
			}); !errors.Is(err, boom) {
				t.Fatal("missing original interruption", err)
			}
			rootBefore := f.git(f.root, "rev-parse", "HEAD")
			r, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", nil)
			if at == "before_git" {
				if err == nil || r.Status != "unknown" || f.git(f.root, "rev-parse", "HEAD") != rootBefore {
					t.Fatal("unknown Git intent was repeated", r, err)
				}
			} else if err != nil || r.Status != "completed" || (at != "after_root_repair" && f.git(f.root, "rev-parse", "HEAD") != rootBefore) {
				t.Fatal("landed write was not recovered read-only", r, err)
			}
		})
	}
}

func TestStoppedLocalMergeRejectsForeignChangedAndHeldCustody(t *testing.T) {
	for _, fault := range []string{"user-change", "ignore-symlink", "candidate", "profile", "snapshot", "ack", "root-head", "foreign-alias", "held-lock", "pending-input", "wrong-version"} {
		t.Run(fault, func(t *testing.T) {
			f := newStoppedMergeFixture(t)
			f.prepare()
			f.detach()
			head := f.git(f.root, "rev-parse", "HEAD")
			version := f.ref.SHA256
			var lock *lockfile.Lock
			switch fault {
			case "user-change":
				stoppedTestBytes(t, filepath.Join(f.root, "original.txt"), []byte("user modification"))
			case "ignore-symlink":
				_ = os.Remove(filepath.Join(f.root, ".gitignore"))
				_ = os.Symlink(filepath.Join(f.worker, ".gitignore"), filepath.Join(f.root, ".gitignore"))
			case "candidate":
				stoppedTestBytes(t, filepath.Join(f.worker, "result.txt"), []byte("foreign candidate"))
			case "profile":
				stoppedTestBytes(t, f.native.Sources["profile"].Path, []byte("changed"))
			case "snapshot":
				stoppedTestBytes(t, filepath.Join(f.storage, ".noodle", "state.snapshot.json"), []byte("{}"))
			case "ack":
				stoppedTestBytes(t, filepath.Join(f.storage, ".noodle", "control-ack.ndjson"), nil)
			case "root-head":
				f.git(f.root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "Foreign root head")
				head = f.git(f.root, "rev-parse", "HEAD")
			case "foreign-alias":
				_ = os.Remove(filepath.Join(f.root, ".worktrees"))
				_ = os.Symlink(t.TempDir(), filepath.Join(f.root, ".worktrees"))
			case "held-lock":
				var err error
				lock, err = lockfile.TryLock(filepath.Join(f.storage, ".noodle", "noodle.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "pending-input":
				stoppedTestBytes(t, filepath.Join(f.storage, ".noodle", "control.ndjson"), []byte("foreign input"))
			case "wrong-version":
				version = strings.Repeat("f", 64)
			}
			if _, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, version, "continue", nil); err == nil {
				t.Fatal("foreign custody accepted", fault)
			}
			if f.git(f.root, "rev-parse", "HEAD") != head {
				t.Fatal("refusal mutated Git")
			}
		})
	}
}

func TestStoppedMergeVisibleArchiveRequiresDurableReadback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	raw := []byte("{\"original\":true}\n")
	stoppedTestBytes(t, path, raw)
	called := false
	boom := errors.New("fixture directory sync refused")
	_, err := stoppedMergeArchiveUsing(path, raw, func(string) error { called = true; return boom })
	if !called || !errors.Is(err, boom) {
		t.Fatalf("visible equality promoted an unconfirmed archive: called=%v err=%v", called, err)
	}
}

func TestStoppedLocalMergeOwnedIgnoreRepairKeepsModeAcrossInterruptedPublication(t *testing.T) {
	f := newStoppedMergeFixture(t)
	f.prepare()
	f.detach()
	path := filepath.Join(f.root, ".gitignore")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("fixture lost original mode", err)
	}
	boom := errors.New("fixture interrupted before repair durability readback")
	for i := 0; i < 2; i++ {
		_, err = stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", func(phase string) error {
			if phase == "after_ignore_repair" {
				current, statErr := os.Stat(path)
				if statErr != nil || current.Mode().Perm() != info.Mode().Perm() || !os.SameFile(info, current) {
					return errors.New("owned suffix repair changed original inode/mode before durability readback")
				}
				return boom
			}
			return nil
		})
		if !errors.Is(err, boom) {
			t.Fatal("known repair interruption cannot resume unchanged", err)
		}
	}
	if r, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", nil); err != nil || r.Status != "completed" {
		t.Fatal("same repair intent could not finish", r, err)
	}
}

func TestStoppedLocalMergeVisibleCanonicalAfterDoesNotSkipDurabilityReadback(t *testing.T) {
	f := newStoppedMergeFixture(t)
	f.prepare()
	f.detach()
	boom := errors.New("fixture original write response lost")
	if _, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", func(phase string) error {
		if phase == "after_canonical" {
			return boom
		}
		return nil
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	head := f.git(f.root, "rev-parse", "HEAD")
	for i := 0; i < 2; i++ {
		r, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", func(phase string) error {
			if phase == "before_canonical_readback_sync" {
				return boom
			}
			return nil
		})
		if !errors.Is(err, boom) || r.Status == "completed" || f.git(f.root, "rev-parse", "HEAD") != head {
			t.Fatal("visible canonical after-image promoted past failed durability boundary", r, err)
		}
	}
	if r, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", nil); err != nil || r.Status != "completed" || f.git(f.root, "rev-parse", "HEAD") != head {
		t.Fatal("same landed effect failed readback recovery", r, err)
	}
}

func TestStoppedLocalMergeCommandsKeepReadOnlyInspectionAndExplicitEffectVersion(t *testing.T) {
	for _, operation := range []string{"prepare", "inspect", "detach", "continue"} {
		root := newReviewCmd(&App{projectDir: t.TempDir()})
		name := operation + "-stopped-local-merge"
		command, _, err := root.Find([]string{name})
		if err != nil || command.Name() != name {
			t.Fatal("missing original review-owner command", name, err)
		}
		effect := operation == "detach" || operation == "continue"
		if (command.Flags().Lookup("expected-version") != nil) != effect {
			t.Fatal("read/effect version boundary differs", name)
		}
		if effect {
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{name, "/not-read"})
			if err = root.Execute(); err == nil || !strings.Contains(err.Error(), "expected-version") {
				t.Fatal("effect path read input before requiring explicit version", name, err)
			}
		}
	}
}

func TestStoppedLocalMergeForeignControllerHelper(t *testing.T) {
	plan := os.Getenv("NOODLE_TEST_STOPPED_FOREIGN_PLAN")
	if plan == "" {
		return
	}
	r, err := stoppedMergeOperation(t.Context(), os.Getenv("NOODLE_TEST_STOPPED_FOREIGN_PROJECT"), plan, os.Getenv("NOODLE_TEST_STOPPED_FOREIGN_VERSION"), os.Getenv("NOODLE_TEST_STOPPED_FOREIGN_OPERATION"), nil)
	message := ""
	if err != nil {
		message = err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"result": r, "error": message}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestStoppedLocalMergeRejectsActualForeignControllerBeforeAnyEffect(t *testing.T) {
	original, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign-controller")
	source, err := os.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.OpenFile(foreign, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(target, source)
	if err = errors.Join(copyErr, target.Close(), source.Close()); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"inspect", "detach", "continue"} {
		t.Run(operation, func(t *testing.T) {
			f := newStoppedMergeFixture(t)
			f.prepare()
			if operation == "continue" {
				f.detach()
			} else if operation == "detach" {
				f.serve(false)
			}
			head := f.git(f.root, "rev-parse", "HEAD")
			snapshot, _ := os.ReadFile(filepath.Join(f.storage, ".noodle", "state.snapshot.json"))
			planBytes, _ := os.ReadFile(f.ref.Path)
			detaches := f.detaches.Load()
			command := exec.Command(foreign, "-test.run=^TestStoppedLocalMergeForeignControllerHelper$")
			command.Env = append(os.Environ(), "NOODLE_TEST_STOPPED_FOREIGN_PLAN="+f.ref.Path, "NOODLE_TEST_STOPPED_FOREIGN_PROJECT="+f.root, "NOODLE_TEST_STOPPED_FOREIGN_VERSION="+f.ref.SHA256, "NOODLE_TEST_STOPPED_FOREIGN_OPERATION="+operation)
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("foreign helper failed before product readback: %v %s", err, out)
			}
			var observed struct {
				Result stoppedMergeResult `json:"result"`
				Error  string             `json:"error"`
			}
			if err = json.Unmarshal(out, &observed); err != nil || !strings.Contains(observed.Error, "actual stopped controller differs") || observed.Result.Status != "refused" || len(observed.Result.Effects) != 0 {
				t.Fatal("foreign executable borrowed the selected controller identity", observed.Error, observed.Result.Status, err)
			}
			current, _ := os.ReadFile(filepath.Join(f.storage, ".noodle", "state.snapshot.json"))
			currentPlan, _ := os.ReadFile(f.ref.Path)
			if f.git(f.root, "rev-parse", "HEAD") != head || f.detaches.Load() != detaches || !bytes.Equal(current, snapshot) || !bytes.Equal(currentPlan, planBytes) {
				t.Fatal("foreign controller changed original custody or effect")
			}
		})
	}
}

func TestStoppedLocalMergeRejectsUnqualifiedPrepareWithoutPublication(t *testing.T) {
	for _, fault := range []string{"native-stop", "native-ack", "native-effect", "native-attempt", "native-executor", "approval", "failed-ack", "ambiguous-ack", "duplicate-effect", "started-effect", "trailing-snapshot", "foreign-review"} {
		t.Run(fault, func(t *testing.T) {
			f := newStoppedMergeFixture(t)
			snapshot := filepath.Join(f.storage, ".noodle", "state.snapshot.json")
			raw, _ := os.ReadFile(snapshot)
			var s reducer.DurableSnapshot
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "native-stop":
				f.native.Observation.Execution.NativePhase = "Ready"
			case "native-ack":
				f.native.Observation.Execution.StopAcknowledged = false
			case "native-effect":
				f.native.Effects = []string{"unknown"}
			case "native-attempt":
				f.native.Observation.Execution.NativeAttemptID = "foreign"
			case "native-executor":
				f.native.Observer.ExecutionBinary.SHA256 = strings.Repeat("0", 64)
			case "approval":
				stoppedTestPut(t, filepath.Join(f.admission, "task-owner.json"), map[string]any{"review_selections": map[string]any{}})
			case "failed-ack":
				ack, _ := json.Marshal(loop.ControlAck{ID: "original-review", Action: "merge-local-task", Status: "error"})
				stoppedTestBytes(t, filepath.Join(f.storage, ".noodle", "control-ack.ndjson"), append(ack, '\n'))
			case "ambiguous-ack":
				ackPath := filepath.Join(f.storage, ".noodle", "control-ack.ndjson")
				ack, _ := os.ReadFile(ackPath)
				stoppedTestBytes(t, ackPath, append(append([]byte{}, ack...), ack...))
			case "duplicate-effect":
				s.EffectLedger = append(s.EffectLedger, s.EffectLedger[0])
			case "started-effect":
				s.EffectLedger[0].Attempts = 1
			case "foreign-review":
				r := s.State.PendingReviews["original"]
				r.SessionID = "foreign"
				s.State.PendingReviews["original"] = r
			}
			if fault == "duplicate-effect" || fault == "started-effect" || fault == "foreign-review" {
				f.native.Observation.Execution.Sources["canonical_snapshot"] = stoppedTestPut(t, snapshot, s)
			}
			if fault == "trailing-snapshot" {
				stoppedTestBytes(t, snapshot, append(raw, []byte("{}")...))
				f.native.Observation.Execution.Sources["canonical_snapshot"], _ = loopruntime.ReadSessionReadbackFile(snapshot, nil)
			}
			head := f.git(f.root, "rev-parse", "HEAD")
			if _, err := prepareStoppedMerge(t.Context(), f.root, f.candidate.Path, func(context.Context, string, string, string, string) (hostSessionReadback, error) {
				return f.native, nil
			}); err == nil {
				t.Fatal("unqualified preparation accepted", fault)
			}
			if _, err := os.Stat(filepath.Join(f.admission, "stopped-local-merge")); !os.IsNotExist(err) {
				t.Fatal("refused prepare published a plan", err)
			}
			if f.git(f.root, "rev-parse", "HEAD") != head || f.detaches.Load() != 0 {
				t.Fatal("refusal changed original effect")
			}
		})
	}
}

func TestStoppedLocalMergePreservesOpaqueHistoryAndReadOnlyReceipt(t *testing.T) {
	f := newStoppedMergeFixture(t)
	path := filepath.Join(f.storage, ".noodle", "state.snapshot.json")
	var s reducer.DurableSnapshot
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	order := s.State.Orders["original"]
	order.Stages[0].Extra["opaque_owner_history"] = json.RawMessage(`{"huge":900719925474099312345,"lexical":1e+20,"status":"unknown"}`)
	s.State.Orders["original"] = order
	f.native.Observation.Execution.Sources["canonical_snapshot"] = stoppedTestPut(t, path, s)
	f.prepare()
	f.detach()
	result, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, f.ref.SHA256, "continue", nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Contains(after, []byte("900719925474099312345")) || !bytes.Contains(after, []byte("1e+20")) || !bytes.Contains(after, []byte(`"unknown"`)) {
		t.Fatal("opaque original history lost", string(after))
	}
	receipt := result.Sources["receipt"].Path
	before, _ := os.ReadFile(receipt)
	info, _ := os.Stat(receipt)
	if r, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, "", "inspect", nil); err != nil || r.Status != "completed" || len(r.Effects) != 0 {
		t.Fatal(r, err)
	}
	got, _ := os.ReadFile(receipt)
	infoAfter, _ := os.Stat(receipt)
	if !bytes.Equal(before, got) || !os.SameFile(info, infoAfter) || !info.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatal("read-only inspect rewrote original receipt")
	}
	stoppedTestBytes(t, receipt, []byte("{}\n"))
	if _, err := stoppedMergeOperation(t.Context(), f.root, f.ref.Path, "", "inspect", nil); err == nil {
		t.Fatal("foreign receipt silently adopted")
	}
}
