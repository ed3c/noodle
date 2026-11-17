package runtime

import (
	"context"
	"fmt"
	"path/filepath"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// ReadOpenShellCompletedEvidence verifies the original completed evidence using
// only native Get. It does not attest runtime inactivity. The Host observer must
// bind this result between authoritative original actor inactivity frames.
// No gateway owner, resident, custody reservation, or Runtime is constructed.
func ReadOpenShellCompletedEvidence(ctx context.Context, profile OpenShellProfile, runtimeDir, contextID string, b CompletedSessionBinding) (CompletedSessionReadback, error) {
	if !filepath.IsAbs(runtimeDir) || contextID == "" {
		return CompletedSessionReadback{}, fmt.Errorf("original observer runtime/context missing")
	}
	runtimeSource := runtimeDir
	var err error
	runtimeDir, err = filepath.EvalSymlinks(runtimeSource)
	if err != nil {
		return CompletedSessionReadback{}, err
	}
	spec := profile.Gateway
	spec.Binding.StateRoot, err = filepath.EvalSymlinks(spec.Binding.StateRoot)
	if err != nil {
		return CompletedSessionReadback{}, err
	}
	spec.ControlRoot, err = filepath.EvalSymlinks(spec.ControlRoot)
	if err != nil {
		return CompletedSessionReadback{}, err
	}
	reader := openShellSessionEvidenceReader{freezeOpenShellProfile(profile), runtimeDir, contextID, spec}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return CompletedSessionReadback{}, err
	}
	defer conn.Close()
	client := pb.NewOpenShellClient(conn)
	observe := func(ctx context.Context, expected OpenShellIdentity) (OpenShellCompletedObservation, error) {
		var custody OpenShellCustodyState
		if _, err := ReadSessionReadbackFile(filepath.Join(spec.Binding.StateRoot, "owner.json"), &custody); err != nil {
			return OpenShellCompletedObservation{}, err
		}
		if !openShellCustodySupportsProfile(custody, spec) || custody.Binding != spec.Binding || custody.ControlRoot != spec.ControlRoot || custody.Blocked != "" {
			return OpenShellCompletedObservation{}, fmt.Errorf("original observer custody binding differs")
		}
		if err := validateOpenShellCustodyLedger(custody); err != nil {
			return OpenShellCompletedObservation{}, err
		}
		if custody.Handover != nil {
			if err := verifyCustodyHandover(*custody.Handover, custody); err != nil {
				return OpenShellCompletedObservation{}, err
			}
		}
		if err := verifyOpenShellRetainedRecovery(spec.Binding); err != nil {
			return OpenShellCompletedObservation{}, err
		}
		if len(spec.Retained) != 0 && !sameOpenShellRetained(custody, spec.Retained) {
			return OpenShellCompletedObservation{}, fmt.Errorf("original retained receipt set differs")
		}
		var record OpenShellCustodyRecord
		count := 0
		for _, row := range custody.Records {
			if row.Worker.Identity == expected {
				record = row
				count++
			}
		}
		if count != 1 {
			return OpenShellCompletedObservation{}, fmt.Errorf("original observer completion missing or ambiguous")
		}
		if err := validateCompletedRecord(record, expected); err != nil {
			return OpenShellCompletedObservation{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, openShellObservationTimeout)
		defer cancel()
		response, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: profile.Gateway.Binding.Workspace}}})
		if err != nil {
			return OpenShellCompletedObservation{}, err
		}
		if err = ctx.Err(); err != nil {
			return OpenShellCompletedObservation{}, err
		}
		return settledNativeObservation(record, expected, response.GetSandbox(), completedObservation)
	}
	result, err := reader.read(ctx, b, observe, false, nil)
	if err != nil {
		return result.completed, err
	}
	afterRoot, err := filepath.EvalSymlinks(profile.Gateway.Binding.StateRoot)
	if err != nil || afterRoot != spec.Binding.StateRoot {
		return CompletedSessionReadback{}, fmt.Errorf("original observer custody root changed")
	}
	afterRuntime, err := filepath.EvalSymlinks(runtimeSource)
	if err != nil || afterRuntime != runtimeDir {
		return CompletedSessionReadback{}, fmt.Errorf("original observer runtime root changed")
	}
	return result.completed, nil
}
