package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/poteto/noodle/cmdmeta"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/spf13/cobra"
)

type stoppedMergeRef = loopruntime.SessionReadbackFileRef
type stoppedMergeCandidate struct {
	Schema       int                                  `json:"schema"`
	TaskEnvelope stoppedMergeRef                      `json:"task_envelope"`
	OrderID      string                               `json:"order_id"`
	SessionID    string                               `json:"session_id"`
	AttemptID    string                               `json:"attempt_id"`
	Worktree     string                               `json:"worktree"`
	Head         string                               `json:"head"`
	SpawnSHA     string                               `json:"spawn_sha256"`
	Native       loopruntime.CompletedSessionReadback `json:"native_execution"`
	Files        map[string]*struct {
		SHA        string `json:"sha256"`
		Executable bool   `json:"executable"`
	} `json:"files"`
}
type stoppedMergeRoot struct {
	Head         string                     `json:"head"`
	Branch       string                     `json:"branch"`
	IndexSHA     string                     `json:"index_sha256"`
	IgnoreHead   []byte                     `json:"ignore_head"`
	IgnoreBefore []byte                     `json:"ignore_before"`
	IgnoreMode   uint32                     `json:"ignore_mode"`
	Custody      worktree.Custody           `json:"custody"`
	Sources      map[string]stoppedMergeRef `json:"sources"`
}
type stoppedMergePlan struct {
	Schema       int                        `json:"schema"`
	Project      string                     `json:"project"`
	Controller   stoppedMergeRef            `json:"controller"`
	Candidate    stoppedMergeRef            `json:"candidate"`
	Host         hostOwnerBinding           `json:"original_host"`
	Native       stoppedMergeRef            `json:"native_completion"`
	Snapshot     []byte                     `json:"canonical_before"`
	ACK          []byte                     `json:"control_ack_before"`
	ReviewOwner  []byte                     `json:"review_owner_before"`
	Stable       map[string]stoppedMergeRef `json:"stable_sources"`
	Root         stoppedMergeRoot           `json:"root_before"`
	Head         string                     `json:"head"`
	Tree         string                     `json:"tree"`
	EffectID     string                     `json:"effect_id"`
	DetachImpact string                     `json:"detach_impact"`
}
type stoppedMergeResult struct {
	Schema            int                        `json:"schema"`
	Owner             string                     `json:"owner"`
	Status            string                     `json:"status"`
	Error             string                     `json:"error,omitempty"`
	Plan              stoppedMergeRef            `json:"plan"`
	Candidate         stoppedMergeRef            `json:"candidate"`
	EffectID          string                     `json:"effect_id"`
	Head              string                     `json:"head"`
	Tree              string                     `json:"tree"`
	Sources           map[string]stoppedMergeRef `json:"sources"`
	Next              map[string]any             `json:"next"`
	Effects           []string                   `json:"effects"`
	AuthorizesLanding bool                       `json:"authorizes_landing"`
	Executor          stoppedMergeRef            `json:"executor"`
	Controller        stoppedMergeRef            `json:"controller"`
}

func newStoppedLocalMergeCmd(app *App, operation string) *cobra.Command {
	var expected string
	cmd := &cobra.Command{Use: operation + "-stopped-local-merge FILE", Short: cmdmeta.Short("review", operation+"-stopped-local-merge"), Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var r stoppedMergeResult
		var err error
		if operation == "prepare" {
			r, err = prepareStoppedMerge(cmd.Context(), app.projectDir, args[0], callHostSessionReadback)
		} else {
			r, err = stoppedMergeOperation(cmd.Context(), app.projectDir, args[0], expected, operation, nil)
		}
		if err != nil {
			r.Error = err.Error()
			if r.Status == "" {
				r.Status = "refused"
			}
		}
		return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(r))
	}}
	if operation == "detach" || operation == "continue" {
		cmd.Flags().StringVar(&expected, "expected-version", "", "Exact plan SHA returned by original inspection")
		_ = cmd.MarkFlagRequired("expected-version")
	}
	return cmd
}

