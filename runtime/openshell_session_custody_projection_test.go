package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionProjectionJSONSemantics(t *testing.T) {
	digest := func(raw string) string {
		t.Helper()
		value, err := completionJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		sum, err := completionJSONDigest(value)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	for _, pair := range [][2]string{
		{`{"a":9007199254740993,"b":[1,null,"x"]}`, ` { "b":[1.00e0,null,"\u0078"],"a":900719925474099300e-2 } `},
		{`[-0,1e1000000000,0.0012300,-120]`, `[0.000,10e999999999,123e-5,-1.2e2]`},
	} {
		if digest(pair[0]) != digest(pair[1]) {
			t.Fatal("JSON formatting or equivalent decimal value changed projection")
		}
	}
	for _, pair := range [][2]string{
		{`{"opaque":9007199254740992}`, `{"opaque":9007199254740993}`},
		{`{"opaque":{"a":1}}`, `{"opaque":{"a":2}}`},
		{`{"a":null}`, `{}`},
		{`[1,2]`, `[2,1]`},
	} {
		if digest(pair[0]) == digest(pair[1]) {
			t.Fatal("semantic JSON change disappeared from projection")
		}
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `{"x":{"a":1,"\u0061":2}}`, `{} {}`, `{"x":1e}`, `[1,]`} {
		if _, err := completionJSON([]byte(raw)); err == nil {
			t.Fatal("ambiguous or malformed JSON accepted")
		}
	}
}

func completionCustodyBytes(t *testing.T, path string, change func(map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var root map[string]any
	if err = d.Decode(&root); err != nil {
		t.Fatal(err)
	}
	change(root)
	raw, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCompletionProjectionFreshQualificationAndStrictSources(t *testing.T) {
	r, b, _ := completedReadbackFixture(t)
	profile := r.config.Profile
	path := filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json")
	observed, err := ReadOpenShellCompletedEvidence(t.Context(), profile, r.config.RuntimeDir, r.config.ContextID, b)
	if err != nil {
		t.Fatal(err)
	}
	p := observed.CustodyProjection
	if p == nil || p.Scope != SelectedSessionStopped || p.Schema != 1 || p.Subject != (SessionCustodySubject{r.config.ContextID, b.OrderID, b.SessionID, observed.NativeAttemptID}) || p.ProfileSHA256 != observed.ProfileSHA256 || p.Before != p.After || p.After != observed.Sources["native_custody"] {
		t.Fatal("standalone producer lost exact contract")
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Argv", "Environment", "SealedPayload", "Credentials", "custodyQualification"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("projection exposed private custody input")
		}
	}
	for _, fault := range []string{"Version", "ControlRoot", "Binding", "Blocked", "Handover", "PendingRetained", "duplicate", "shape", "alias", "credential-shape", "opaque-root", "opaque-selected", "native-attempt", "request", "command", "stop-ack", "create-ack", "main-instance", "exec-exit"} {
		t.Run(fault, func(t *testing.T) {
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Error(err)
				}
			})
			changed := completionCustodyBytes(t, path, func(root map[string]any) {
				rows := root["Records"].([]any)
				row := rows[0].(map[string]any)
				switch fault {
				case "Version":
					root["Version"] = 99
				case "ControlRoot":
					root["ControlRoot"] = t.TempDir()
				case "Binding":
					root["Binding"].(map[string]any)["Owner"] = "foreign"
				case "Blocked":
					root["Blocked"] = "unresolved"
				case "Handover":
					root["Handover"] = map[string]any{"path": "/missing", "sha256": strings.Repeat("a", 64)}
				case "PendingRetained":
					root["PendingRetained"] = []any{map[string]any{"Name": "unknown"}}
				case "duplicate":
					root["Records"] = append(rows, row)
				case "shape":
					root["Records"] = "invalid"
				case "alias":
					root["records"] = rows
				case "credential-shape":
					root["Credentials"] = []any{map[string]any{"Provider": "bad"}}
				case "opaque-root":
					root["Opaque"] = json.Number("9007199254740993")
				case "opaque-selected":
					row["Opaque"] = json.Number("9007199254740993")
				case "native-attempt":
					row["Exec"].(map[string]any)["AttemptID"] = "foreign"
				case "request":
					row["Exec"].(map[string]any)["Command"].(map[string]any)["RequestID"] = "foreign"
				case "command":
					row["Exec"].(map[string]any)["Command"].(map[string]any)["Argv"] = []string{"changed"}
				case "stop-ack":
					row["StopAcknowledged"] = false
				case "create-ack":
					row["CreateAcknowledged"] = false
				case "main-instance":
					row["MainProcessInstanceID"] = "foreign"
				case "exec-exit":
					row["Exec"].(map[string]any)["Result"].(map[string]any)["ExitCode"] = 1
				}
			})
			if err = os.WriteFile(path, changed, 0600); err != nil {
				t.Fatal(err)
			}
			if err = VerifyCompletedSessionReadback(observed, profile, r.config.ContextID, b.OrderID, b.SessionID); err == nil {
				t.Fatal("changed original custody accepted")
			}
		})
	}
	for _, name := range []string{"openshell-dispatch.json", "spawn.json", "meta.json", "delivery/result.json", "delivery/changes.patch", "delivery/publish.json", "progress_receipt", "progress_archive_noodle-result.tar", "progress_archive_noodle-session.tar"} {
		t.Run(name, func(t *testing.T) {
			ref := observed.Sources[name]
			before, err := os.ReadFile(ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(ref.Path, before, 0600); err != nil {
					t.Error(err)
				}
			})
			if err = os.WriteFile(ref.Path, append(before, ' '), 0600); err != nil {
				t.Fatal(err)
			}
			if err = VerifyCompletedSessionReadback(observed, profile, r.config.ContextID, b.OrderID, b.SessionID); err == nil {
				t.Fatal("non-custody source drift escaped scoped verifier")
			}
		})
	}
	for _, fault := range []string{"scope", "schema", "subject", "profile", "before", "after", "digest", "missing-source", "unqualified"} {
		t.Run(fault, func(t *testing.T) {
			e := observed
			proof := *p
			e.CustodyProjection = &proof
			switch fault {
			case "scope":
				proof.Scope = "host_zero_workers"
			case "schema":
				proof.Schema++
			case "subject":
				proof.Subject.ContextID = "foreign"
			case "profile":
				proof.ProfileSHA256 = strings.Repeat("a", 64)
			case "before":
				proof.Before.Path = "/foreign/owner.json"
			case "after":
				proof.After.SHA256 = strings.Repeat("a", 64)
			case "digest":
				proof.SelectedRecordSHA256 = "bad"
			case "missing-source":
				e.Sources = map[string]SessionReadbackFileRef{}
			case "unqualified":
				e.custodyQualification = nil
			}
			if err = VerifyCompletedSessionReadback(e, profile, r.config.ContextID, b.OrderID, b.SessionID); err == nil {
				t.Fatal("unqualified or rebound proof accepted")
			}
		})
	}
}

