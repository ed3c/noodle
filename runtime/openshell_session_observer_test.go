package runtime

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenShellCompletedEvidenceObserverUsesOnlyGetWithoutOwner(t *testing.T) {
	r, b, f := completedReadbackFixture(t)
	// The running owner remains reserved. A second gateway construction would
	// contend with that owner; this observer never constructs one.
	profile := r.config.Profile
	profile.Gateway.ControlRoot = r.config.Gateway.custody.state.ControlRoot
	custodyPath := filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json")
	before, err := os.ReadFile(custodyPath)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := os.ReadFile(filepath.Join(f.root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ReadOpenShellCompletedEvidence(t.Context(), profile, r.config.RuntimeDir, r.config.ContextID, b)
	if err != nil {
		t.Fatal(err)
	}
	if result.CandidateHead == "" || !result.StopAcknowledged || result.NativePhase != "Stopped" {
		t.Fatal("observer lost original execution facts")
	}
	after, err := os.ReadFile(custodyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("observer changed original business custody")
	}
	eventsAfter, err := os.ReadFile(filepath.Join(f.root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if string(eventsAfter[len(eventsBefore):]) != "get\n" {
		t.Fatalf("observer native effects: %s", eventsAfter[len(eventsBefore):])
	}
	// The original in-process reader still refuses active sessions. The fact
	// helper's lack of an activity claim must never weaken that wrapper.
	r.mu.Lock()
	r.active[b.SessionID] = &openShellSession{}
	r.mu.Unlock()
	if _, err = r.ReadCompletedSession(t.Context(), b); err == nil || !strings.Contains(err.Error(), "has not settled") {
		t.Fatal("ordinary reader lost original active guard", err)
	}
	r.mu.Lock()
	delete(r.active, b.SessionID)
	r.mu.Unlock()
	for _, fault := range []string{"context", "profile", "custody", "source"} {
		t.Run(fault, func(t *testing.T) {
			p := profile
			contextID := r.config.ContextID
			switch fault {
			case "context":
				contextID = "foreign"
			case "profile":
				p.PayloadSHA256 = "changed"
			case "custody":
				p.Gateway.ControlRoot = "foreign"
			case "source":
				p.Gateway.Binding.StateRoot = t.TempDir()
			}
			if _, err := ReadOpenShellCompletedEvidence(t.Context(), p, r.config.RuntimeDir, contextID, b); err == nil {
				t.Fatal("accepted foreign observer source")
			}
			current, _ := os.ReadFile(custodyPath)
			if string(current) != string(before) {
				t.Fatal("failed observer wrote custody")
			}
		})
	}
}

// Use the real file loader before the original gateway is reserved. Dispatch
// then produces its own wrapped command, custody, progress receipt and archives.
func fileProfileObserverFixture(t *testing.T) (*OpenShellRuntime, CompletedSessionBinding, *managedFixture) {
	t.Helper()
	var loaded OpenShellProfile
	r, f, target := openShellRuntimeFixture(t, func(f *managedFixture) {
		filename, doc := profileFixture(t)
		root := filepath.Dir(filename)
		write := func(name string, data []byte) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(root, name), data, 0700); err != nil {
				t.Fatal(err)
			}
		}
		key, err := x509.MarshalPKCS8PrivateKey(f.spec.TLS.Certificate.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		var cert []byte
		for _, der := range f.spec.TLS.Certificate.Certificate {
			cert = append(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
		}
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.spec.TLS.Certificate.Certificate[len(f.spec.TLS.Certificate.Certificate)-1]})
		write("ca.pem", ca)
		write("client.pem", cert)
		write("client.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
		client := filepath.Join(root, "client", "openshell", "gateways", doc.GatewayName)
		for name, data := range map[string][]byte{"ca.crt": ca, "tls.crt": cert, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})} {
			if err := os.WriteFile(filepath.Join(client, "mtls", name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		writeReadbackJSON(t, filepath.Join(client, "metadata.json"), map[string]any{"name": doc.GatewayName, "gateway_endpoint": f.spec.Binding.Gateway, "auth_mode": "mtls", "is_remote": false, "gateway_port": 443})
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		write("transfer", openShellTransferFixtureScript(executable))
		doc.CLI = "transfer"
		write("spec.json", []byte(`{"template":{"image":"image@sha256:example"},"providers":["provider-one"],"command":["sleep","infinity"]}`))
		doc.Gateway.Resident = f.spec.Resident
		doc.Gateway.Binding.StateRoot = f.spec.Binding.StateRoot
		doc.Gateway.Binding.Gateway = f.spec.Binding.Gateway
		doc.Gateway.Binding.Workspace = f.spec.Binding.Workspace
		doc.Gateway.Binding.Owner = f.spec.Binding.Owner
		doc.Gateway.Version = f.spec.Version
		doc.Gateway.ComputeDriver = f.spec.ComputeDriver
		doc.Gateway.ReadinessTimeoutSeconds = 3
		doc.Environment = append(doc.Environment, "NOODLE_TRANSFER_CHILD=1", "NOODLE_TRANSFER_GATEWAY="+doc.GatewayName, "NOODLE_TRANSFER_ENDPOINT="+f.spec.Binding.Gateway, "NOODLE_PROGRESS_FIXTURE_CUSTODY="+filepath.Join(f.spec.Binding.StateRoot, "owner.json"))
		writeProfileFixture(t, filename, doc)
		loaded, err = LoadOpenShellProfile(filename)
		if err != nil {
			t.Fatal(err)
		}
		loaded.Gateway.ControlRoot = f.root
		f.spec = loaded.Gateway
		managedMarker(t, f.root, "completion-observation")
		managedMarker(t, f.root, "provider-ready")
	})
	r.config.Profile = loaded
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "file-backed", Provider: "codex", Model: "fixture", Runtime: "openshell", Prompt: "produce result", WorktreePath: target, AllowPrimaryCheckout: true, EnvVars: map[string]string{"NOODLE_ORDER_ID": "file-backed", "NOODLE_STAGE_INDEX": "0"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("file-backed original dispatch did not settle")
	}
	if h.Outcome().Status != StatusCompleted {
		t.Fatalf("file-backed dispatch: %+v", h.Outcome())
	}
	t.Cleanup(func() {
		if _, err := f.owner.Release("a"); err != nil {
			t.Error(err)
		}
	})
	writeReadbackJSON(t, filepath.Join(r.config.RuntimeDir, "sessions", h.ID(), "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
	return r, CompletedSessionBinding{OrderID: "file-backed", SessionID: h.ID(), WorktreePath: target, Provider: "codex", Model: "fixture"}, f
}

func TestOpenShellObserverPreservesDistinctProducerDigests(t *testing.T) {
	r, b, f := fileProfileObserverFixture(t)
	profile := r.config.Profile
	ref, err := ReadSessionReadbackFile(profile.SourcePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyOpenShellProfileSource(profile, ref); err != nil {
		t.Fatal(err)
	}
	var dispatch openShellDispatchReceipt
	if _, err = ReadSessionReadbackFile(filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "openshell-dispatch.json"), &dispatch); err != nil {
		t.Fatal(err)
	}
	var custody OpenShellCustodyState
	if _, err = ReadSessionReadbackFile(filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json"), &custody); err != nil {
		t.Fatal(err)
	}
	if len(custody.Records) != 1 {
		t.Fatal("original fixture custody ambiguous")
	}
	wrapped := custody.Records[0].Exec.Command
	_, binding, err := unwrapOpenShellProgress(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	wrappedSHA, err := OpenShellCommandSHA256(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if ref.SHA256 == profile.SourceSHA256 || dispatch.ProfileSHA256 == profile.SourceSHA256 || binding.CommandSHA256 == wrappedSHA || dispatch.Progress.Identity.CommandSHA256 != binding.CommandSHA256 {
		t.Fatal("fixture collapsed distinct original producer digest domains")
	}
	before, _ := os.ReadFile(filepath.Join(f.root, "events"))
	got, err := ReadOpenShellCompletedEvidence(t.Context(), profile, r.config.RuntimeDir, r.config.ContextID, b)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileSHA256 != dispatch.ProfileSHA256 || got.CommandSHA256 != wrappedSHA || got.CommandSHA256 == dispatch.Progress.Identity.CommandSHA256 {
		t.Fatal("observer changed original digest semantics")
	}
	after, _ := os.ReadFile(filepath.Join(f.root, "events"))
	if string(after[len(before):]) != "get\n" {
		t.Fatal("observer did more than Get")
	}
}
