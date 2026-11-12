package runtime

import (
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// Native profiles.rs::is_gateway_mintable_strategy owns this classification.
// Static/external values have no required refresh-state row. Co-minted outputs
// are committed by their primary refresh owner, not separate refresh grants.
func openShellGatewayMintable(strategy pb.ProviderCredentialRefreshStrategy) bool {
	switch strategy {
	case pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN,
		pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_CLIENT_CREDENTIALS,
		pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_GOOGLE_SERVICE_ACCOUNT_JWT,
		pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_AWS_STS_ASSUME_ROLE:
		return true
	}
	return false
}

func credentialSourceReadbackSettled(profile *pb.ProviderProfile, keys []string, provider, providerID string, response *pb.GetProviderRefreshStatusResponse, now time.Time) error {
	wait := func() error {
		return credentialWaiting("openshell credential source refresh readback incomplete or unsettled; original owner recovery required")
	}
	declared := make(map[string]pb.ProviderCredentialRefreshStrategy)
	for _, credential := range profile.GetCredentials() {
		strategy := credential.GetRefresh().GetStrategy()
		if !openShellGatewayMintable(strategy) && strategy != pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_UNSPECIFIED && strategy != pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_STATIC && strategy != pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_EXTERNAL {
			return wait()
		}
		for _, key := range credential.GetEnvVars() {
			declared[key] = strategy
		}
	}
	managed := make(map[string]pb.ProviderCredentialRefreshStrategy)
	for _, key := range keys {
		strategy, exists := declared[key]
		if !exists {
			return wait()
		}
		if openShellGatewayMintable(strategy) {
			managed[key] = strategy
		}
	}
	seen := make(map[string]bool)
	for _, state := range response.GetCredentials() {
		if state == nil || state.GetProvider() != provider || state.GetProviderId() != providerID || state.GetCredentialKey() == "" || seen[state.GetCredentialKey()] {
			return wait()
		}
		strategy, exists := declared[state.GetCredentialKey()]
		if !exists {
			return wait()
		}
		if openShellGatewayMintable(strategy) {
			if state.GetStrategy() != strategy {
				return wait()
			}
		} else if openShellGatewayMintable(state.GetStrategy()) || (state.GetStrategy() != pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_UNSPECIFIED && state.GetStrategy() != pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_STATIC && state.GetStrategy() != pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_EXTERNAL) {
			return wait()
		}
		// provider_refresh.rs exposes configured/refreshed only outside the
		// in-progress/committing exchange. A retryable error supplies its own
		// authoritative schedule; absence or a future date does not permit minting.
		switch state.GetStatus() {
		case "configured", "refreshed":
			if state.GetRecoveryAction() != pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_UNSPECIFIED {
				return wait()
			}
		case "error":
			if state.GetRecoveryAction() != pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_RETRY || state.GetNextRefreshTime() == nil || state.GetNextRefreshTime().CheckValid() != nil || state.GetNextRefreshTime().AsTime().After(now) {
				return wait()
			}
		default:
			return wait()
		}
		seen[state.GetCredentialKey()] = true
	}
	for key := range managed {
		if !seen[key] {
			return wait()
		}
	}
	return nil
}