func TestCompletionCustodyQualifiesFullCredentialAndRetainedLedger(t *testing.T) {
	r, b, _ := completedReadbackFixture(t)
	spec := r.config.Profile.Gateway
	var err error
	spec.Binding.StateRoot, err = filepath.EvalSymlinks(spec.Binding.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	spec.ControlRoot, err = filepath.EvalSymlinks(spec.ControlRoot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spec.Binding.StateRoot, "owner.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Error(err)
		}
	})
	var state OpenShellCustodyState
	if err = json.Unmarshal(original, &state); err != nil {
		t.Fatal(err)
	}
	id := state.Records[0].Worker.Identity
	subject := SessionCustodySubject{r.config.ContextID, b.OrderID, b.SessionID, id.AttemptID}
	spec.CredentialSource = &OpenShellCredentialSource{BindingSHA256: strings.Repeat("a", 64)}
	state.Version = 5
	state.Credentials = []OpenShellCredentialDelivery{{Provider: "provider", SourceBinding: spec.CredentialSource.BindingSHA256}}
	writeReadbackJSON(t, path, state)
	before, err := readCompletionCustody(spec, subject, &id)
	if err != nil {
		t.Fatal(err)
	}
	state.Credentials[0].SourceReadPending = true
	writeReadbackJSON(t, path, state)
	after, err := readCompletionCustody(spec, subject, &id)
	if err != nil {
		t.Fatal(err)
	}
	if before.ref == after.ref || before.rootSHA != after.rootSHA || before.selectedSHA != after.selectedSHA {
		t.Fatal("legal credential-row progress changed selected/root projection")
	}
	state.Credentials[0].SourceBinding = strings.Repeat("b", 64)
	writeReadbackJSON(t, path, state)
	if _, err = readCompletionCustody(spec, subject, &id); err == nil {
		t.Fatal("foreign credential source binding accepted")
	}
	state.Credentials[0].SourceBinding = spec.CredentialSource.BindingSHA256
	state.Credentials[0].Pending = &OpenShellCredentialIntent{Kind: "update", RequestID: "unresolved"}
	writeReadbackJSON(t, path, state)
	if _, err = readCompletionCustody(spec, subject, &id); err == nil {
		t.Fatal("malformed credential intent accepted")
	}
	state.Credentials[0].Pending = nil
	retained := otherCompletedCustodyRecord(t, state.Records[0])
	retained.Retained = true
	state.Records = append(state.Records, retained)
	spec.Retained = []OpenShellRetainedSandbox{{retained.Name, retained.Worker.Identity.Sandbox.SandboxID, retained.Worker.Identity.Sandbox.Workspace}}
	writeReadbackJSON(t, path, state)
	before, err = readCompletionCustody(spec, subject, &id)
	if err != nil {
		t.Fatal(err)
	}
	raw := completionCustodyBytes(t, path, func(root map[string]any) {
		root["Records"].([]any)[1].(map[string]any)["Opaque"] = "retained-change"
	})
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	after, err = readCompletionCustody(spec, subject, &id)
	if err != nil {
		t.Fatal(err)
	}
	if before.rootSHA != after.rootSHA || before.selectedSHA != after.selectedSHA || before.retainedSHA == after.retainedSHA {
		t.Fatal("retained drift escaped the original local qualification")
	}
	spec.Retained[0].SandboxID = "foreign"
	if _, err = readCompletionCustody(spec, subject, &id); err == nil {
		t.Fatal("changed original retained receipt set accepted")
	}
}
