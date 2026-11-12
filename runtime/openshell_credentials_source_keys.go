package runtime

import (
	"slices"

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

// Native GetProvider redacts values but preserves actual inline/handle keys.
// A profile's env_vars are aliases: Required needs any one, not every alias.
func credentialSourceKeys(profile *pb.ProviderProfile, provider *dm.Provider, readback *pb.GetProviderRefreshStatusResponse) ([]string, map[string]bool, error) {
	available, owned := make(map[string]bool), make(map[string]bool)
	for key := range provider.GetCredentials() {
		available[key] = true
	}
	for _, state := range readback.GetCredentials() {
		if state.GetProvider() != provider.GetMetadata().GetName() || state.GetProviderId() != provider.GetMetadata().GetId() {
			return nil, nil, credentialWaiting("openshell credential source refresh ownership differs")
		}
		for _, credential := range profile.GetCredentials() {
			if !slices.Contains(credential.GetEnvVars(), state.GetCredentialKey()) || !openShellGatewayMintable(credential.GetRefresh().GetStrategy()) {
				continue
			}
			if state.GetStrategy() != credential.GetRefresh().GetStrategy() {
				return nil, nil, credentialWaiting("openshell credential source refresh strategy differs")
			}
			switch state.GetStatus() {
			case "configured", "refreshed", "error", "refresh_in_progress", "refresh_committing":
			default:
				continue
			}
			owned[state.GetCredentialKey()] = true
			// profiles.rs::resolved_additional_output_keys permits exactly one
			// env var per sibling output. No alias is invented for a co-mint.
			for _, output := range credential.GetRefresh().GetAdditionalOutputs() {
				for _, sibling := range profile.GetCredentials() {
					if sibling.GetName() == output.GetCredential() && len(sibling.GetEnvVars()) == 1 {
						owned[sibling.EnvVars[0]] = true
					}
				}
			}
		}
	}
	var keys []string
	for _, credential := range profile.GetCredentials() {
		supplied := false
		for _, key := range credential.GetEnvVars() {
			if available[key] || owned[key] {
				keys = append(keys, key)
				supplied = true
			}
		}
		if credential.GetRequired() && !supplied {
			return nil, nil, credentialWaiting("openshell credential source required alias group unavailable")
		}
	}
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	// Pinned OpenShell GetProviderCredentialsRequest caps its actual selection
	// at 32 (proto/openshell.proto; server provider_credentials.rs MAX_KEYS).
	if len(keys) == 0 || len(keys) > 32 {
		return nil, nil, credentialWaiting("openshell credential source runtime key selection unavailable")
	}
	return keys, owned, nil
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