func stoppedMergeRead(ref stoppedMergeRef, value any) error {
	got, err := hostReadbackBinaryRef(ref.Path)
	if err != nil || got != ref {
		return fmt.Errorf("stopped merge source changed: %s", ref.Path)
	}
	if value != nil {
		actual, err := loopruntime.ReadSessionReadbackFile(ref.Path, value)
		if err != nil || actual != ref {
			return fmt.Errorf("stopped merge JSON changed: %s", ref.Path)
		}
	}
	return nil
}
func stoppedMergeSave(path string, value any) (stoppedMergeRef, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return stoppedMergeRef{}, err
	}
	raw = append(raw, '\n')
	return stoppedMergeArchive(path, raw)
}
func stoppedMergeArchive(path string, raw []byte) (stoppedMergeRef, error) {
	return stoppedMergeArchiveUsing(path, raw, syncStoppedMergeFile)
}
func stoppedMergeArchiveUsing(path string, raw []byte, sync func(string) error) (stoppedMergeRef, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return stoppedMergeRef{}, fmt.Errorf("stopped merge evidence is not regular: %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return stoppedMergeRef{}, err
	}
	if prior, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Equal(prior, raw) {
			return stoppedMergeRef{}, fmt.Errorf("original stopped merge evidence differs: %s", path)
		}
	} else if !os.IsNotExist(readErr) {
		return stoppedMergeRef{}, readErr
	} else if err := filex.WriteFileAtomicDurable(path, raw); err != nil {
		return stoppedMergeRef{}, err
	}
	if err := sync(path); err != nil {
		return stoppedMergeRef{}, err
	}
	return loopruntime.ReadSessionReadbackFile(path, nil)
}
func syncStoppedMergeFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return fmt.Errorf("stopped evidence is not a regular file")
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return filex.SyncDir(filepath.Dir(path))
}
func stoppedMergeGit(ctx context.Context, e *execution.Context, root string, args ...string) ([]byte, error) {
	return e.Command(ctx, root, "git", args...).Output()
}
func stoppedMergeText(ctx context.Context, e *execution.Context, root string, args ...string) (string, error) {
	raw, err := stoppedMergeGit(ctx, e, root, args...)
	return strings.TrimSpace(string(raw)), err
}
func stoppedMergeRefusal() stoppedMergeResult {
	return stoppedMergeResult{Schema: 1, Owner: "Noodle stopped local merge continuation", Status: "refused", Effects: []string{}, Sources: map[string]stoppedMergeRef{}}
}
func stoppedMergeResultFor(p stoppedMergePlan, ref stoppedMergeRef, status, next string) stoppedMergeResult {
	r := stoppedMergeRefusal()
	r.Status, r.Plan, r.Candidate, r.EffectID, r.Head, r.Tree = status, ref, p.Candidate, p.EffectID, p.Head, p.Tree
	r.Executor, r.Controller = p.Host.ExecutionBinary, p.Controller
	r.Sources["native_completion"], r.Sources["task_envelope"] = p.Native, p.Stable["task_envelope"]
	r.Next = map[string]any{"owner": r.Owner, "operation": next}
	r.Next["readback_argv"] = []string{p.Controller.Path, "--project-dir", p.Project, "review", "inspect-stopped-local-merge", ref.Path}
	if next != "original_integrated_acceptance" && next != "unknown_original_merge_readback" {
		r.Next["argv"] = []string{p.Controller.Path, "--project-dir", p.Project, "review", next + "-stopped-local-merge", ref.Path, "--expected-version", ref.SHA256}
	}
	return r
}

