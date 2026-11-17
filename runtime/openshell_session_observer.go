package runtime

import (
	"context"
	"fmt"
	"path/filepath"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// ReadOpenShellCompletedEvidence verifies the original completed evidence using
// only native Get. The Host observer binds its selected-session stopped proof
// between authoritative original actor and context-lease frames.
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
	profile = freezeOpenShellProfile(profile)
	reader := openShellSessionEvidenceReader{profile, runtimeDir, contextID, spec, true}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return CompletedSessionReadback{}, err
	}
	defer conn.Close()
	client := pb.NewOpenShellClient(conn)
	observe := func(ctx context.Context, expected OpenShellIdentity) (OpenShellCompletedObservation, error) {
		snapshot, err := readCompletionCustody(spec, SessionCustodySubject{contextID, b.OrderID, b.SessionID, expected.AttemptID}, &expected)
		if err != nil {
			return OpenShellCompletedObservation{}, err
		}
		record := snapshot.record
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
	// The original reader verifies the complete profile and selected evidence
	// before and after Get. Preserve that result rather than repeating its read.
	afterRoot, err := filepath.EvalSymlinks(profile.Gateway.Binding.StateRoot)
	if err != nil || afterRoot != spec.Binding.StateRoot {
		return CompletedSessionReadback{}, fmt.Errorf("original observer custody root changed")
	}
	afterControl, err := filepath.EvalSymlinks(profile.Gateway.ControlRoot)
	if err != nil || afterControl != spec.ControlRoot {
		return CompletedSessionReadback{}, fmt.Errorf("original observer control root changed")
	}
	afterRuntime, err := filepath.EvalSymlinks(runtimeSource)
	if err != nil || afterRuntime != runtimeDir {
		return CompletedSessionReadback{}, fmt.Errorf("original observer runtime root changed")
	}
	return result.completed, nil
}
