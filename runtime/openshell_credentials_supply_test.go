package runtime

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestOpenShellCredentialSupplyExcludesUnrefreshableExpiredAlias(t *testing.T) {
	for _, mode := range []string{"expired", "insufficient", "valid", "none-supplied"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			source.profile = &pb.ProviderProfile{Id: "supply-alias", Credentials: []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"A", "B"}, Required: true}}}
			source.provider.Type = source.profile.Id
			deadline := now.Add(-time.Minute)
			if mode == "insufficient" {
				deadline = now.Add(4 * time.Minute)
			}
			if mode == "valid" {
				deadline = now.Add(time.Hour)
			}
			source.values = map[string]*pb.ProviderCredentialValue{"A": {Value: "synthetic-A", ExpirationTime: timestamppb.New(deadline)}}
			if mode != "none-supplied" {
				source.values["B"] = &pb.ProviderCredentialValue{Value: "synthetic-B", ExpirationTime: timestamppb.New(now.Add(time.Hour))}
			}
			err := credentialDeliver(t, o)
			if mode == "none-supplied" {
				var waiting *openShellCredentialWaiting
				if !errors.As(err, &waiting) || source.retrievals != 0 || target.creates != 0 {
					t.Fatalf("unavailable required group was retrieved: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, hasA := target.provider.Credentials["A"]
			if hasA != (mode == "valid") || target.provider.Credentials["B"] != "synthetic-B" {
				t.Fatal("supply selection mixed stored presence with usable lifetime")
			}
		})
	}
}

func TestOpenShellCredentialSupplyEmptyOptionalClearsTargetWithoutRetrieval(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	source.profile = &pb.ProviderProfile{Id: "only-optional", Credentials: []*pb.ProviderProfileCredential{{Name: "optional", EnvVars: []string{"OPTIONAL"}}}}
	source.provider.Type = source.profile.Id
	source.values = map[string]*pb.ProviderCredentialValue{"OPTIONAL": {Value: "synthetic-optional", ExpirationTime: timestamppb.New(now.Add(time.Hour))}}
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	delete(source.values, "OPTIONAL")
	*now = now.Add(55 * time.Minute)
	gets := source.gets
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	if source.retrievals != 1 || source.gets != gets+2 || target.creates != 1 || target.updates != 1 || len(target.provider.Credentials) != 0 || len(target.provider.CredentialExpirationTimes) != 0 {
		t.Fatal("empty optional supply retrieved or retained stale target material, or lost bracket")
	}
	if o.custody.Snapshot().Credentials[0].SourceReadPending {
		t.Fatal("empty selection fabricated a source effect")
	}
}

