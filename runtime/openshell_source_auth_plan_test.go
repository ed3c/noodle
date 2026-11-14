package runtime

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sourceAuthFixture(t *testing.T) (string, string, string, openShellProfileFile) {
	t.Helper()
	original, f := profileFixture(t)
	root := filepath.Dir(original)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(101), Subject: pkix.Name{CommonName: "source CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("source-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	for i, name := range []string{"user", "operator"} {
		client, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(102 + i)), Subject: pkix.Name{OrganizationalUnit: []string{name}}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &client.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(client)
		if err != nil {
			t.Fatal(err)
		}
		write(name+".pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		write(name+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	}
	f.CredentialSource = &openShellCredentialSourceFile{Gateway: "https://127.0.0.1:24681", Workspace: "source-workspace"}
	f.CredentialSource.TLS.CAFile = "source-ca.pem"
	f.CredentialSource.TLS.ServerName = "source.test"
	f.CredentialSource.TLS.CertificateFile = "user.pem"
	f.CredentialSource.TLS.KeyFile = "user.key"
	writeProfileFixture(t, original, f)
	template := filepath.Join(root, "template.json")
	candidate := f
	source := *f.CredentialSource
	source.TLS.CertificateFile, source.TLS.KeyFile = "operator.pem", "operator.key"
	candidate.CredentialSource = &source
	writeProfileFixture(t, template, candidate)
	loaded, err := LoadOpenShellProfile(original)
	if err != nil {
		t.Fatal(err)
	}
	dispatchSHA, err := OpenShellDispatchSHA256(loaded)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := filepath.Join(root, "openshell-dispatch.json")
	d := openShellDispatchReceipt{Version: 1, ContextID: "context", OrderID: "order", SessionID: "session", AttemptID: "native-attempt", ProfileSHA256: dispatchSHA, Target: filepath.Join(root, "worktree"), Phase: "credential_waiting"}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	write("openshell-dispatch.json", raw)
	state := OpenShellCustodyState{Version: 5, ControlRoot: root, Binding: loaded.Gateway.Binding, Credentials: []OpenShellCredentialDelivery{{Provider: "chosen", SourceBinding: loaded.CredentialOrigin.BindingSHA256, SourceProviderID: "source-provider", ProfileID: "original", SourceReadPending: true, Pending: &OpenShellCredentialIntent{Kind: "update", RequestID: "original-pending-id", StartedAt: time.Now(), SealedPayload: bytes.Repeat([]byte{42}, 32)}, TargetReceipts: [][]byte{[]byte("opaque-original-receipt")}}}}
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var opaque map[string]json.RawMessage
	if err = json.Unmarshal(raw, &opaque); err != nil {
		t.Fatal(err)
	}
	opaque["opaque_history"] = json.RawMessage(`{"original":"must remain"}`)
	raw, err = json.Marshal(opaque)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(loaded.Gateway.Binding.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(loaded.Gateway.Binding.StateRoot, "owner.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return original, template, dispatch, f
}

func TestSourceAuthPlanPreservesFrozenPendingAndOpaqueBytes(t *testing.T) {
	original, template, dispatch, _ := sourceAuthFixture(t)
	loaded, err := LoadOpenShellProfile(original)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(loaded.Gateway.Binding.StateRoot, "owner.json")
	before := map[string][]byte{}
	for _, p := range []string{original, template, dispatch, state} {
		before[p], err = os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
	}
	plan, err := InspectSourceAuthPlan(original, template, dispatch, "context", filepath.Dir(original))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "unknown" || plan.Executable || len(plan.Effects) != 0 || plan.SessionID != "session" || plan.OrderID != "order" || plan.FrozenProfileSHA256 == "" || plan.CustodyVersion != 5 || len(plan.Requires) == 0 {
		t.Fatalf("proposal granted effects or lost original identity: %+v", plan)
	}
	if plan.CurrentAccess.BindingSHA256 == plan.ProposedAccess.BindingSHA256 || plan.CurrentAccess.TrustSHA256 != plan.ProposedAccess.TrustSHA256 || len(plan.PreservedPending) != 1 || !plan.PreservedPending[0].SourceReadPending || !plan.PreservedPending[0].TargetMutationPending || plan.PreservedPending[0].TargetReceiptCount != 1 {
		t.Fatalf("auth or pending projection changed: %+v", plan)
	}
	for p, raw := range before {
		after, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatalf("original bytes changed: %s %v", p, err)
		}
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("opaque-original-receipt")) || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatal("proposal exposed original opaque/credential values")
	}
	if err = os.WriteFile(state, append(before[state], ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if err = VerifySessionReadbackFiles(plan.Sources); err == nil {
		t.Fatal("stale custody proposal remained valid")
	}
}

func TestSourceAuthPlanRejectsBusinessTrustAndPrincipalDrift(t *testing.T) {
	for _, kind := range []string{"endpoint", "workspace", "trust", "principal", "frozen-business", "source-binding", "dispatch-context", "control-root", "handover-epoch", "native-attempt"} {
		t.Run(kind, func(t *testing.T) {
			original, template, dispatch, f := sourceAuthFixture(t)
			raw, err := os.ReadFile(template)
			if err != nil {
				t.Fatal(err)
			}
			var candidate openShellProfileFile
			if err = json.Unmarshal(raw, &candidate); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "endpoint":
				candidate.CredentialSource.Gateway = "https://127.0.0.1:24682"
			case "workspace":
				candidate.CredentialSource.Workspace = "other-source"
			case "trust":
				candidate.CredentialSource.TLS.CAFile = "ca.pem"
			case "principal":
				candidate.CredentialSource = f.CredentialSource
			case "frozen-business":
				f.Gateway.Binding.Owner = "different"
				writeProfileFixture(t, original, f)
			case "source-binding":
				loaded, err := LoadOpenShellProfile(original)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(loaded.Gateway.Binding.StateRoot, "owner.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.ReplaceAll(raw, []byte(loaded.CredentialOrigin.BindingSHA256), []byte(strings.Repeat("b", 64)))
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "control-root", "handover-epoch", "native-attempt":
				loaded, err := LoadOpenShellProfile(original)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(loaded.Gateway.Binding.StateRoot, "owner.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var state OpenShellCustodyState
				if err = json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				if kind == "control-root" {
					state.ControlRoot = "/foreign/host"
				} else if kind == "native-attempt" {
					state.Records = []OpenShellCustodyRecord{{Name: "old-native", Task: OpenShellTask{OrderID: "order", SessionID: "session", AttemptID: "conflicting-uuid"}}}
				} else {
					state.Handover = &SessionReadbackFileRef{Path: "/missing/original/epoch", SHA256: strings.Repeat("a", 64)}
				}
				raw, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "dispatch-context":
				raw, err := os.ReadFile(dispatch)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(dispatch, bytes.ReplaceAll(raw, []byte(`"context"`), []byte(`"other-context"`)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeProfileFixture(t, template, candidate)
			plan, err := InspectSourceAuthPlan(original, template, dispatch, "context", filepath.Dir(original))
			if err == nil || plan.Executable || len(plan.Effects) != 0 {
				t.Fatalf("%s drift accepted: %+v %v", kind, plan, err)
			}
		})
	}
}

func TestSourceAuthPlanKeepsV4LedgerAndMissingAcksUnknown(t *testing.T) {
	original, template, dispatch, _ := sourceAuthFixture(t)
	loaded, err := LoadOpenShellProfile(original)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(loaded.Gateway.Binding.StateRoot, "owner.json")
	state := OpenShellCustodyState{Version: 4, ControlRoot: filepath.Dir(original), Binding: loaded.Gateway.Binding}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := InspectSourceAuthPlan(original, template, dispatch, "context", filepath.Dir(original))
	if err != nil || plan.Status != "unknown" || plan.CustodyVersion != 4 || len(plan.PreservedPending) != 0 || plan.Executable {
		t.Fatalf("legacy missing ACK promoted: %+v %v", plan, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("legacy ledger migrated")
	}
}

func TestSourceAuthPlanUsesRegisteredLegacyTemplateCredentialOrigin(t *testing.T) {
	original, _, dispatch, f := sourceAuthFixture(t)
	template, candidate := profileFixture(t)
	root := filepath.Dir(template)
	sourceRoot := filepath.Dir(original)
	candidate.CredentialSource = nil
	candidate.Gateway.Binding.Gateway = f.CredentialSource.Gateway
	candidate.Gateway.Binding.Workspace = f.CredentialSource.Workspace
	candidate.Gateway.TLS = f.CredentialSource.TLS
	candidate.Gateway.TLS.CAFile = filepath.Join(sourceRoot, "source-ca.pem")
	candidate.Gateway.TLS.CertificateFile = filepath.Join(sourceRoot, "operator.pem")
	candidate.Gateway.TLS.KeyFile = filepath.Join(sourceRoot, "operator.key")
	client := filepath.Join(root, "client", "openshell", "gateways", "dedicated")
	for src, dst := range map[string]string{"source-ca.pem": "ca.crt", "operator.pem": "tls.crt", "operator.key": "tls.key"} {
		raw, err := os.ReadFile(filepath.Join(sourceRoot, src))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(client, "mtls", dst), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(map[string]string{"name": "dedicated", "gateway_endpoint": candidate.Gateway.Binding.Gateway, "auth_mode": "mtls"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(client, "metadata.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	writeProfileFixture(t, template, candidate)
	plan, err := InspectSourceAuthPlan(original, template, dispatch, "context", filepath.Dir(original))
	if err != nil || plan.Status != "unknown" || plan.Executable || plan.CurrentAccess.Gateway != plan.ProposedAccess.Gateway || plan.CurrentAccess.BindingSHA256 == plan.ProposedAccess.BindingSHA256 {
		t.Fatalf("registered legacy template was not a Source-only proposal: %s %v", plan.Status, err)
	}
}
