package runtime

import (
	"slices"
	"strings"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const codexAccountKey = "CODEX_AUTH_ACCOUNT_ID"

func useCodexCredentialProfile(source *credentialNative) {
	source.profile.Id = "codex"
	source.provider.Type = "codex"
	source.profile.Credentials[0].Name = "access_token"
	source.profile.Credentials = append(source.profile.Credentials,
		&pb.ProviderProfileCredential{Name: "refresh_token", EnvVars: []string{codexRefreshKey}},
		&pb.ProviderProfileCredential{Name: "account_id", EnvVars: []string{codexAccountKey}, Required: true, AuthStyle: "header", HeaderName: "chatgpt-account-id"},
		&pb.ProviderProfileCredential{Name: "id_token", EnvVars: []string{"CODEX_AUTH_ID_TOKEN"}},
	)
	source.values[codexAccountKey] = &pb.ProviderCredentialValue{Value: "synthetic-account"}
}

func TestOpenShellCredentialCodexProjectsDormantRefreshAlias(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	useCodexCredentialProfile(source)
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	if source.retrievals != 1 || target.creates != 1 || target.imports != 1 ||
		len(target.provider.Credentials) != 2 || target.provider.Credentials[codexAccountKey] != "synthetic-account" {
		t.Fatal("Codex runtime credentials were not delivered exactly once")
	}
	if len(source.profile.Credentials) != 4 || !slices.Equal(source.profile.Credentials[1].EnvVars, []string{codexRefreshKey}) ||
		len(target.profile.Credentials) != 3 || target.profile.Credentials[1].GetHeaderName() != "chatgpt-account-id" {
		t.Fatal("source profile was changed or account header was lost")
	}
	for _, credential := range target.profile.Credentials {
		if credential.Refresh != nil || slices.Contains(credential.EnvVars, codexRefreshKey) {
			t.Fatal("refresh authority reached the target profile")
		}
	}
	*now = now.Add(55 * time.Minute)
	source.values[codexAccessKey] = &pb.ProviderCredentialValue{Value: "synthetic-next-access", ExpirationTime: timestamppb.New(now.Add(time.Hour))}
	if err := credentialDeliver(t, o); err != nil || target.imports != 1 || target.updates != 1 {
		t.Fatal("Codex refresh did not reuse the original external profile", err)
	}
	credentialNoLeaks(t, o)
}

func TestOpenShellCredentialCodexRefusesRefreshAuthority(t *testing.T) {
	for _, mode := range []string{"foreign", "mixed", "required", "grant", "stored", "handle", "output", "no-owner", "refresh-owner"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, _ := credentialFixture(t)
			useCodexCredentialProfile(source)
			access, alias := source.profile.Credentials[0], source.profile.Credentials[1]
			switch mode {
			case "foreign":
				source.profile.Id, source.provider.Type = "other", "other"
			case "mixed":
				alias.EnvVars = append(alias.EnvVars, "OTHER_KEY")
			case "required":
				alias.Required = true
			case "grant":
				alias.TokenGrant = &pb.ProviderCredentialTokenGrant{}
			case "stored":
				source.values[codexRefreshKey] = &pb.ProviderCredentialValue{Value: "synthetic-forbidden-value"}
			case "handle":
				source.provider.CredentialHandles = map[string]*dm.CredentialHandle{codexRefreshKey: {}}
			case "output":
				access.Refresh.AdditionalOutputs = []*pb.ProviderCredentialRefreshOutput{{Output: "refresh_token", Credential: "refresh_token"}}
			case "no-owner":
				source.refreshReadback = &pb.GetProviderRefreshStatusResponse{}
			case "refresh-owner":
				source.refreshReadback = &pb.GetProviderRefreshStatusResponse{Credentials: []*pb.ProviderCredentialRefreshStatus{
					{Provider: "chosen", ProviderId: "source-provider", CredentialKey: codexAccessKey, Strategy: codexOAuth, Status: "refreshed"},
					{Provider: "chosen", ProviderId: "source-provider", CredentialKey: codexRefreshKey, Strategy: codexOAuth, Status: "configured"},
				}}
			}
			if err := credentialDeliver(t, o); err == nil || source.retrievals != 0 || target.imports != 0 || target.creates != 0 {
				t.Fatal("refresh authority readback did not prevent credential retrieval and target effects", err)
			}
		})
	}
}

func TestOpenShellCredentialCodexOrphanReadbackPreservesUncertainty(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	useCodexCredentialProfile(source)
	source.sourceLost = true
	if err := credentialDeliver(t, o); err == nil || source.retrievals != 1 {
		t.Fatal("original credential retrieval did not lose its reply", err)
	}
	beforeReadbacks := source.readbacks
	recovered := reopenCredentialOwner(t, o)
	*now = now.Add(11 * time.Second)
	recovered.credentialTick(t.Context())
	rows := recovered.custody.Snapshot().Credentials
	if len(rows) != 1 || !rows[0].SourceReadPending || !strings.Contains(rows[0].LastError, "acknowledgement") ||
		source.readbacks <= beforeReadbacks || source.retrievals != 1 || target.creates != 0 {
		t.Fatal("original Codex uncertainty was lost or triggered another credential retrieval", rows)
	}
}

func TestOpenShellCredentialCodexRejectsRefreshHandleDrift(t *testing.T) {
	o, source, target, _ := credentialFixture(t)
	useCodexCredentialProfile(source)
	source.afterRetrieval = func(source *credentialNative) {
		source.provider.CredentialHandles = map[string]*dm.CredentialHandle{codexRefreshKey: {}}
	}
	if err := credentialDeliver(t, o); err == nil || source.retrievals != 1 || target.creates != 0 {
		t.Fatal("late refresh handle did not stop original delivery", err)
	}
	if err := credentialDeliver(t, o); err == nil || source.retrievals != 1 || target.creates != 0 {
		t.Fatal("new refresh handle triggered another retrieval", err)
	}
}
