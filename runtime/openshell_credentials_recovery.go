package runtime

import (
	"context"
	"fmt"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// With no active/native consumer, recovery may only observe an existing
// operation. Its ACK never authorizes the next fresh retrieval or creation.
func (o *OpenShellGateway) recoverCredentialRow(ctx context.Context, row *OpenShellCredentialDelivery) error {
	if row.Pending != nil {
		return o.replayCredential(ctx, row)
	}
	if !row.SourceReadPending {
		return nil
	}
	source := o.spec.CredentialSource
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: source.Workspace}}
	response, err := o.credentialClient.GetProvider(ctx, &pb.GetProviderRequest{WorkspaceScope: scope, Name: row.Provider})
	if err != nil {
		return credentialFailure("source orphan provider readback", err)
	}
	provider := response.GetProvider()
	if provider.GetMetadata().GetName() != row.Provider || provider.GetMetadata().GetId() != row.SourceProviderID || provider.GetMetadata().GetWorkspace() != source.Workspace {
		return fmt.Errorf("openshell credential orphan source provider changed")
	}
	var profileScope *dm.WorkspaceSelector
	if provider.GetProfileWorkspace() != "" {
		profileScope = &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: provider.GetProfileWorkspace()}}
	}
	profileResponse, err := o.credentialClient.GetProviderProfile(ctx, &pb.GetProviderProfileRequest{WorkspaceScope: profileScope, Id: provider.GetType()})
	if err != nil {
		return credentialFailure("source orphan profile readback", err)
	}
	profile, _, err := externalOpenShellProfile(profileResponse.GetProfile())
	if err != nil {
		return err
	}
	if profile.Id != row.ProfileID {
		return fmt.Errorf("openshell credential orphan source profile changed")
	}
	status, err := o.credentialClient.GetProviderRefreshStatus(ctx, &pb.GetProviderRefreshStatusRequest{WorkspaceScope: scope, Provider: row.Provider})
	if err != nil {
		return credentialFailure("source orphan refresh readback", err)
	}
	if err := credentialSourceReadbackSettled(profileResponse.GetProfile(), row.SourceRead, row.Provider, row.SourceProviderID, status, o.credentialTime()); err != nil {
		return err
	}
	// GetProviderRefreshStatus cannot return the original GetCredentials ACK.
	// Preserve that uncertainty and the original next; do not mint on its behalf.
	return credentialWaiting("openshell credential original source retrieval acknowledgement remains unknown after readback")
}
