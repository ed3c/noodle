package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

// The fence is only valid inside the owner callback. Reading this structure
// later cannot authorize resource effects or transfer the original obligation.
type archiveRecoveryHostExitFence struct {
	Disposition loopruntime.SessionReadbackFileRef
	Scope       archiveRecoveryCleanupDisposition
	Request     hostRequestBinding
	Owner       *hostOwnerBinding
	Generation  hostGeneration
	Pool        hostPool
	RuntimeDir  string
	Profile     loopruntime.SessionReadbackFileRef
	Custody     loopruntime.SessionReadbackFileRef
	CustodyLock *lockfile.Lock
}

type archiveRecoveryExitInputs struct {
	scope                            archiveRecoveryCleanupDisposition
	request                          hostRequestBinding
	session, runtimeDir, custodyRoot string
	capture                          loopruntime.OpenShellWorkspaceCapture
	profile, captureRef              loopruntime.SessionReadbackFileRef
}

func readArchiveRecoveryExitInputs(ref loopruntime.SessionReadbackFileRef) (archiveRecoveryExitInputs, error) {
	scope, request, err := readOriginalArchiveRecoveryDisposition(ref)
	if err != nil {
		return archiveRecoveryExitInputs{}, err
	}
	session := filepath.Dir(ref.Path)
	runtimeDir := filepath.Dir(filepath.Dir(session))
	if scope.Candidate.Path != filepath.Join(session, "archive-recovery-candidate.json") {
		return archiveRecoveryExitInputs{}, errors.New("original Host exit candidate is outside its session")
	}
	var candidate struct {
		Capture loopruntime.SessionReadbackFileRef `json:"capture"`
	}
	if err = readArchiveCleanupRef(scope.Candidate, &candidate); err != nil {
		return archiveRecoveryExitInputs{}, err
	}
	capture, err := loopruntime.ReadOpenShellWorkspaceCapture(candidate.Capture)
	if err != nil {
		return archiveRecoveryExitInputs{}, err
	}
	if capture.OrderID != scope.OrderID || capture.SessionID != scope.SessionID || capture.NativeAttemptID != scope.NativeAttemptID ||
		capture.SandboxID != scope.SandboxID || capture.MainProcessInstanceID != scope.MainProcessInstanceID ||
		capture.Archive != scope.Archive || capture.Sources["request"] != scope.Request ||
		capture.BaselineSource.Path != filepath.Join(session, "openshell-dispatch.json") {
		return archiveRecoveryExitInputs{}, errors.New("original Host exit capture differs from disposition")
	}
	profile := capture.Sources["profile"]
	var profileBinding struct {
		Gateway struct {
			Binding struct {
				StateRoot string `json:"state_root"`
			} `json:"binding"`
		} `json:"gateway"`
	}
	if err = readArchiveCleanupRef(profile, &profileBinding); err != nil {
		return archiveRecoveryExitInputs{}, err
	}
	custodyRoot := profileBinding.Gateway.Binding.StateRoot
	custodyPath := filepath.Join(custodyRoot, "owner.json")
	if custodyPath != capture.Sources["custody"].Path {
		return archiveRecoveryExitInputs{}, errors.New("original Host exit custody root differs from capture")
	}
	for _, path := range []string{request.HostDir, runtimeDir, session, custodyRoot} {
		if canonical, e := filepath.EvalSymlinks(path); e != nil || !filepath.IsAbs(path) || canonical != path {
			return archiveRecoveryExitInputs{}, fmt.Errorf("original Host exit path is unavailable or indirect: %s", path)
		}
	}
	return archiveRecoveryExitInputs{scope, request, session, runtimeDir, custodyRoot, capture, profile, candidate.Capture}, nil
}

func readArchiveRecoveryExitOwner(scope archiveRecoveryCleanupDisposition, request hostRequestBinding) (*hostOwnerBinding, hostPool, hostGeneration, error) {
	owner, err := readHostOwnerBinding(scope.Request.Path)
	if err != nil {
		return nil, hostPool{}, hostGeneration{}, err
	}
	if owner.Request != scope.Request || owner.HostRoot != request.HostDir ||
		owner.Current.HostInstanceID != scope.HostInstanceID || owner.Current.ContextID != scope.ContextID ||
		owner.Current.RequestID != request.RequestID {
		return nil, hostPool{}, hostGeneration{}, errors.New("original Host exit request or execution owner differs")
	}
	pool, err := readHostPool(owner.HostRoot)
	if err != nil {
		return nil, hostPool{}, hostGeneration{}, err
	}
	generation, err := archiveRecoveryExitGeneration(pool, owner)
	if err != nil {
		return nil, hostPool{}, hostGeneration{}, err
	}
	return owner, pool, generation, nil
}

