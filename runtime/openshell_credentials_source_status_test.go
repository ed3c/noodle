package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These managed/static declarations reproduce the relevant original exported
// codex-noodle profile: access_token owns OAuth refresh; account_id is static.
// The operator readback is archived at original-login-profile-readback.json.
func credentialOriginalCodexDeclarations(source *credentialNative) {
	source.profile = &pb.ProviderProfile{Id: "codex-noodle", Credentials: []*pb.ProviderProfileCredential{
		{Name: "access_token", EnvVars: []string{"CODEX_AUTH_ACCESS_TOKEN"}, Required: true, Refresh: &pb.ProviderCredentialRefresh{Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN}},
		{Name: "account_id", EnvVars: []string{"CODEX_AUTH_ACCOUNT_ID"}, Required: true},
	}}
	source.provider.Type = source.profile.Id
	source.values["CODEX_AUTH_ACCOUNT_ID"] = &pb.ProviderCredentialValue{Value: "synthetic-static-account"}
}

func credentialSettledReadback() *pb.GetProviderRefreshStatusResponse {
	return &pb.GetProviderRefreshStatusResponse{Credentials: []*pb.ProviderCredentialRefreshStatus{{Provider: "chosen", ProviderId: "source-provider", CredentialKey: "CODEX_AUTH_ACCESS_TOKEN", Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN, Status: "refreshed"}}}
}

func TestOpenShellCredentialUnknownReadbackDoesNotRetrieve(t *testing.T) {
	for _, mode := range []string{"empty", "nil-row", "unknown-status", "unknown-recovery", "missing-key", "duplicate", "provider-name", "provider-id", "strategy", "retry-future", "retry-no-schedule"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			credentialOriginalCodexDeclarations(source)
			source.sourceLost = true
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("lost original retrieval expected")
			}
			readback := credentialSettledReadback()
			switch mode {
			case "empty":
				readback.Credentials = nil
			case "nil-row":
				readback.Credentials = []*pb.ProviderCredentialRefreshStatus{nil}
			case "unknown-status":
				readback.Credentials[0].Status = "unknown-future-state"
			case "unknown-recovery":
				readback.Credentials[0].RecoveryAction = pb.ProviderCredentialRefreshRecoveryAction(999)
			case "missing-key":
				readback.Credentials = []*pb.ProviderCredentialRefreshStatus{{Provider: "chosen", ProviderId: "source-provider", CredentialKey: "CODEX_AUTH_ACCOUNT_ID", Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_STATIC, Status: "configured"}}
			case "duplicate":
				readback.Credentials = append(readback.Credentials, readback.Credentials[0])
			case "provider-name":
				readback.Credentials[0].Provider = "other-provider"
			case "provider-id":
				readback.Credentials[0].ProviderId = "replacement-provider"
			case "strategy":
				readback.Credentials[0].Strategy = pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_CLIENT_CREDENTIALS
			case "retry-future":
				readback.Credentials[0].Status = "error"
				readback.Credentials[0].RecoveryAction = pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_RETRY
				readback.Credentials[0].NextRefreshTime = timestamppb.New(now.Add(time.Minute))
			case "retry-no-schedule":
				readback.Credentials[0].Status = "error"
				readback.Credentials[0].RecoveryAction = pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_RETRY
			}
			source.refreshReadback = readback
			err := credentialDeliver(t, o)
			var waiting *openShellCredentialWaiting
			if !errors.As(err, &waiting) {
				t.Fatalf("incomplete original readback did not remain typed waiting: %v", err)
			}
			if source.retrievals != 1 || target.creates != 0 || !o.custody.state.Credentials[0].SourceReadPending {
				t.Fatal("unknown readback authorized another credential retrieval or lost original uncertainty")
			}
			credentialNoLeaks(t, o)
		})
	}
}