func TestOpenShellCredentialSupplyLostReplyUsesActualOwnerBinding(t *testing.T) {
	for _, mode := range []string{"mixed-static-alias", "static-only-unready-owner", "missing-original-owner", "legacy-unproven"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			source.profile = &pb.ProviderProfile{Id: "managed-alias", Credentials: []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"A", "B"}, Required: true, Refresh: &pb.ProviderCredentialRefresh{Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN}}}}
			source.provider.Type = source.profile.Id
			source.values = map[string]*pb.ProviderCredentialValue{"A": {Value: "synthetic-managed", ExpirationTime: timestamppb.New(now.Add(time.Hour))}, "B": {Value: "synthetic-static"}}
			source.refreshReadback = &pb.GetProviderRefreshStatusResponse{Credentials: []*pb.ProviderCredentialRefreshStatus{{Provider: "chosen", ProviderId: "source-provider", CredentialKey: "A", Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN, Status: "refreshed"}}}
			if mode == "static-only-unready-owner" {
				source.values["A"].ExpirationTime = timestamppb.New(now.Add(-time.Minute))
				source.refreshReadback.Credentials[0].Status = "refresh_in_progress"
			}
			source.sourceLost = true
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("lost reply expected")
			}
			if mode == "missing-original-owner" {
				source.refreshReadback.Credentials = nil
			}
			if mode == "legacy-unproven" {
				// Decode an old v5 row, which has no proof of the original retrieval.
				row := o.custody.state.Credentials[0]
				var legacy OpenShellCredentialDelivery
				data, _ := json.Marshal(map[string]any{"Provider": row.Provider, "SourceBinding": row.SourceBinding, "SourceProviderID": row.SourceProviderID, "ProfileID": row.ProfileID, "SourceReadPending": true})
				if err := json.Unmarshal(data, &legacy); err != nil {
					t.Fatal(err)
				}
				o.custody.state.Credentials[0] = legacy
			}
			err := credentialDeliver(t, o)
			if mode != "mixed-static-alias" && mode != "static-only-unready-owner" {
				var waiting *openShellCredentialWaiting
				if !errors.As(err, &waiting) || source.retrievals != 1 || target.creates != 0 {
					t.Fatalf("unknown original owner binding authorized retrieval: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := 2
			if mode == "static-only-unready-owner" {
				expected = 1
			}
			if source.retrievals != 2 || target.creates != 1 || len(target.provider.Credentials) != expected {
				t.Fatal("static alias required a fabricated refresh row")
			}
		})
	}
}

func TestOpenShellCredentialSupplyUnreadyOwnerKeepsStaticAlias(t *testing.T) {
	for _, mode := range []string{"reauthorize", "retry-future", "in-progress", "committing", "valid-with-reauthorize", "no-static-neighbor"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			source.profile = &pb.ProviderProfile{Id: "owner-alias", Credentials: []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"A", "B"}, Required: true, Refresh: &pb.ProviderCredentialRefresh{Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN}}}}
			source.provider.Type = source.profile.Id
			source.values = map[string]*pb.ProviderCredentialValue{"A": {Value: "synthetic-A", ExpirationTime: timestamppb.New(now.Add(-time.Minute))}, "B": {Value: "synthetic-B"}}
			owner := &pb.ProviderCredentialRefreshStatus{Provider: "chosen", ProviderId: "source-provider", CredentialKey: "A", Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN, Status: "error", RecoveryAction: pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_REAUTHORIZE}
			switch mode {
			case "retry-future":
				owner.RecoveryAction = pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_RETRY
				owner.NextRefreshTime = timestamppb.New(now.Add(time.Minute))
			case "in-progress":
				owner.Status = "refresh_in_progress"
			case "committing":
				owner.Status = "refresh_committing"
			case "valid-with-reauthorize":
				source.values["A"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
			case "no-static-neighbor":
				delete(source.values, "B")
			}
			source.refreshReadback = &pb.GetProviderRefreshStatusResponse{Credentials: []*pb.ProviderCredentialRefreshStatus{owner}}
			err := credentialDeliver(t, o)
			if mode == "no-static-neighbor" {
				var waiting *openShellCredentialWaiting
				if !errors.As(err, &waiting) || source.retrievals != 0 || target.creates != 0 {
					t.Fatalf("unready only alias retrieved: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, hasA := target.provider.Credentials["A"]
			if hasA != (mode == "valid-with-reauthorize") || target.provider.Credentials["B"] != "synthetic-B" {
				t.Fatal("unready owner blocked independent static alias or refused a native-valid stored value")
			}
		})
	}
}

