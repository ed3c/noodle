package runtime

import (
	"slices"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

func credentialProfileHasRefresh(profile *pb.ProviderProfile) bool {
	for _, credential := range profile.GetCredentials() {
		if openShellGatewayMintable(credential.GetRefresh().GetStrategy()) {
			return true
		}
	}
	return false
}

// A declaration defines alias groups, not stored values or refresh owners.
// This non-secret proof records the actual selection before its source effect.
type OpenShellCredentialSourceRead struct {
	Keys   []string
	Owners map[string]pb.ProviderCredentialRefreshStrategy
}

const openShellCredentialMinimumLifetime = 5 * time.Minute

// Native GetProvider preserves actual inline/handle keys and public expiries.
// provider_credentials.rs::sufficiently_valid and ensure_refreshable determine
// which stored values or actual refresh owners can satisfy the same request.
func credentialSourceKeys(profile *pb.ProviderProfile, provider *dm.Provider, readback *pb.GetProviderRefreshStatusResponse, now time.Time) (*OpenShellCredentialSourceRead, map[string]bool, error) {
	owners := make(map[string]*pb.ProviderCredentialRefreshStatus)
	owned := make(map[string]bool)
	seen := make(map[string]bool)
	for _, state := range readback.GetCredentials() {
		if state == nil || state.GetProvider() != provider.GetMetadata().GetName() || state.GetProviderId() != provider.GetMetadata().GetId() || state.GetCredentialKey() == "" || seen[state.GetCredentialKey()] {
			return nil, nil, credentialWaiting("openshell credential source refresh ownership differs")
		}
		seen[state.GetCredentialKey()] = true
		for _, credential := range profile.GetCredentials() {
			if !slices.Contains(credential.GetEnvVars(), state.GetCredentialKey()) || !openShellGatewayMintable(credential.GetRefresh().GetStrategy()) {
				continue
			}
			if state.GetStrategy() != credential.GetRefresh().GetStrategy() {
				return nil, nil, credentialWaiting("openshell credential source refresh strategy differs")
			}
			outputs := []string{state.GetCredentialKey()}
			// Native resolved_additional_output_keys permits one env var per sibling.
			for _, output := range credential.GetRefresh().GetAdditionalOutputs() {
				for _, sibling := range profile.GetCredentials() {
					if sibling.GetName() == output.GetCredential() && len(sibling.GetEnvVars()) == 1 {
						outputs = append(outputs, sibling.EnvVars[0])
					}
				}
			}
			for _, key := range outputs {
				if owners[key] != nil {
					return nil, nil, credentialWaiting("openshell credential source refresh ownership ambiguous")
				}
				owners[key] = state
				owned[key] = true
			}
		}
	}
	selection := &OpenShellCredentialSourceRead{Owners: make(map[string]pb.ProviderCredentialRefreshStrategy)}
	for _, credential := range profile.GetCredentials() {
		supplied := false
		for _, key := range credential.GetEnvVars() {
			_, stored := provider.GetCredentials()[key]
			valid := stored
			if expiry := provider.GetCredentialExpirationTimes()[key]; expiry != nil {
				valid = stored && expiry.CheckValid() == nil && expiry.AsTime().After(now.Add(openShellCredentialMinimumLifetime))
			}
			owner := owners[key]
			if owner != nil {
				// Native final delivery rejects an in-progress/committing owner even
				// when its old value is valid. A static alias can remain independently usable.
				switch owner.GetStatus() {
				case "configured", "refreshed", "error":
				default:
					continue
				}
			}
			if !valid && !credentialSourceOwnerCanRefresh(owner, now) {
				continue
			}
			selection.Keys = append(selection.Keys, key)
			supplied = true
			if owner != nil {
				selection.Owners[owner.GetCredentialKey()] = owner.GetStrategy()
			}
		}
		if credential.GetRequired() && !supplied {
			return nil, nil, credentialWaiting("openshell credential source required alias group unavailable")
		}
	}
	selection.Keys = slices.Compact(slices.Sorted(slices.Values(selection.Keys)))
	// Native GetProviderCredentials caps its actual nonempty RPC selection at 32 (proto/openshell.proto; provider_credentials.rs MAX_KEYS);
	// an empty optional selection needs no getter and may clear the target.
	if len(selection.Keys) > 32 {
		return nil, nil, credentialWaiting("openshell credential source runtime key selection unavailable")
	}
	return selection, owned, nil
}

func credentialSourceOwnerCanRefresh(owner *pb.ProviderCredentialRefreshStatus, now time.Time) bool {
	if owner == nil {
		return false
	}
	switch owner.GetStatus() {
	case "configured", "refreshed", "error":
	default:
		return false
	}
	switch owner.GetRecoveryAction() {
	case pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_UNSPECIFIED:
		return true
	case pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_RETRY:
		return owner.GetNextRefreshTime() != nil && owner.GetNextRefreshTime().CheckValid() == nil && !owner.GetNextRefreshTime().AsTime().After(now)
	}
	return false
}

func credentialSourceKeySnapshotMatches(before, after *dm.Provider, owned map[string]bool) bool {
	for key := range before.GetCredentials() {
		if _, exists := after.GetCredentials()[key]; !exists {
			return false
		}
	}
	for key := range after.GetCredentials() {
		if _, exists := before.GetCredentials()[key]; !exists && !owned[key] {
			return false
		}
	}
	return true
}