func prepareStoppedMerge(ctx context.Context, project, candidatePath string, observe func(context.Context, string, string, string, string) (hostSessionReadback, error)) (r stoppedMergeResult, err error) {
	r = stoppedMergeRefusal()
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return r, err
	}
	var c stoppedMergeCandidate
	candidateRef, err := loopruntime.ReadSessionReadbackFile(candidatePath, &c)
	if err != nil {
		return r, err
	}
	if c.Schema != 1 || c.OrderID == "" || c.AttemptID == "" || c.SessionID == "" || filepath.Base(c.SessionID) != c.SessionID || len(c.Files) == 0 {
		return r, fmt.Errorf("original candidate identity is incomplete")
	}
	directory := filepath.Dir(c.TaskEnvelope.Path)
	if candidateRef.Path != filepath.Join(directory, "candidates", candidateRef.SHA256+".json") {
		return r, fmt.Errorf("original immutable candidate location differs")
	}
	planDir := filepath.Join(directory, "stopped-local-merge", candidateRef.SHA256)
	planPath := filepath.Join(planDir, "plan.json")
	if _, statErr := os.Stat(planPath); statErr == nil {
		return stoppedMergeOperation(ctx, project, planPath, "", "inspect", nil)
	} else if !os.IsNotExist(statErr) {
		return r, statErr
	}
	owner, err := readHostOwnerBinding(filepath.Join(directory, "host-request.json"))
	if err != nil {
		return r, err
	}
	if owner.Current.ProjectDir != project || owner.Current.ContextState != "active" {
		return r, fmt.Errorf("original active Context differs")
	}
	exe, err := os.Executable()
	if err != nil {
		return r, err
	}
	controller, err := hostReadbackBinaryRef(exe)
	if err != nil {
		return r, err
	}
	if controller == owner.ExecutionBinary {
		return r, fmt.Errorf("stopped controller must be separate from original executor")
	}
	native, err := observe(ctx, project, owner.Request.Path, c.OrderID, c.SessionID)
	if err != nil {
		return r, err
	}
	p := stoppedMergePlan{Schema: 1, Project: project, Controller: controller, Candidate: candidateRef, Host: *owner, Stable: map[string]stoppedMergeRef{"candidate": candidateRef, "task_envelope": c.TaskEnvelope, "original_request": owner.Request, "executor": owner.ExecutionBinary, "controller": controller}, DetachImpact: "Only the original Context is released. The final local gateway consumer may drain and normally stop its resident; no VM/model/Source/A-B retry, DB/TLS/profile migration or Host upgrade."}
	p.Snapshot, err = os.ReadFile(filepath.Join(project, ".noodle", "state.snapshot.json"))
	if err != nil {
		return r, err
	}
	p.ACK, err = os.ReadFile(filepath.Join(project, ".noodle", "control-ack.ndjson"))
	if err != nil {
		return r, err
	}
	p.ReviewOwner, err = os.ReadFile(filepath.Join(directory, "task-owner.json"))
	if err != nil {
		return r, err
	}
	p.Stable["config"] = stoppedMergeRef{Path: owner.Current.ConfigPath, SHA256: owner.Current.ConfigSHA256}
	if native.SchemaVersion != 2 || native.Status != "observed" || native.Observation == nil || native.Observer == nil || native.Observer.ExecutionBinary != owner.ExecutionBinary || native.Observer.Binary != controller || native.HostInstanceID != owner.Current.HostInstanceID || native.ContextID != owner.Current.ContextID || native.RequestID != owner.Current.RequestID || len(native.Effects) != 0 {
		return r, fmt.Errorf("fresh original native completion/frame differs")
	}
	read := native.Observation
	if !reflect.DeepEqual(c.Native.Instructions, read.Execution.Instructions) {
		return r, fmt.Errorf("original prepared instruction binding differs")
	}
	if read.OrderID != c.OrderID || read.SessionID != c.SessionID || read.AttemptID != c.AttemptID || read.WorktreePath != c.Worktree || read.Runtime != "openshell" || read.AttemptStatus != "completed" || read.Status != "observed" || read.Execution.NativeAttemptID == "" || read.Execution.MainProcessInstanceID == "" || len(read.Execution.CommandSHA256) != 64 || read.Execution.NativePhase != "Stopped" || !read.Execution.StopAcknowledged || read.Execution.NativeExecExitCode != 0 || read.Execution.CandidateHead != c.Head || c.Native.NativeAttemptID != read.Execution.NativeAttemptID || c.Native.MainProcessInstanceID != read.Execution.MainProcessInstanceID || c.Native.CommandSHA256 != read.Execution.CommandSHA256 || c.Native.NativePhase != "Stopped" || !c.Native.StopAcknowledged || c.Native.NativeExecExitCode != 0 {
		return r, fmt.Errorf("original completed execution differs from reviewed candidate")
	}
	if read.Execution.Sources["canonical_snapshot"].SHA256 != hostDigest(p.Snapshot) {
		return r, fmt.Errorf("native completion and prepared canonical snapshot differ")
	}
	for name, ref := range read.Execution.Sources {
		if name == "meta.json" || name == "native_custody" || name == "canonical_snapshot" {
			continue
		}
		if c.Native.Sources[name] != ref {
			return r, fmt.Errorf("reviewed stable execution source differs: %s", name)
		}
		p.Stable[name] = ref
	}
	if len(p.Stable) < 10 || read.Execution.Sources["spawn.json"].SHA256 != c.SpawnSHA {
		return r, fmt.Errorf("original execution sources incomplete")
	}
	if ref, ok := native.Sources["profile"]; ok {
		p.Stable["profile"] = ref
	} else {
		return r, fmt.Errorf("original profile source missing")
	}
	if err = stoppedMergeValidateCustody(&p, c); err != nil {
		return r, err
	}
	e, err := execution.New(project, os.Environ())
	if err != nil {
		return r, err
	}
	if err = validateHostEnvironment(e); err != nil {
		return r, err
	}
	p.Root, err = captureStoppedMergeRoot(ctx, e, c)
	if err != nil {
		return r, err
	}
	if err = stoppedMergeStable(p); err != nil {
		return r, err
	}
	if err = os.MkdirAll(planDir, 0700); err != nil {
		return r, err
	}
	canonicalDir, dirErr := filepath.EvalSymlinks(planDir)
	if dirErr != nil || canonicalDir != planDir {
		return r, fmt.Errorf("stopped merge artifact directory changed")
	}
	if _, err = stoppedMergeArchive(filepath.Join(planDir, "before.json"), p.Snapshot); err != nil {
		return r, err
	}
	p.Native, err = stoppedMergeSave(filepath.Join(planDir, "native-before.json"), native)
	if err != nil {
		return r, err
	}
	ref, err := stoppedMergeSave(planPath, p)
	if err != nil {
		return r, err
	}
	for _, directory := range []string{planDir, filepath.Dir(planDir), directory} {
		if err = filex.SyncDir(directory); err != nil {
			return r, err
		}
	}
	return stoppedMergeResultFor(p, ref, "prepared", "detach"), nil
}