func TestOpenShellCredentialSettledReadbackKeepsStaticNeighbor(t *testing.T) {
	for _, mode := range []string{"mixed-static-account", "static-only-empty-status", "retry-due", "configured"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			credentialOriginalCodexDeclarations(source)
			if mode == "static-only-empty-status" {
				source.profile.Credentials = source.profile.Credentials[1:]
			}
			source.sourceLost = true
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("lost original retrieval expected")
			}
			source.refreshReadback = credentialSettledReadback()
			switch mode {
			case "static-only-empty-status":
				source.refreshReadback.Credentials = nil
			case "configured":
				source.refreshReadback.Credentials[0].Status = "configured"
			case "retry-due":
				source.refreshReadback.Credentials[0].Status = "error"
				source.refreshReadback.Credentials[0].RecoveryAction = pb.ProviderCredentialRefreshRecoveryAction_PROVIDER_CREDENTIAL_REFRESH_RECOVERY_ACTION_RETRY
				source.refreshReadback.Credentials[0].NextRefreshTime = timestamppb.New(now.Add(-time.Second))
			}
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			if source.retrievals != 2 || target.creates != 1 || target.provider.Credentials["CODEX_AUTH_ACCOUNT_ID"] != "synthetic-static-account" {
				t.Fatal("static account required a fabricated refresh owner or settled readback did not continue")
			}
			credentialNoLeaks(t, o)
		})
	}
}

func TestOpenShellCredentialPendingInstallationWaitsInOriginalSession(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "installed", true: "cancelled"}[cancelled], func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			target.sandbox = custodySandbox("existing-vm", "native-existing", pb.SandboxPhase_SANDBOX_PHASE_READY)
			target.sandbox.Spec.Providers = []string{"chosen"}
			target.sandbox.Status.MainProcessInstanceId = "original-main"
			spec, _ := openShellBytes(target.sandbox.Spec)
			requestID, _ := openShellUUID()
			intent, _ := openShellBytes(&pb.CreateSandboxRequest{Name: "existing-vm", RequestId: requestID, WorkspaceScope: o.custody.scope(), Spec: target.sandbox.Spec})
			identity := OpenShellIdentity{OrderID: "existing-order", AttemptID: "existing-attempt", SessionID: "existing-session", SandboxName: "existing-vm", ConfigSource: o.custody.state.Binding.ConfigSource, Sandbox: OpenShellSandboxRef{Gateway: o.custody.state.Binding.Gateway, Workspace: "owned-workspace", SandboxID: "native-existing"}}
			o.custody.state.Records = []OpenShellCustodyRecord{{Task: OpenShellTask{OrderID: identity.OrderID, AttemptID: identity.AttemptID, SessionID: identity.SessionID}, Name: "existing-vm", CreateRequestID: requestID, CreateIntent: intent, Spec: spec, CreateAcknowledged: true, MainProcessInstanceID: "original-main", Worker: OpenShellWorkerRecord{Identity: identity}}}
			if err := o.custody.validateLedger(); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(55 * time.Minute)
			source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			r := &OpenShellRuntime{config: OpenShellRuntimeConfig{Gateway: o}}
			s := newOpenShellSession(r, "waiting-original-session", t.TempDir())
			s.receipt = openShellDispatchReceipt{Version: 1, SessionID: s.id, Phase: "prepared"}
			done := make(chan error, 1)
			go func() {
				release, err := s.waitCredentials(t.Context(), []string{"chosen"})
				if release != nil {
					release()
				}
				done <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for {
				s.mu.Lock()
				phase := s.receipt.Phase
				s.mu.Unlock()
				if phase == "credential_waiting" {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("async pending installation consumed original session: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("original session did not enter credential_waiting")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if cancelled {
				if err := s.Terminate(); err != nil {
					t.Fatal(err)
				}
			} else {
				target.mu.Lock()
				target.installationReady = true
				target.mu.Unlock()
			}
			select {
			case err := <-done:
				if cancelled {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled wait returned %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("original installation wait did not resume or cancel")
			}
			snapshot := o.custody.Snapshot()
			if len(snapshot.Records) != 1 || snapshot.Records[0].Task.SessionID != "existing-session" || s.id != "waiting-original-session" {
				t.Fatal("installation wait created/replayed a VM or replaced either session")
			}
			target.mu.Lock()
			updates := target.updates
			target.mu.Unlock()
			if updates != 1 {
				t.Fatal("pending installation replayed provider mutation")
			}
		})
	}
}
