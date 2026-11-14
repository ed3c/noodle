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
	"strings"
	"time"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

func stoppedMergeOperation(ctx context.Context, project, planPath, expected, operation string, barrier func(string) error) (r stoppedMergeResult, err error) {
	effects := []string{}
	defer func() { r.Effects = effects }()
	r = stoppedMergeRefusal()
	var p stoppedMergePlan
	ref, err := loopruntime.ReadSessionReadbackFile(planPath, &p)
	if err != nil {
		return r, err
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return r, err
	}
	var c stoppedMergeCandidate
	if p.Schema != 1 || p.Project != project || p.Host.Current.ProjectDir != project || p.Host.Current.ContextState != "active" || ref.Path != filepath.Join(filepath.Dir(p.Stable["task_envelope"].Path), "stopped-local-merge", p.Candidate.SHA256, "plan.json") {
		return r, fmt.Errorf("original stopped merge plan location/identity differs")
	}
	if operation != "inspect" && expected != ref.SHA256 {
		return r, fmt.Errorf("stopped merge expected version changed")
	}
	if err = stoppedMergeRead(p.Candidate, &c); err != nil {
		return r, err
	}
	if err = stoppedMergeRead(p.Native, nil); err != nil {
		return r, err
	}
	if err = stoppedMergeStable(p); err != nil {
		return r, err
	}
	qualified := p
	if err = stoppedMergeValidateCustody(&qualified, c); err != nil || qualified.EffectID != p.EffectID || qualified.Head != p.Head || qualified.Tree != p.Tree {
		return r, fmt.Errorf("original stopped merge qualification changed: %v", err)
	}
	dir := filepath.Dir(ref.Path)
	if canonical, dirErr := filepath.EvalSymlinks(dir); dirErr != nil || canonical != dir {
		return r, fmt.Errorf("original continuation directory changed")
	}
	journalPath := filepath.Join(dir, "continuation.json")
	guard, err := lockfile.TryLock(filepath.Join(dir, "continuation.lock"))
	if err != nil {
		return r, err
	}
	defer guard.Close()
	j := stoppedMergeJournal{Schema: 1, Plan: ref, Phase: "prepared"}
	if raw, readErr := os.ReadFile(journalPath); readErr == nil {
		if json.Unmarshal(raw, &j) != nil || j.Schema != 1 || j.Plan != ref {
			return r, fmt.Errorf("original continuation journal changed")
		}
	} else if !os.IsNotExist(readErr) {
		return r, readErr
	}
	if operation != "inspect" {
		refs := []stoppedMergeRef{ref, p.Native, {Path: filepath.Join(dir, "before.json"), SHA256: hostDigest(p.Snapshot)}}
		if journal, readErr := loopruntime.ReadSessionReadbackFile(journalPath, nil); readErr == nil {
			refs = append(refs, journal)
		} else if !os.IsNotExist(readErr) {
			return r, readErr
		}
		// Atomic visibility after a failed sync is not a durable effect boundary.
		for _, source := range refs {
			if err = stoppedMergeRead(source, nil); err != nil {
				return r, err
			}
			if err = syncStoppedMergeFile(source.Path); err != nil {
				return r, err
			}
			if err = stoppedMergeRead(source, nil); err != nil {
				return r, err
			}
		}
		for _, directory := range []string{dir, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
			if err = filex.SyncDir(directory); err != nil {
				return r, err
			}
		}
	}
	if len(j.After) > 0 {
		computed, computeErr := loop.CompleteStoppedLocalMerge(p.Snapshot, c.OrderID, p.EffectID, ref, j.At)
		if j.At.IsZero() || computeErr != nil || !bytes.Equal(computed, j.After) {
			return r, fmt.Errorf("original continuation after-image differs")
		}
	}
	save := func() error {
		raw, err := json.MarshalIndent(j, "", "  ")
		if err != nil {
			return err
		}
		return filex.WriteFileAtomicDurable(journalPath, append(raw, '\n'))
	}
	r = stoppedMergeResultFor(p, ref, j.Phase, "detach")
	if operation == "detach" {
		if j.Phase != "prepared" && j.Phase != "detached" {
			return r, fmt.Errorf("original detach already progressed; inspect only")
		}
		if err = stoppedMergeBefore(p); err != nil {
			return r, err
		}
		e, environmentErr := execution.New(project, os.Environ())
		if environmentErr != nil {
			return r, environmentErr
		}
		if err = validateHostEnvironment(e); err != nil {
			return r, err
		}
		if err = stoppedMergeAliases(ctx, e, p, c); err != nil {
			return r, err
		}
		if err = stoppedMergeRootBefore(ctx, e, p); err != nil {
			return r, err
		}
		req := hostRequest{Operation: "detach", ProjectDir: project, ConfigPath: p.Host.Current.ConfigPath, ConfigSHA256: p.Host.Current.ConfigSHA256, Mode: p.Host.Current.Mode, RequestID: p.Host.Current.RequestID, ContextID: p.Host.Current.ContextID, InstanceID: p.Host.Current.HostInstanceID, BinarySHA256: p.Host.ExecutionBinary.SHA256, AdmissionSHA: p.Host.AdmissionBinary.SHA256, CleanupCompleted: p.Host.Current.CleanupCompleted}
		detachIntent := filepath.Join(dir, "detach-intent.json")
		if _, intentErr := os.Stat(detachIntent); os.IsNotExist(intentErr) {
			effects = append(effects, "original_context_detach_attempt")
		}
		r.Status = "unknown"
		response, callErr := legacyDetachOnce(ctx, p.Host.GenerationDir, detachIntent, req)
		if callErr != nil || !stoppedMergeDetached(p, response) {
			r.Status = "unknown"
			return r, fmt.Errorf("original detach unresolved; status only, no resend: %v %s", callErr, response.Error)
		}
		if err = stoppedMergeDetachedOwner(p); err != nil {
			return r, err
		}
		if _, err = stoppedMergeSave(filepath.Join(dir, "detached.json"), response); err != nil {
			return r, err
		}
		j.Detached = &response
		j.Phase = "detached"
		if err = save(); err != nil {
			return r, err
		}
		r = stoppedMergeResultFor(p, ref, "detached", "continue")
		return r, nil
	}
	if j.Detached == nil {
		if _, intentErr := os.Stat(filepath.Join(dir, "detach-intent.json")); intentErr == nil {
			r.Status = "detach_unknown"
		}
		if operation == "continue" {
			return r, fmt.Errorf("positive original detached proof is required")
		}
		return r, nil
	}
	if !stoppedMergeDetached(p, *j.Detached) || stoppedMergeDetachedOwner(p) != nil {
		return r, fmt.Errorf("original detached custody changed")
	}
	if operation == "inspect" && j.Phase == "detached" {
		return stoppedMergeResultFor(p, ref, "detached", "continue"), nil
	}
	if operation != "continue" && operation != "inspect" {
		return r, fmt.Errorf("unsupported stopped operation")
	}
	runtime := filepath.Join(project, ".noodle")
	w := filepath.Join(runtime, "noodle.lock")
	if info, statErr := os.Lstat(w); statErr != nil || !info.Mode().IsRegular() {
		return r, fmt.Errorf("original runtime lock missing/changed")
	}
	lock, err := lockfile.TryLock(w)
	if err != nil {
		return r, err
	}
	defer lock.Close()
	control, err := lockfile.TryLock(filepath.Join(runtime, "control.lock"))
	if err != nil {
		return r, err
	}
	defer control.Close()
	e, err := execution.New(project, os.Environ())
	if err != nil {
		return r, err
	}
	if err = validateHostEnvironment(e); err != nil {
		return r, err
	}
	for _, name := range []string{"control.ndjson", "orders-next.json"} {
		raw, readErr := os.ReadFile(filepath.Join(runtime, name))
		if readErr != nil && !os.IsNotExist(readErr) {
			return r, readErr
		}
		if len(bytes.TrimSpace(raw)) != 0 {
			return r, fmt.Errorf("original runtime has pending input")
		}
	}
	snapshotPath := filepath.Join(runtime, "state.snapshot.json")
	if info, statErr := os.Lstat(snapshotPath); statErr != nil || !info.Mode().IsRegular() {
		return r, fmt.Errorf("original canonical snapshot is not regular")
	}
	current, err := os.ReadFile(snapshotPath)
	if err != nil {
		return r, err
	}
	before := bytes.Equal(current, p.Snapshot)
	after := len(j.After) > 0 && bytes.Equal(current, j.After)
	if !before && !after {
		return r, fmt.Errorf("canonical state changed outside original effect continuation")
	}
	if err = stoppedMergeAliases(ctx, e, p, c); err != nil {
		return r, err
	}
	if j.Phase == "completed" || after {
		if !after {
			return r, fmt.Errorf("completed journal lacks original canonical after-image")
		}
		if err = stoppedMergeLanded(ctx, e, p); err != nil {
			return r, err
		}
		if operation == "continue" {
			if barrier != nil {
				if err = barrier("before_canonical_readback_sync"); err != nil {
					return r, err
				}
			}
			if err = syncStoppedMergeFile(snapshotPath); err != nil {
				return r, err
			}
			fresh, readErr := os.ReadFile(snapshotPath)
			if readErr != nil || !bytes.Equal(fresh, j.After) {
				return r, fmt.Errorf("original canonical after-image changed during durability readback")
			}
			if loop.CheckStoppedLocalMergeProjection(project, j.After) != nil {
				effects = append(effects, "original_projection_completion_attempt")
				if err = loop.ProjectStoppedLocalMerge(project, j.After); err != nil {
					return r, err
				}
			}
			if j.Phase != "completed" {
				j.Phase = "completed"
				if err = save(); err != nil {
					return r, err
				}
			}
		}
		if operation == "inspect" && loop.CheckStoppedLocalMergeProjection(project, j.After) != nil {
			return stoppedMergeResultFor(p, ref, "projection_pending", "continue"), nil
		}
		r = stoppedMergeResultFor(p, ref, "completed", "original_integrated_acceptance")
		return stoppedMergeCompletionSources(r, p, j, dir, operation == "continue")
	}
	if j.Phase == "merge-intent" || j.Phase == "git-landed" {
		if err = stoppedMergeLanded(ctx, e, p); err != nil {
			r = stoppedMergeResultFor(p, ref, "unknown", "unknown_original_merge_readback")
			return r, fmt.Errorf("original Git write outcome unknown; no remerge: %w", err)
		}
		j.Phase = "git-landed"
	} else {
		if operation == "inspect" {
			return stoppedMergeResultFor(p, ref, j.Phase, "continue"), nil
		}
		if err = stoppedMergeBefore(p); err != nil {
			return r, err
		}
		if err = stoppedMergeRootBefore(ctx, e, p); err != nil {
			return r, err
		}
		if j.Phase != "detached" && j.Phase != "root-repair-intent" {
			return r, fmt.Errorf("unknown original continuation phase")
		}
		j.Phase = "root-repair-intent"
		if err = save(); err != nil {
			return r, err
		}
		r = stoppedMergeResultFor(p, ref, "unknown", "unknown_original_merge_readback")
		ignore := filepath.Join(project, ".gitignore")
		raw, err := os.ReadFile(ignore)
		if err != nil {
			return r, err
		}
		if bytes.Equal(raw, p.Root.IgnoreBefore) {
			effects = append(effects, "owned_gitignore_repair_attempt")
		} else if !bytes.Equal(raw, p.Root.IgnoreHead) {
			return r, fmt.Errorf("unowned gitignore change")
		}
		if err = stoppedMergeTrimIgnore(p, barrier); err != nil {
			return r, err
		}
		managed, err := worktree.PrepareStorageExcludes(ctx, e)
		if err != nil || !managed {
			return r, fmt.Errorf("original storage exclusions unavailable: %v", err)
		}
		if err = stoppedMergeCleanBase(ctx, e, p); err != nil {
			return r, err
		}
		if barrier != nil {
			if err = barrier("after_root_repair"); err != nil {
				return r, err
			}
		}
		j.Phase = "merge-intent"
		if err = save(); err != nil {
			return r, err
		}
		if barrier != nil {
			if err = barrier("before_git"); err != nil {
				return r, err
			}
		}
		app := &worktree.App{Root: project, CommonDir: p.Root.Custody.CommonDir, Execution: e, Quiet: true, IntegrationBranch: p.Root.Branch}
		effects = append(effects, "same_original_merge_effect_git_attempt")
		if err = app.ContinueBoundGitOnly(filepath.Base(c.Worktree), p.Head, p.Tree); err != nil {
			r = stoppedMergeResultFor(p, ref, "unknown", "unknown_original_merge_readback")
			return r, err
		}
		if barrier != nil {
			if err = barrier("after_git"); err != nil {
				return r, err
			}
		}
		if err = stoppedMergeLanded(ctx, e, p); err != nil {
			return r, err
		}
		j.Phase = "git-landed"
	}
	if operation == "inspect" {
		return stoppedMergeResultFor(p, ref, "git-landed", "continue"), nil
	}
	if len(j.After) == 0 {
		j.At = time.Now().UTC()
		j.After, err = loop.CompleteStoppedLocalMerge(p.Snapshot, c.OrderID, p.EffectID, ref, j.At)
		if err != nil {
			return r, err
		}
	}
	if err = save(); err != nil {
		return r, err
	}
	if barrier != nil {
		if err = barrier("before_canonical"); err != nil {
			return r, err
		}
	}
	if _, err = stoppedMergeArchive(filepath.Join(dir, "after.json"), j.After); err != nil {
		return r, err
	}
	fresh, err := os.ReadFile(filepath.Join(runtime, "state.snapshot.json"))
	if err != nil || !bytes.Equal(fresh, p.Snapshot) {
		return r, fmt.Errorf("original snapshot changed before completion")
	}
	if err = stoppedMergeLanded(ctx, e, p); err != nil {
		return r, err
	}
	effects = append(effects, "original_canonical_completion_attempt")
	if err = filex.WriteFileAtomicDurable(filepath.Join(runtime, "state.snapshot.json"), j.After); err != nil {
		return r, err
	}
	if barrier != nil {
		if err = barrier("after_canonical"); err != nil {
			return r, err
		}
	}
	effects = append(effects, "original_projection_completion_attempt")
	if err = loop.ProjectStoppedLocalMerge(project, j.After); err != nil {
		return r, err
	}
	j.Phase = "completed"
	if err = save(); err != nil {
		return r, err
	}
	r = stoppedMergeResultFor(p, ref, "completed", "original_integrated_acceptance")
	return stoppedMergeCompletionSources(r, p, j, dir, true)
}