func stoppedMergeStable(p stoppedMergePlan) error {
	refs := map[string]stoppedMergeRef{}
	for name, ref := range p.Stable {
		if name == "executor" || name == "controller" {
			if err := stoppedMergeRead(ref, nil); err != nil {
				return err
			}
			continue
		}
		refs[name] = ref
	}
	return loopruntime.VerifySessionReadbackFiles(refs)
}

func stoppedMergeValidateCustody(p *stoppedMergePlan, c stoppedMergeCandidate) error {
	var s reducer.DurableSnapshot
	decoder := json.NewDecoder(bytes.NewReader(p.Snapshot))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("original canonical snapshot has trailing data")
	}
	if !orderx.ValidOrderRevision(s.OrderRevision) || s.State.SchemaVersion != statever.Current || s.GeneratedAt.IsZero() || s.EffectLedger == nil {
		return fmt.Errorf("original canonical snapshot incomplete")
	}
	if len(s.State.Orders) != 1 || len(s.State.PendingReviews) != 1 {
		return fmt.Errorf("stopped merge has foreign order/review custody")
	}
	o, ok := s.State.Orders[c.OrderID]
	if !ok || o.Status != state.OrderActive || len(o.Stages) != 1 {
		return fmt.Errorf("original active order differs")
	}
	stage := o.Stages[0]
	review, ok := s.State.PendingReviews[c.OrderID]
	reason := "merge failed: merge " + filepath.Base(c.Worktree) + ": root checkout has uncommitted changes — commit or stash before merging"
	if !ok || stage.StageIndex != 0 || stage.Status != state.StageReview || stage.Merge != nil || stage.Runtime != "openshell" || len(stage.Attempts) != 1 || review.Reason != reason || review.OrderID != c.OrderID || review.StageIndex != 0 || review.SessionID != c.SessionID || review.WorktreePath != c.Worktree || c.Worktree != filepath.Join(p.Project, ".worktrees", review.WorktreeName) {
		return fmt.Errorf("original root-preflight failed park differs")
	}
	a := stage.Attempts[0]
	if a.Status != state.AttemptCompleted || a.AttemptID != c.AttemptID || a.SessionID != c.SessionID || a.WorktreeName != review.WorktreeName {
		return fmt.Errorf("original completed attempt differs")
	}
	var binding struct {
		Candidate stoppedMergeRef `json:"candidate"`
		Head      string          `json:"head"`
		Tree      string          `json:"tree"`
	}
	if json.Unmarshal(stage.Extra["local_task_merge"], &binding) != nil || binding.Candidate != p.Candidate || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(binding.Head) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(binding.Tree) {
		return fmt.Errorf("original reviewed merge binding differs")
	}
	p.Head, p.Tree = binding.Head, binding.Tree
	var owner struct {
		Reviews map[string]struct {
			Candidate stoppedMergeRef `json:"candidate"`
			Decision  string          `json:"decision"`
		} `json:"review_selections"`
	}
	if json.Unmarshal(p.ReviewOwner, &owner) != nil {
		return fmt.Errorf("original review Owner invalid")
	}
	var commands map[string]json.RawMessage
	_ = json.Unmarshal(p.ReviewOwner, &commands)
	count := 0
	for name, selected := range owner.Reviews {
		if selected.Candidate != p.Candidate || selected.Decision != "approve" {
			continue
		}
		var cmd loop.ControlCommand
		if json.Unmarshal(commands[name], &cmd) != nil || cmd.Action != "merge-local-task" || cmd.OrderID != c.OrderID || cmd.Value != p.Candidate.Path || cmd.Target != p.Candidate.SHA256 {
			return fmt.Errorf("original merge control differs")
		}
		acks := 0
		for _, line := range bytes.Split(p.ACK, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var ack loop.ControlAck
			if json.Unmarshal(line, &ack) != nil {
				return fmt.Errorf("invalid original ACK history")
			}
			if ack.ID == cmd.ID {
				if ack.Status != "ok" || ack.Action != cmd.Action {
					return fmt.Errorf("original merge ACK unresolved")
				}
				acks++
			}
		}
		if acks != 1 {
			return fmt.Errorf("original merge ACK missing/ambiguous")
		}
		count++
	}
	if count != 1 {
		return fmt.Errorf("original approval missing/ambiguous")
	}
	count = 0
	for _, row := range s.EffectLedger {
		if row.Status == reducer.EffectLedgerDone || row.Status == reducer.EffectLedgerFailed {
			continue
		}
		var effect struct {
			Order    string `json:"order_id"`
			Stage    int    `json:"stage_index"`
			Worktree string `json:"worktree_name"`
			Mode     string `json:"merge_mode"`
			Branch   string `json:"merge_branch"`
			Attempt  string `json:"attempt_id"`
		}
		if row.EffectID == "" || row.EffectID != row.Effect.EffectID || row.Status != reducer.EffectLedgerPending || row.Result != nil || json.Unmarshal(row.Effect.Payload, &effect) != nil || effect.Order != c.OrderID || effect.Stage != 0 {
			return fmt.Errorf("foreign/active unresolved effect")
		}
		switch row.Effect.Type {
		case reducer.EffectMerge:
			if effect.Worktree != review.WorktreeName || effect.Mode != "local" || effect.Branch != "" || row.Attempts != 0 || !row.LastAttemptAt.IsZero() {
				return fmt.Errorf("original merge effect tuple differs")
			}
			p.EffectID = row.EffectID
			count++
		case reducer.EffectDispatch:
			if effect.Attempt != c.AttemptID {
				return fmt.Errorf("foreign pending dispatch")
			}
		case reducer.EffectAck:
		default:
			return fmt.Errorf("unsupported pending effect")
		}
	}
	if count != 1 {
		return fmt.Errorf("original merge effect missing/ambiguous")
	}
	return nil
}

