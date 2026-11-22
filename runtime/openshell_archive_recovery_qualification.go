package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// The standalone observer cannot borrow a live controller's in-flight fields.
// Its caller excludes that actor. Each original native intent must also have
// an unambiguous persisted outcome before fresh inventory can qualify.
func qualifyIndependentArchiveCustody(state OpenShellCustodyState, gateway OpenShellGatewaySpec, sessionDir string, recovering ...OpenShellTask) error {
	if state.Binding != gateway.Binding || state.Blocked != "" || len(state.PendingRetained) != 0 ||
		!openShellCustodySupportsProfile(state, gateway) {
		return fmt.Errorf("original archive recovery custody differs or has unresolved initialization")
	}
	if state.Handover != nil {
		if err := verifyCustodyHandover(*state.Handover, state); err != nil {
			return err
		}
	}
	for _, name := range []string{"maintenance-stop-intent.json", "storage-failure-stop-intent.json"} {
		if _, err := os.Lstat(filepath.Join(sessionDir, name)); !os.IsNotExist(err) {
			return fmt.Errorf("original archive recovery has a conflicting Stop intent")
		}
	}
	for _, credential := range state.Credentials {
		if credential.Pending != nil || credential.SourceReadPending ||
			(!credential.InstallationObserved && (credential.MutationID != "" || len(credential.TargetReceipts) != 0)) {
			return fmt.Errorf("original Gateway has an unresolved credential effect")
		}
	}
	for _, row := range state.Records {
		if (!row.Retained && !row.CreateAcknowledged) || row.Worker.StopPending || row.Worker.StopError != "" ||
			(row.StopRequestID != "" && !row.StopAcknowledged) || (row.Exec != nil && !row.Exec.Result.ExitObserved) ||
			(row.ProgressStop != nil && !row.ProgressStop.Result.ExitObserved) || (row.Retirement != nil && !row.Retirement.Retired && (len(recovering) == 0 || row.Task != recovering[0])) {
			return fmt.Errorf("original Gateway has an unresolved sandbox effect")
		}
	}
	return validateOpenShellCustodyLedger(state)
}

// This read-only audit never opens a mutable OpenShellCustody, which could
// persist Blocked or start recovery. The original target is read by Get after
// List so inventory alone does not qualify its exact Stopped identity.
func observeIndependentArchiveInventory(ctx context.Context, state OpenShellCustodyState, client pb.OpenShellClient) error {
	byName := make(map[string]OpenShellCustodyRecord, len(state.Records))
	for _, row := range state.Records {
		byName[row.Name] = row
	}
	seen, tokens := make(map[string]bool), make(map[string]bool)
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_AllWorkspaces{AllWorkspaces: &dm.AllWorkspaces{}}}
	token := ""
	for {
		response, err := client.ListSandboxes(ctx, &pb.ListSandboxesRequest{WorkspaceScope: scope, PageSize: 100, PageToken: token})
		if err != nil {
			return err
		}
		for _, sandbox := range response.GetSandboxes() {
			name := sandbox.GetMetadata().GetName()
			row, known := byName[name]
			if !known || seen[name] || row.Retirement != nil {
				return fmt.Errorf("original Gateway has an unregistered, duplicate or retired sandbox in inventory")
			}
			if err = verifyArchiveRecoveryStopped(row, &pb.SandboxResponse{Sandbox: sandbox}); err != nil {
				return err
			}
			seen[name] = true
		}
		token = response.GetNextPageToken()
		if token == "" {
			break
		}
		if tokens[token] || len(tokens) >= len(byName) {
			return fmt.Errorf("original Gateway inventory pagination is cyclic or unbounded")
		}
		tokens[token] = true
	}
	for _, row := range state.Records {
		if row.Retirement != nil {
			present, err := row.Retirement.Storage.sandboxPresent(row.Worker.Identity.Sandbox.SandboxID)
			if err != nil || present {
				return fmt.Errorf("original Gateway retired sandbox disk absence is unverified: %v", err)
			}
		} else if !seen[row.Name] {
			return fmt.Errorf("original Gateway inventory is missing an owned sandbox")
		}
	}
	return nil
}
