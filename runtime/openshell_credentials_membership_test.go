package runtime

import (
	"errors"
	"slices"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestOpenShellCredentialMembershipValidValueSurvivesNativeRecoveryStatus(t *testing.T) {
	for _, recovery := range []struct {
		state  string
		action pb.ProviderCredentialRefreshRecoveryAction
	}{
		{"reauthorization_required", pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_REAUTHORIZE},
		{"configuration_required", pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_FIX_CONFIGURATION},
		{"investigation_required", pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_INVESTIGATE},
	} {
		for _, valid := range []bool{true, false} {
			t.Run(recovery.state+map[bool]string{true: "/valid", false: "/expired"}[valid], func(t *testing.T) {
				o, source, target, now := credentialFixture(t)
				source.refreshReadback = credentialSettledReadback()
				source.refreshReadback.Credentials[0].Status = recovery.state
				source.refreshReadback.Credentials[0].RecoveryAction = recovery.action
				if !valid {
					source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = timestamppb.New(now.Add(-time.Minute))
				}
				err := credentialDeliver(t, o)
				if !valid {
					var waiting *openShellCredentialWaiting
					if !errors.As(err, &waiting) || source.retrievals != 0 || target.creates != 0 {
						t.Fatalf("expired recovery-blocked value was minted: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if source.retrievals != 1 || target.creates != 1 || target.provider.Credentials["CODEX_AUTH_ACCESS_TOKEN"] != "synthetic-initial-access" {
					t.Fatal("native-valid value refused because refresh requires recovery")
				}
			})
		}
	}
}

func TestOpenShellCredentialMembershipRetainsUnfetchedPresentOptional(t *testing.T) {
	for _, mode := range []string{"in-progress", "committing", "unrefreshable-present", "true-delete", "all-optional-active", "required-alias-fallback"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			optional := &pb.ProviderProfileCredential{Name: "optional", EnvVars: []string{"OPTIONAL"}, Refresh: &pb.ProviderCredentialRefresh{Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN}}
			source.profile = &pb.ProviderProfile{Id: "optional-membership", Credentials: []*pb.ProviderProfileCredential{optional, {Name: "base", EnvVars: []string{"BASE"}, Required: true}}}
			if mode == "required-alias-fallback" {
				source.profile.Credentials = []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"OPTIONAL", "BASE"}, Required: true, Refresh: optional.Refresh}}
			}
			source.provider.Type = source.profile.Id
			expiry := timestamppb.New(now.Add(10 * time.Minute))
			source.values = map[string]*pb.ProviderCredentialValue{"OPTIONAL": {Value: "synthetic-retained", ExpirationTime: expiry}, "BASE": {Value: "synthetic-base"}}
			if mode == "all-optional-active" {
				source.profile.Credentials = source.profile.Credentials[:1]
				delete(source.values, "BASE")
			}
			source.refreshReadback = &pb.GetProviderRefreshStatusResponse{Credentials: []*pb.ProviderCredentialRefreshStatus{{Provider: "chosen", ProviderId: "source-provider", CredentialKey: "OPTIONAL", Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN, Status: "refreshed"}}}
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(5 * time.Minute)
			switch mode {
			case "committing":
				source.refreshReadback.Credentials[0].Status = "refresh_committing"
			case "true-delete":
				delete(source.values, "OPTIONAL")
				source.refreshReadback.Credentials = nil
			case "unrefreshable-present":
				source.values["OPTIONAL"].ExpirationTime = timestamppb.New(now.Add(-time.Minute))
				source.refreshReadback.Credentials[0].Status = "reauthorization_required"
				source.refreshReadback.Credentials[0].RecoveryAction = pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_REAUTHORIZE
			default:
				source.refreshReadback.Credentials[0].Status = "refresh_in_progress"
			}
			if source.values["BASE"] != nil {
				source.values["BASE"].Value = "synthetic-new-base"
			}
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			if target.creates != 1 || target.updates != 1 {
				t.Fatal("membership reconciliation replaced provider")
			}
			if mode == "true-delete" {
				if _, exists := target.provider.Credentials["OPTIONAL"]; exists {
					t.Fatal("true removed source member was retained")
				}
				if target.provider.CredentialExpirationTimes["OPTIONAL"] != nil {
					t.Fatal("true removal retained expiry")
				}
				return
			}
			if target.provider.Credentials["OPTIONAL"] != "synthetic-retained" || !proto.Equal(target.provider.CredentialExpirationTimes["OPTIONAL"], expiry) {
				t.Fatal("temporarily unfetched optional member was tombstoned")
			}
			row := o.custody.Snapshot().Credentials[0]
			if !row.ExpiresAt.Equal(expiry.AsTime()) || row.RefreshAt.After(now.Add(30*time.Second)) {
				t.Fatal("retained material expiry was omitted from renewal schedule")
			}
			expected := 2
			if mode == "all-optional-active" {
				expected = 1
			}
			if source.retrievals != expected {
				t.Fatal("unavailable optional member was fetched")
			}
			if mode == "required-alias-fallback" && target.provider.Credentials["BASE"] != "synthetic-new-base" {
				t.Fatal("independent valid alias became unavailable")
			}
			credentialNoLeaks(t, o)
		})
	}
}