func captureStoppedMergeRoot(ctx context.Context, e *execution.Context, c stoppedMergeCandidate) (r stoppedMergeRoot, err error) {
	r.Sources = map[string]stoppedMergeRef{}
	r.Custody, err = worktree.ReadCustody(ctx, e, c.Worktree)
	if err != nil {
		return r, err
	}
	r.Head, err = stoppedMergeText(ctx, e, e.Root(), "rev-parse", "HEAD")
	if err != nil {
		return r, err
	}
	r.Branch, err = stoppedMergeText(ctx, e, e.Root(), "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return r, err
	}
	var envelope struct {
		Root string `json:"control_root"`
		Base string `json:"base_head"`
	}
	if err = stoppedMergeRead(c.TaskEnvelope, &envelope); err != nil {
		return r, err
	}
	if envelope.Root != e.Root() || envelope.Base != r.Head {
		return r, fmt.Errorf("original root advanced before preflight recovery")
	}
	index, err := stoppedMergeGit(ctx, e, e.Root(), "ls-files", "--stage", "-z")
	if err != nil {
		return r, err
	}
	r.IndexSHA = hostDigest(index)
	headIgnore, err := stoppedMergeGit(ctx, e, e.Root(), "show", "HEAD:.gitignore")
	if err != nil {
		return r, err
	}
	indexIgnore, err := stoppedMergeGit(ctx, e, e.Root(), "show", ":.gitignore")
	if err != nil || !bytes.Equal(indexIgnore, headIgnore) {
		return r, fmt.Errorf("original gitignore index differs")
	}
	r.IgnoreHead = headIgnore
	path := filepath.Join(e.Root(), ".gitignore")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return r, fmt.Errorf("original gitignore is not regular")
	}
	r.IgnoreMode = uint32(info.Mode().Perm())
	r.IgnoreBefore, err = os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if !bytes.Equal(r.IgnoreBefore, append(append([]byte{}, headIgnore...), []byte(".worktrees/\n")...)) {
		return r, fmt.Errorf("gitignore change is not the exact original producer append")
	}
	staged, err := stoppedMergeGit(ctx, e, e.Root(), "diff", "--cached", "--name-only")
	if err != nil || len(staged) != 0 {
		return r, fmt.Errorf("original root index contains user changes")
	}
	diff, err := stoppedMergeText(ctx, e, e.Root(), "diff", "--name-only")
	if err != nil || diff != ".gitignore" {
		return r, fmt.Errorf("root contains other tracked changes")
	}
	untracked, err := stoppedMergeGit(ctx, e, e.Root(), "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return r, err
	}
	for _, name := range strings.Split(strings.TrimRight(string(untracked), "\x00"), "\x00") {
		if name != "" && name != ".noodle" && name != ".worktrees" {
			return r, fmt.Errorf("root contains unowned untracked files")
		}
	}
	storage := filepath.Dir(filepath.Dir(r.Custody.Physical))
	sum := hostDigest([]byte(e.Root()))
	for _, path := range []string{filepath.Join(r.Custody.CommonDir, "noodle-contexts", sum, "source.json"), filepath.Join(storage, "source.json")} {
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			return r, err
		}
		r.Sources[path] = ref
	}
	var source worktree.StorageSource
	raw, err := os.ReadFile(filepath.Join(storage, "source.json"))
	if err != nil || json.Unmarshal(raw, &source) != nil || source.Project != e.Root() || source.CommonDir != r.Custody.CommonDir || source.StorageRoot != storage {
		return r, fmt.Errorf("original storage identity differs")
	}
	paired, err := os.ReadFile(filepath.Join(r.Custody.CommonDir, "noodle-contexts", sum, "source.json"))
	if err != nil || !bytes.Equal(raw, paired) {
		return r, fmt.Errorf("original paired storage receipts differ")
	}
	for _, name := range []string{".noodle", ".worktrees"} {
		info, err := os.Lstat(filepath.Join(e.Root(), name))
		target, readErr := filepath.EvalSymlinks(filepath.Join(e.Root(), name))
		if err != nil || readErr != nil || info.Mode()&os.ModeSymlink == 0 || target != filepath.Join(storage, name) {
			return r, fmt.Errorf("original managed alias changed")
		}
	}
	integration := filepath.Join(e.Root(), ".noodle", "integration-branch.json")
	ref, err := loopruntime.ReadSessionReadbackFile(integration, nil)
	if err != nil {
		return r, err
	}
	r.Sources["integration"] = ref
	var branch struct {
		Branch string `json:"branch"`
	}
	if err = stoppedMergeRead(ref, &branch); err != nil || branch.Branch != r.Branch {
		return r, fmt.Errorf("original integration branch differs")
	}
	return r, nil
}

// Keep the normal mutation paths explicit: inspection only reads this state.
type stoppedMergeJournal struct {
	Schema   int             `json:"schema"`
	Plan     stoppedMergeRef `json:"plan"`
	Phase    string          `json:"phase"`
	Detached *hostResponse   `json:"detached,omitempty"`
	After    []byte          `json:"canonical_after,omitempty"`
	At       time.Time       `json:"completed_at,omitempty"`
}