// Only the proven suffix is removed; replacing the file would lose its mode
// before a failed rename/sync could be recovered under the same intent.
func stoppedMergeTrimIgnore(p stoppedMergePlan, barrier func(string) error) error {
	path := filepath.Join(p.Project, ".gitignore")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || uint32(info.Mode().Perm()) != p.Root.IgnoreMode {
		return fmt.Errorf("original gitignore inode/mode changed")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("original gitignore changed during open")
	}
	raw, err := io.ReadAll(f)
	if err != nil || (!bytes.Equal(raw, p.Root.IgnoreBefore) && !bytes.Equal(raw, p.Root.IgnoreHead)) {
		return fmt.Errorf("original gitignore bytes changed before suffix removal")
	}
	if bytes.Equal(raw, p.Root.IgnoreBefore) {
		if err = f.Truncate(int64(len(p.Root.IgnoreHead))); err != nil {
			return err
		}
	}
	if barrier != nil {
		if err = barrier("after_ignore_repair"); err != nil {
			return err
		}
	}
	fresh, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, fresh) || fresh.Mode().Perm() != info.Mode().Perm() {
		return fmt.Errorf("original gitignore changed during suffix removal")
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return filex.SyncDir(filepath.Dir(path))
}

func stoppedMergeBefore(p stoppedMergePlan) error {
	for name, want := range map[string][]byte{"state.snapshot.json": p.Snapshot, "control-ack.ndjson": p.ACK} {
		raw, err := os.ReadFile(filepath.Join(p.Project, ".noodle", name))
		if err != nil || !bytes.Equal(raw, want) {
			return fmt.Errorf("original %s changed", name)
		}
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(p.Stable["task_envelope"].Path), "task-owner.json"))
	if err != nil || !bytes.Equal(raw, p.ReviewOwner) {
		return fmt.Errorf("original review changed before continuation")
	}
	return nil
}
func stoppedMergeDetached(p stoppedMergePlan, r hostResponse) bool {
	want := p.Host.Current
	want.ContextState = "detached"
	want.TerminalCause = loop.TerminalMigrationDetach
	return r.Status == "ok" && r.Error == "" && reflect.DeepEqual(r.hostReceipt, want)
}
func stoppedMergeDetachedOwner(p stoppedMergePlan) error {
	owner, err := readHostOwnerBinding(p.Host.Request.Path)
	if err != nil {
		return err
	}
	if owner.Request != p.Host.Request || owner.Original != p.Host.Original || owner.ExecutionBinary != p.Host.ExecutionBinary || owner.AdmissionBinary != p.Host.AdmissionBinary || owner.GenerationDir != p.Host.GenerationDir || !stoppedMergeDetached(p, hostResponse{hostReceipt: owner.Current, Status: "ok"}) {
		return fmt.Errorf("original Context was not positively detached")
	}
	return nil
}
func stoppedMergeAliases(ctx context.Context, e *execution.Context, p stoppedMergePlan, c stoppedMergeCandidate) error {
	entries, err := os.ReadDir(filepath.Join(p.Project, ".noodle", "sessions"))
	if err != nil || len(entries) != 1 || !entries[0].IsDir() || entries[0].Type()&os.ModeSymlink != 0 || entries[0].Name() != c.SessionID {
		return fmt.Errorf("original single session custody changed")
	}
	for _, ref := range p.Root.Sources {
		if err := stoppedMergeRead(ref, nil); err != nil {
			return err
		}
	}
	got, err := worktree.ReadCustody(ctx, e, c.Worktree)
	if err != nil || got != p.Root.Custody {
		return fmt.Errorf("original worker/storage custody changed: %v", err)
	}
	storage := filepath.Dir(filepath.Dir(got.Physical))
	for _, name := range []string{".noodle", ".worktrees"} {
		target, err := filepath.EvalSymlinks(filepath.Join(e.Root(), name))
		info, statErr := os.Lstat(filepath.Join(e.Root(), name))
		if err != nil || statErr != nil || info.Mode()&os.ModeSymlink == 0 || target != filepath.Join(storage, name) {
			return fmt.Errorf("original runtime alias changed")
		}
	}
	head, err := stoppedMergeText(ctx, e, got.Physical, "rev-parse", "HEAD")
	tree, treeErr := stoppedMergeText(ctx, e, got.Physical, "rev-parse", "HEAD^{tree}")
	dirty, dirtyErr := stoppedMergeText(ctx, e, got.Physical, "status", "--porcelain", "--untracked-files=all")
	if err != nil || treeErr != nil || dirtyErr != nil || head != p.Head || tree != p.Tree || dirty != "" {
		return fmt.Errorf("reviewed candidate HEAD/tree/checkout changed")
	}
	return nil
}
func stoppedMergeRootBefore(ctx context.Context, e *execution.Context, p stoppedMergePlan) error {
	info, statErr := os.Lstat(filepath.Join(e.Root(), ".gitignore"))
	if statErr != nil || !info.Mode().IsRegular() || uint32(info.Mode().Perm()) != p.Root.IgnoreMode {
		return fmt.Errorf("original gitignore path/mode changed")
	}
	head, err := stoppedMergeText(ctx, e, e.Root(), "rev-parse", "HEAD")
	index, indexErr := stoppedMergeGit(ctx, e, e.Root(), "ls-files", "--stage", "-z")
	branch, branchErr := stoppedMergeText(ctx, e, e.Root(), "symbolic-ref", "--short", "HEAD")
	if err != nil || indexErr != nil || branchErr != nil || head != p.Root.Head || branch != p.Root.Branch || hostDigest(index) != p.Root.IndexSHA {
		return fmt.Errorf("original integration HEAD/index/branch changed")
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		path, err := stoppedMergeText(ctx, e, e.Root(), "rev-parse", "--git-path", name)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.Root(), path)
		}
		if _, err = os.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("original Git operation is in flight or unknown: %s", name)
		}
	}
	raw, err := os.ReadFile(filepath.Join(e.Root(), ".gitignore"))
	if err != nil || (!bytes.Equal(raw, p.Root.IgnoreBefore) && !bytes.Equal(raw, p.Root.IgnoreHead)) {
		return fmt.Errorf("unowned original root gitignore changes")
	}
	staged, err := stoppedMergeGit(ctx, e, e.Root(), "diff", "--cached", "--name-only")
	if err != nil || len(staged) != 0 {
		return fmt.Errorf("root index changed")
	}
	changed, err := stoppedMergeText(ctx, e, e.Root(), "diff", "--name-only")
	if err != nil || (changed != "" && changed != ".gitignore") {
		return fmt.Errorf("root tracked source changed")
	}
	untracked, err := stoppedMergeGit(ctx, e, e.Root(), "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, name := range strings.Split(strings.TrimRight(string(untracked), "\x00"), "\x00") {
		if name != "" && name != ".noodle" && name != ".worktrees" {
			return fmt.Errorf("root unowned untracked source")
		}
	}
	return nil
}
func stoppedMergeCleanBase(ctx context.Context, e *execution.Context, p stoppedMergePlan) error {
	if err := stoppedMergeRootBefore(ctx, e, p); err != nil {
		return err
	}
	dirty, err := stoppedMergeText(ctx, e, e.Root(), "status", "--porcelain", "--untracked-files=all")
	if err != nil || dirty != "" {
		return fmt.Errorf("original integration checkout remains dirty")
	}
	return nil
}
func stoppedMergeLanded(ctx context.Context, e *execution.Context, p stoppedMergePlan) error {
	head, err := stoppedMergeText(ctx, e, e.Root(), "rev-parse", "HEAD")
	tree, tErr := stoppedMergeText(ctx, e, e.Root(), "rev-parse", "HEAD^{tree}")
	branch, bErr := stoppedMergeText(ctx, e, e.Root(), "symbolic-ref", "--short", "HEAD")
	dirty, dErr := stoppedMergeText(ctx, e, e.Root(), "status", "--porcelain", "--untracked-files=all")
	if err != nil || tErr != nil || bErr != nil || dErr != nil || head != p.Head || tree != p.Tree || branch != p.Root.Branch || dirty != "" {
		return fmt.Errorf("original exact Git landing not observed")
	}
	return nil
}
func stoppedMergeCompletionSources(r stoppedMergeResult, p stoppedMergePlan, j stoppedMergeJournal, dir string, write bool) (stoppedMergeResult, error) {
	for key, name := range map[string]string{"continuation": "continuation.json", "detach_intent": "detach-intent.json", "detached": "detached.json", "canonical_before": "before.json", "canonical_after": "after.json"} {
		ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, name), nil)
		if err != nil {
			return r, err
		}
		r.Sources[key] = ref
	}
	path := filepath.Join(dir, "completion.json")
	receipt := map[string]any{"schema": 1, "kind": "stopped_original_local_merge_completion", "plan": r.Plan, "candidate": p.Candidate, "effect_id": p.EffectID, "executor": p.Host.ExecutionBinary, "controller": p.Controller, "head": p.Head, "tree": p.Tree, "native_completion": p.Native, "detached": r.Sources["detached"], "canonical_before": r.Sources["canonical_before"], "canonical_after": r.Sources["canonical_after"], "completed_at": j.At, "task_acceptance": "NOT_RUN", "authorizes_landing": false}
	if write {
		ref, err := stoppedMergeSave(path, receipt)
		if err != nil {
			return r, err
		}
		r.Sources["receipt"] = ref
	} else {
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if os.IsNotExist(err) {
			return stoppedMergeResultFor(p, r.Plan, "receipt_pending", "continue"), nil
		}
		if err != nil {
			return r, err
		}
		r.Sources["receipt"] = ref
	}
	expected, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil || r.Sources["receipt"].SHA256 != hostDigest(append(expected, '\n')) {
		return r, fmt.Errorf("original completion receipt changed")
	}
	return r, nil
}
