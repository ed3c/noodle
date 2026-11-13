package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

// A current bounded observation, never a durable fence, drain, retirement or
// authority. The full controller must maintain its own original control epoch.
type sourceComputeObservation struct {
	State      string                                `json:"state"`
	Diagnostic string                                `json:"diagnostic,omitempty"`
	ObservedAt time.Time                             `json:"observed_at"`
	History    *loopruntime.OpenShellPhysicalHistory `json:"physical_history,omitempty"`
}

func sourceComputeFenced(scope *hostOperatorNativeScope) (bool, error) {
	if scope == nil || scope.Schema != 1 {
		return false, errors.New("original Source native scope is missing")
	}
	for i, actor := range []hostOperatorActor{scope.Host, scope.Gateway, scope.Driver} {
		current, err := procx.ObserveNativeIdentity(actor.Process.PID)
		if err != nil || !actor.Process.SameGeneration(current) || current.Zombie || current.ParentPID != actor.Process.ParentPID || current.Group != actor.Process.Group {
			return false, errors.New("original Source actor generation or ancestry changed")
		}
		if i != 1 && !current.Stopped {
			return false, nil
		}
		if i == 1 && current.Stopped {
			return false, errors.New("credential gateway is suspended; normal native drain is not established")
		}
		binary, err := hostReadbackBinaryRef(actor.Binary.Path)
		if err != nil || binary != actor.Binary {
			return false, errors.New("original Source actor executable changed")
		}
	}
	return true, nil
}

// VM/supervisor children retain the original groups. Preparation is the native
// exception and must retain a lease before spawning outside those groups.
func sourceComputeLocal(scope *hostOperatorNativeScope, rows []admission.ProcessObservation) error {
	actors := []procx.NativeIdentity{scope.Host.Process, scope.Gateway.Process, scope.Driver.Process}
	seen := map[int]bool{}
	for _, row := range rows {
		index := slices.IndexFunc(actors, func(p procx.NativeIdentity) bool { return p.PID == row.PID })
		if index >= 0 {
			actor := actors[index]
			if row.UID == nil || *row.UID != actor.UID || row.ParentPID != actor.ParentPID || row.ProcessGroup != actor.Group {
				return errors.New("Source process metadata differs from its selected kernel actor")
			}
			seen[row.PID] = true
			continue
		}
		for _, actor := range actors {
			if row.ProcessGroup == actor.Group || slices.Contains(row.AncestorPIDs, actor.PID) || row.ParentPID == actor.PID {
				return fmt.Errorf("Source has an additional process: %d", row.PID)
			}
		}
	}
	if len(seen) != len(actors) {
		return errors.New("Source process observation is missing an original actor")
	}
	root, err := filepath.EvalSymlinks(scope.StateDirectory)
	if err != nil || root != scope.StateDirectory || !filepath.IsAbs(root) {
		return errors.New("original Source VM namespace is unavailable or indirect")
	}
	for _, path := range []string{filepath.Join(root, "sandboxes"), filepath.Join(root, "images", "preparations")} {
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil || canonical != path {
			return fmt.Errorf("Source namespace directory is unavailable or indirect: %s", path)
		}
		before, err := os.Lstat(path)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Source namespace directory is missing or indirect: %s", path)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("Source namespace retains sandbox or preparation ownership: %s", path)
		}
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, after) {
			return errors.New("Source namespace changed during observation")
		}
	}
	return nil
}

func readSourceComputeStatus(ctx context.Context, p hostMaintenancePlan) sourceComputeObservation {
	result := sourceComputeObservation{State: "unknown", ObservedAt: time.Now().UTC()}
	fail := func(err error) sourceComputeObservation { result.Diagnostic = err.Error(); return result }
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		return fail(err)
	}
	fenced, err := sourceComputeFenced(op.NativeScope)
	if err != nil {
		return fail(err)
	}
	if !fenced {
		result.State, result.Diagnostic = "unfenced", "original Host and driver must remain suspended before current Source compute qualification"
		return result
	}
	history, err := loopruntime.InspectOpenShellPhysicalHistory(op.Custody)
	if err != nil {
		return fail(err)
	}
	result.History = &history
	checkLocal := func() error {
		rows, err := admission.ObserveVisibleProcesses(ctx)
		if err != nil {
			return err
		}
		return sourceComputeLocal(op.NativeScope, rows)
	}
	if err = checkLocal(); err != nil {
		return fail(err)
	}
	if err = verifySourcePolicyHoldFiles(op); err != nil {
		return fail(err)
	}
	gateway, err := loopruntime.LoadOpenShellGatewaySpec(op.ProfileSource)
	if err != nil || gateway.Binding != op.Binding {
		return fail(errors.New("Source gateway binding changed before inventory readback"))
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	count, err := loopruntime.ObserveOpenShellStoreCount(readCtx, gateway)
	if err != nil {
		return fail(err)
	}
	if count != 0 {
		return fail(errors.New("Source gateway retains sandbox metadata; preserve its original owner readback"))
	}
	if err = checkLocal(); err != nil {
		return fail(err)
	}
	fenced, err = sourceComputeFenced(op.NativeScope)
	if err != nil {
		return fail(err)
	}
	if !fenced {
		return fail(errors.New("Source producer suspension changed during observation"))
	}
	if err = verifySourcePolicyHoldFiles(op); err != nil {
		return fail(err)
	}
	if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"custody": op.Custody, "native-config": op.NativeScope.GatewayConfig}); err != nil {
		return fail(err)
	}
	result.State = "observed_zero"
	return result
}
