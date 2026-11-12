package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

func reopenCredentialOwner(t *testing.T, original *OpenShellGateway) *OpenShellGateway {
	t.Helper()
	binding := original.custody.state.Binding
	original.stopCredentials()
	if err := original.custody.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := newOpenShellConnection(binding.Gateway, original.spec.TLS)
	if err != nil {
		t.Fatal(err)
	}
	c, err := reserveOpenShellCustodyWithCredentials(binding, conn, nil, filepath.Dir(binding.StateRoot), original.spec.CredentialSource)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	err = c.initialize(t.Context())
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	source, err := newOpenShellConnection(original.spec.CredentialSource.Gateway, original.spec.CredentialSource.TLS)
	if err != nil {
		t.Fatal(err)
	}
	owner := &OpenShellGateway{spec: original.spec, custody: c, credentialConnection: source, credentialClient: pb.NewOpenShellClient(source), credentialNow: original.credentialNow}
	t.Cleanup(func() { owner.stopCredentials(); _ = c.Close() })
	return owner
}

func TestOpenShellCredentialOrphanPendingReplaysOnlyOriginalIntent(t *testing.T) {
	for _, kind := range []string{"import", "create"} {
		t.Run(kind, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			target.lost = kind
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("original lost native reply expected")
			}
			pending := o.custody.Snapshot().Credentials[0].Pending
			originalID := pending.RequestID
			originalGets := source.retrievals
			recovered := reopenCredentialOwner(t, o)
			*now = now.Add(11 * time.Second)
			recovered.credentialTick(t.Context())
			state := recovered.custody.Snapshot()
			if state.Credentials[0].Pending != nil {
				t.Fatal("orphan original intent was not read back")
			}
			if len(state.Records) != 0 || source.retrievals != originalGets || target.updates != 0 || target.requests[originalID] == nil {
				t.Fatal("orphan recovery minted credentials, created worker, or changed native ID")
			}
			if kind == "import" && target.creates != 0 {
				t.Fatal("import ACK incorrectly advanced into fresh ProviderCreate")
			}
			*now = now.Add(55 * time.Minute)
			recovered.credentialTick(t.Context())
			if source.retrievals != originalGets || target.updates != 0 {
				t.Fatal("acknowledged orphan without demand renewed credentials")
			}
		})
	}
}

func TestOpenShellCredentialOrphanSourceReadbackDoesNotMintOrClearUnknown(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	source.sourceLost = true
	if err := credentialDeliver(t, o); err == nil {
		t.Fatal("original source reply loss expected")
	}
	beforeGets, beforeReadbacks := source.gets, source.readbacks
	recovered := reopenCredentialOwner(t, o)
	*now = now.Add(11 * time.Second)
	recovered.credentialTick(t.Context())
	state := recovered.custody.Snapshot()
	if source.gets <= beforeGets || source.readbacks <= beforeReadbacks {
		t.Fatal("orphan source uncertainty received no original readback")
	}
	if source.retrievals != 1 || target.creates != 0 || len(state.Records) != 0 || !state.Credentials[0].SourceReadPending {
		t.Fatal("orphan source recovery minted, created, or cleared unknown effects")
	}
	if !strings.Contains(state.Credentials[0].LastError, "acknowledgement") {
		t.Fatal("orphan original ACK uncertainty not diagnosed")
	}
	if err := recovered.custody.Quiesce(t.Context()); err != nil {
		t.Fatalf("source-only unknown pinned unrelated target gateway: %v", err)
	}
	if !recovered.custody.Snapshot().Credentials[0].SourceReadPending || source.retrievals != 1 || target.creates != 0 {
		t.Fatal("release cleared source unknown or created fresh effects")
	}
}

func TestOpenShellCredentialQuiesceRefusesPersistedUnknown(t *testing.T) {
	for _, kind := range []string{"import", "create"} {
		t.Run(kind, func(t *testing.T) {
			o, _, target, _ := credentialFixture(t)
			target.lost = kind
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("unknown original effect expected")
			}
			recovered := reopenCredentialOwner(t, o)
			if err := recovered.custody.Quiesce(t.Context()); err == nil {
				t.Fatal("persisted unknown passed quiesce with zero local RPCs and zero VMs")
			}
		})
	}
}

func TestOpenShellCredentialSourceOnlyUnknownAllowsLastRelease(t *testing.T) {
	fixture, source, target, _ := credentialFixture(t)
	f := newManagedFixture(t, true)
	f.spec.CredentialSource = fixture.spec.CredentialSource
	o := f.open(t)
	c, err := o.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.client = credentialProviderClient{OpenShellClient: c.client, provider: fixture.custody.client}
	c.mu.Unlock()
	source.mu.Lock()
	source.sourceLost = true
	source.mu.Unlock()
	if _, err := o.EnsureCredentials(t.Context(), []string{"chosen"}); err == nil {
		t.Fatal("source reply loss expected")
	}
	state := c.Snapshot()
	if len(state.Records) != 0 || len(state.Credentials) != 1 || !state.Credentials[0].SourceReadPending || state.Credentials[0].Pending != nil {
		t.Fatal("fixture did not isolate source-only uncertainty")
	}
	if _, err := o.Release("a"); err != nil {
		t.Fatalf("source-only readback pinned target last lease: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.residents.WaitClosed(ctx); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.Snapshot().Credentials[0].SourceReadPending || source.retrievals != 1 || target.creates != 0 {
		t.Fatal("target release cleared source unknown, fetched fresh credentials, or created provider/worker")
	}
	// This independent connection still reads the original source after Target
	// exits. Releasing Target neither stops nor adopts Source's refresh actor.
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: "source-workspace"}}
	status, err := fixture.credentialClient.GetProviderRefreshStatus(t.Context(), &pb.GetProviderRefreshStatusRequest{WorkspaceScope: scope, Provider: "chosen"})
	if err != nil || len(status.GetCredentials()) == 0 {
		t.Fatal("target release affected source gateway service")
	}
}