func TestOpenShellCredentialMembershipRetrievalNotFoundRetainsUnknown(t *testing.T) {
	for _, mode := range []string{"optional-delete", "alias-swap"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			source.profile = &pb.ProviderProfile{Id: "membership-race", Credentials: []*pb.ProviderProfileCredential{{Name: "base", EnvVars: []string{"BASE"}, Required: true}, {Name: "optional", EnvVars: []string{"OLD"}}}}
			source.provider.Type = source.profile.Id
			source.values = map[string]*pb.ProviderCredentialValue{"BASE": {Value: "synthetic-base"}, "OLD": {Value: "synthetic-old", ExpirationTime: timestamppb.New(now.Add(10 * time.Minute))}}
			if mode == "alias-swap" {
				source.profile.Credentials = []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"OLD", "NEW"}, Required: true}}
				delete(source.values, "BASE")
			}
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(5 * time.Minute)
			source.values["OLD"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
			source.retrievalNotFoundOnce = true
			source.onRetrievalNotFound = func(s *credentialNative) {
				delete(s.values, "OLD")
				if mode == "alias-swap" {
					s.values["NEW"] = &pb.ProviderCredentialValue{Value: "synthetic-new", ExpirationTime: timestamppb.New(now.Add(time.Hour))}
				}
			}
			err := credentialDeliver(t, o)
			var waiting *openShellCredentialWaiting
			if !errors.As(err, &waiting) {
				t.Fatalf("source NotFound consumed original task: %v", err)
			}
			before := o.custody.Snapshot().Credentials[0]
			if !before.SourceReadPending || before.SourceRead == nil || !slices.Contains(before.SourceRead.Keys, "OLD") || target.updates != 0 {
				t.Fatal("NotFound incorrectly proved no effect or mutated target")
			}
			events := len(source.retrievalEvents)
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			if len(source.retrievalEvents) < events+2 || source.retrievalEvents[events] != "readback" || source.retrievalEvents[events+1] != "retrieve" {
				t.Fatal("retrieval NotFound blindly retried without original readback")
			}
			if target.creates != 1 || target.updates != 1 || target.provider.Credentials["OLD"] != "" {
				t.Fatal("source race recreated target or retained proved removal")
			}
			if mode == "alias-swap" && target.provider.Credentials["NEW"] != "synthetic-new" {
				t.Fatal("valid replacement alias unavailable")
			}
		})
	}
	for _, operation := range []string{"source provider", "target provider", "target mutation"} {
		var waiting *openShellCredentialWaiting
		if errors.As(credentialFailure(operation, status.Error(codes.NotFound, "missing")), &waiting) {
			t.Fatalf("unrelated %s NotFound became transient", operation)
		}
	}
}