func readVerifiedArchiveRecoveryFence(ref loopruntime.SessionReadbackFileRef, in archiveRecoveryExitInputs) (*archiveRecoveryHostExitFence, error) {
	scope, request, runtimeDir, custodyRoot, capture, profile := in.scope, in.request, in.runtimeDir, in.custodyRoot, in.capture, in.profile
	custodyPath := filepath.Join(custodyRoot, "owner.json")
	owner, pool, generation, err := readArchiveRecoveryExitOwner(scope, request)
	if err != nil {
		return nil, err
	}
	hold, err := archiveRecoveryPaused(runtimeDir)
	if err != nil || hold != scope.Hold {
		return nil, fmt.Errorf("original Host exit lacks the preserved dispatch hold: %v", err)
	}
	var state loopruntime.OpenShellCustodyState
	custody, err := loopruntime.ReadSessionReadbackFile(custodyPath, &state)
	if err != nil {
		return nil, err
	}
	if state.Binding.StateRoot != custodyRoot || state.ControlRoot != request.HostDir || state.Blocked != "" ||
		len(state.PendingRetained) != 0 {
		return nil, errors.New("original Host exit OpenShell custody differs or remains unresolved")
	}
	config := owner.Current.ConfigPath
	if config == "" {
		config = filepath.Join(owner.Current.ProjectDir, ".noodle.toml")
	}
	sources := map[string]loopruntime.SessionReadbackFileRef{
		"archive_disposition": ref, "archive_candidate": scope.Candidate,
		"archive_capture": in.captureRef, "archive_dispatch": capture.BaselineSource,
		"archive_pause": hold.Acknowledgements, "archive_stop": scope.Stop, "archive_profile": profile,
		"original_config": {Path: config, SHA256: owner.Current.ConfigSHA256}, "native_custody": custody,
	}
	for role, source := range owner.Sources {
		sources[role] = source
	}
	if err = verifyArchiveRecoveryLanding(archiveRecoveryCleanupPlan{Candidate: scope.Candidate, LandingProof: scope.LandingProof,
		Request: scope.Request, OrderID: scope.OrderID, AttemptID: scope.AttemptID, SessionID: scope.SessionID,
		Head: scope.Head, Tree: scope.Tree, OriginalReturn: "UNKNOWN"}); err != nil {
		return nil, err
	}
	if err = loopruntime.VerifySessionReadbackFiles(sources); err != nil {
		return nil, err
	}
	return &archiveRecoveryHostExitFence{Disposition: ref, Scope: scope, Request: request, Owner: owner,
		Generation: generation, Pool: pool, RuntimeDir: runtimeDir, Profile: profile, Custody: custody}, nil
}

func withOriginalArchiveHostExitFence(ctx context.Context, ref loopruntime.SessionReadbackFileRef, action func(*archiveRecoveryHostExitFence) error) error {
	in, err := readArchiveRecoveryExitInputs(ref)
	if err != nil {
		return err
	}
	return withArchiveRecoveryExitLocks(ctx, in.request.HostDir, in.runtimeDir, in.session, in.custodyRoot, func(custodyLock *lockfile.Lock) error {
		fence, err := readVerifiedArchiveRecoveryFence(ref, in)
		if err != nil {
			return err
		}
		if err = verifyOriginalArchiveHostKernelExit(fence.Generation.Process); err != nil {
			return err
		}
		fence.CustodyLock = custodyLock
		return action(fence)
	})
}

func withArchiveRecoveryExitLocks(ctx context.Context, hostRoot, runtimeDir, session, custodyRoot string, action func(*lockfile.Lock) error) error {
	for _, path := range []string{filepath.Join(hostRoot, "host.lock"), filepath.Join(hostRoot, "pool.lock"),
		filepath.Join(runtimeDir, "noodle.lock"), filepath.Join(runtimeDir, "control.lock"),
		filepath.Join(session, "maintenance.lock")} {
		if err := ctx.Err(); err != nil {
			return err
		}
		lock, err := lockfile.TryLockExisting(path)
		if err != nil {
			return fmt.Errorf("original Host exit exclusion unavailable: %w", err)
		}
		defer lock.Close()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	custodyLock, err := lockfile.TryLockExisting(filepath.Join(custodyRoot, "owner.lock"))
	if err != nil {
		return fmt.Errorf("original custody exit exclusion unavailable: %w", err)
	}
	defer custodyLock.Close()
	return action(custodyLock)
}

func archiveRecoveryExitGeneration(pool hostPool, owner *hostOwnerBinding) (hostGeneration, error) {
	original, ok := pool.Owners[owner.Current.RequestID]
	g, exists := pool.Generations[owner.Current.HostInstanceID]
	if !ok || original.State != "owned" || original.MaintenanceHold != "" || !exists || pool.Active != g.ID ||
		g.State != "active" || g.Directory != owner.GenerationDir || g.Binary != owner.ExecutionBinary.Path ||
		g.SHA != owner.ExecutionBinary.SHA256 || g.Process.PID != owner.Current.HostPID || g.Process.SessionID != g.ID ||
		g.Process.PID <= 1 || g.Process.StartedAt.IsZero() {
		return g, errors.New("original Host exit generation or owner is unverified")
	}
	for id, record := range pool.Owners {
		if id != owner.Current.RequestID && record.Current.ContextState != "detached" {
			return g, errors.New("original Host exit has another unresolved request")
		}
	}
	for id, current := range pool.Generations {
		if id != g.ID && current.State != "exited" {
			return g, errors.New("original Host exit has another unresolved generation")
		}
	}
	for _, request := range pool.Queue {
		if request.State != "active" && request.State != "failed" {
			return g, errors.New("original Host exit has pending version input")
		}
	}
	return g, nil
}

// No old socket or receipt participates. An observed reused PID or surviving
// group is uncertain because the original generation may retain a writer.
func verifyOriginalArchiveHostKernelExit(process procx.ProcessRecord) error {
	if process.PID <= 1 || process.SessionID == "" || process.StartedAt.IsZero() {
		return errors.New("original Host kernel generation evidence is incomplete")
	}
	if _, err := procx.ObserveNativeIdentity(process.PID); !errors.Is(err, procx.ErrProcessGone) {
		if err == nil {
			return errors.New("original Host PID is still present")
		}
		return fmt.Errorf("original Host kernel exit unobserved: %w", err)
	}
	absent, err := procx.ProcessGroupAbsent(process.PID)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("original Host process group is still present")
	}
	return nil
}