func TestOpenShellCredentialSupplyEmptyDoesNotClearPriorSourceUnknown(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	source.profile = &pb.ProviderProfile{Id: "optional-unknown", Credentials: []*pb.ProviderProfileCredential{{Name: "optional", EnvVars: []string{"OPTIONAL"}}}}
	source.provider.Type = source.profile.Id
	source.values = map[string]*pb.ProviderCredentialValue{"OPTIONAL": {Value: "synthetic-optional", ExpirationTime: timestamppb.New(now.Add(time.Hour))}}
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(55 * time.Minute)
	source.values["OPTIONAL"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
	source.sourceLost = true
	if err := credentialDeliver(t, o); err == nil {
		t.Fatal("source reply loss expected")
	}
	proof := o.custody.Snapshot().Credentials[0].SourceRead
	delete(source.values, "OPTIONAL")
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	row := o.custody.Snapshot().Credentials[0]
	if !row.SourceReadPending || row.SourceRead == nil || !reflect.DeepEqual(row.SourceRead, proof) || source.retrievals != 2 || target.updates != 1 || len(target.provider.Credentials) != 0 || len(target.provider.CredentialExpirationTimes) != 0 {
		t.Fatal("empty selection replayed/cleared source unknown or failed target clear")
	}
	if err := o.custody.Quiesce(t.Context()); err != nil {
		t.Fatalf("source-only unknown pinned target: %v", err)
	}
}

func TestOpenShellCredentialSupplyEmptyProfileNeedsNoGetter(t *testing.T) {
	o, source, target, _ := credentialFixture(t)
	source.profile = &pb.ProviderProfile{Id: "no-credentials"}
	source.provider.Type = source.profile.Id
	source.values = map[string]*pb.ProviderCredentialValue{}
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	if source.retrievals != 0 || target.creates != 1 || source.gets != 2 || len(target.provider.Credentials) != 0 {
		t.Fatal("empty profile invented a getter requirement or bypassed source bracket")
	}
}

func TestOpenShellCredentialSupplyProofSurvivesReopenAndSnapshot(t *testing.T) {
	o, source, target, _ := credentialFixture(t)
	source.sourceLost = true
	if err := credentialDeliver(t, o); err == nil {
		t.Fatal("lost source reply expected")
	}
	detached := o.custody.Snapshot().Credentials[0].SourceRead
	if detached == nil || len(detached.Keys) != 1 || len(detached.Owners) != 1 {
		t.Fatal("source effect lacks its original key/owner proof")
	}
	detached.Keys[0] = "foreign"
	clear(detached.Owners)
	recovered := reopenCredentialOwner(t, o)
	proof := recovered.custody.Snapshot().Credentials[0].SourceRead
	if proof == nil || proof.Keys[0] != "CODEX_AUTH_ACCESS_TOKEN" || len(proof.Owners) != 1 {
		t.Fatal("source proof lost or aliased across persistence")
	}
	if err := credentialDeliver(t, recovered); err != nil {
		t.Fatal(err)
	}
	if source.retrievals != 2 || target.creates != 1 {
		t.Fatal("persisted source proof failed original recovery")
	}
	credentialNoLeaks(t, recovered)
}

func TestOpenShellCredentialSupplyRejectsMalformedPersistedProof(t *testing.T) {
	for _, mode := range []string{"empty-keys", "duplicate-key", "empty-owner-key", "unknown-owner", "missing-owner-map", "nonpending-proof"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, _ := credentialFixture(t)
			source.sourceLost = true
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("lost reply expected")
			}
			row := &o.custody.state.Credentials[0]
			switch mode {
			case "empty-keys":
				row.SourceRead.Keys = nil
			case "duplicate-key":
				row.SourceRead.Keys = append(row.SourceRead.Keys, row.SourceRead.Keys[0])
			case "empty-owner-key":
				row.SourceRead.Owners[""] = pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN
			case "unknown-owner":
				row.SourceRead.Owners[row.SourceRead.Keys[0]] = pb.ProviderCredentialRefreshStrategy(999)
			case "missing-owner-map":
				row.SourceRead.Owners = nil
			case "nonpending-proof":
				row.SourceReadPending = false
			}
			if err := o.custody.validateLedger(); err == nil {
				t.Fatal("malformed persisted proof accepted")
			}
			if source.retrievals != 1 || target.creates != 0 {
				t.Fatal("proof validation introduced fresh effects")
			}
		})
	}
}
