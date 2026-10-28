package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/filex"
)

func retainedRecoveryFixture(t *testing.T) (OpenShellCustodyBinding, OpenShellTLS, []OpenShellRetainedSandbox, []byte) {
	t.Helper()
	g, binding, security, _ := custodyFixture(t)
	refs := []OpenShellRetainedSandbox{{"one", "one-id", binding.Workspace}, {"two", "two-id", binding.Workspace}}
	g.mutate(func() {
		for _, ref := range refs {
			g.sandboxes[ref.Name] = custodySandbox(ref.Name, ref.SandboxID, pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
		}
	})
	conn, err := newOpenShellConnection(binding.Gateway, security)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := reserveOpenShellCustody(binding, conn, refs)
	if err != nil {
		t.Fatal(err)
	}
	binding = owner.Snapshot().Binding
	_ = owner.block(retainedInitializationBlock)
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(binding.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	return binding, security, refs, raw
}

func TestOpenShellRetainedRecoveryInspectApplyAndAudit(t *testing.T) {
	binding, security, refs, before := retainedRecoveryFixture(t)
	inspected, err := RecoverOpenShellRetainedInitialization(binding, refs, retainedRecoveryHash(before), false)
	if err != nil || inspected.Status != "eligible" {
		t.Fatalf("inspect: %+v %v", inspected, err)
	}
	raw, _ := os.ReadFile(filepath.Join(binding.StateRoot, "owner.json"))
	if !bytes.Equal(before, raw) {
		t.Fatal("inspect changed owner")
	}
	if _, err = os.Stat(inspected.EvidencePath); !os.IsNotExist(err) {
		t.Fatal("inspect wrote recovery evidence")
	}
	applied, err := RecoverOpenShellRetainedInitialization(binding, refs, inspected.BeforeSHA256, true)
	if err != nil || applied.Status != "applied" || applied.AfterSHA256 != inspected.AfterSHA256 {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	proof, err := os.ReadFile(applied.EvidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var audit retainedRecoveryAudit
	if err = json.Unmarshal(proof, &audit); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(binding.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(audit.Before, before) || !bytes.Equal(audit.After, after) || retainedRecoveryHash(after) != applied.AfterSHA256 {
		t.Fatal("original/recovered evidence lost")
	}
	var previous, current OpenShellCustodyState
	_ = json.Unmarshal(before, &previous)
	_ = json.Unmarshal(after, &current)
	previous.Blocked = ""
	if !reflect.DeepEqual(previous, current) {
		t.Fatal("recovery changed more than Blocked")
	}
	conn, err := newOpenShellConnection(binding.Gateway, security)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := reserveOpenShellCustody(binding, conn, refs)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if owner.Snapshot().Blocked != "" || len(owner.Snapshot().PendingRetained) != 2 {
		t.Fatal("recovery accepted native state or discarded pending receipts")
	}
	if err = owner.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(owner.Snapshot().Records) != 2 || len(owner.Snapshot().PendingRetained) != 0 {
		t.Fatal("recovery blocked normal initialization")
	}
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err = OpenOpenShellCustody(t.Context(), binding, security, refs)
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoverOpenShellRetainedInitialization(binding, refs, applied.AfterSHA256, true); err == nil {
		t.Fatal("recovery automatically repeated")
	}
}

func TestOpenShellRetainedRecoveryRejectsOtherStates(t *testing.T) {
	for _, change := range []string{"hash", "binding", "other-block", "records", "empty-pending", "duplicate", "drop", "add", "replace", "ledger", "owned", "unknown-field"} {
		t.Run(change, func(t *testing.T) {
			binding, security, refs, before := retainedRecoveryFixture(t)
			var state OpenShellCustodyState
			_ = json.Unmarshal(before, &state)
			switch change {
			case "binding":
				binding.Owner = "other"
			case "other-block":
				state.Blocked = "native identity changed"
			case "records":
				state.Records = []OpenShellCustodyRecord{{Name: "worker"}}
			case "empty-pending":
				state.PendingRetained = nil
			case "duplicate":
				refs[1] = refs[0]
			case "drop":
				refs = refs[:1]
			case "add":
				refs = append(refs, OpenShellRetainedSandbox{"third", "third-id", binding.Workspace})
			case "replace":
				refs[0].SandboxID = "different"
			case "ledger":
				state.PendingRetained[0].Workspace = "foreign"
				refs[0].Workspace = "foreign"
			}
			before, _ = json.Marshal(state)
			if change == "unknown-field" {
				before = append([]byte(`{"Unexpected":true,`), before[1:]...)
			}
			path := filepath.Join(state.Binding.StateRoot, "owner.json")
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			expected := retainedRecoveryHash(before)
			if change == "hash" {
				expected = retainedRecoveryHash([]byte("stale"))
			}
			if change == "owned" {
				conn, err := newOpenShellConnection(binding.Gateway, security)
				if err != nil {
					t.Fatal(err)
				}
				owner, err := reserveOpenShellCustody(binding, conn, refs)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
			}
			if _, err := RecoverOpenShellRetainedInitialization(binding, refs, expected, true); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected recovery changed ledger")
			}
		})
	}
}

func TestOpenShellRetainedRecoveryWriteFailuresKeepPendingGuard(t *testing.T) {
	for step := 1; step <= 3; step++ {
		for _, afterRename := range []bool{false, true} {
			t.Run(fmt.Sprintf("write-%d/after-%v", step, afterRename), func(t *testing.T) {
				binding, security, refs, before := retainedRecoveryFixture(t)
				calls := 0
				write := func(path string, data []byte) error {
					calls++
					if calls == step && !afterRename {
						return errors.New("injected pre-write failure")
					}
					if err := filex.WriteFileAtomicDurable(path, data); err != nil {
						return err
					}
					if calls == step {
						return errors.New("injected post-rename durability failure")
					}
					return nil
				}
				result, err := recoverOpenShellRetainedInitialization(binding, refs, retainedRecoveryHash(before), true, write)
				if err == nil || result.Status != "unknown_effect" || calls != step {
					t.Fatalf("failure retried/claimed success: %+v %v calls=%d", result, err, calls)
				}
				conn, err := newOpenShellConnection(binding.Gateway, security)
				if err != nil {
					t.Fatal(err)
				}
				owner, err := reserveOpenShellCustody(binding, conn, refs)
				if owner != nil {
					defer owner.Close()
				}
				if step == 1 && !afterRename {
					if err != nil || owner.Snapshot().Blocked != retainedInitializationBlock {
						t.Fatal("pre-evidence failure lost original rejection")
					}
				} else {
					if err == nil {
						t.Fatal("pending recovery allowed reservation")
					}
					proof, readErr := os.ReadFile(result.EvidencePath)
					if readErr != nil {
						t.Fatal(readErr)
					}
					var audit retainedRecoveryAudit
					if err = json.Unmarshal(proof, &audit); err != nil || !bytes.Equal(audit.Before, before) {
						t.Fatal("failure lost original evidence")
					}
				}
			})
		}
	}
}

func TestOpenShellRetainedRecoveryRejectsDamagedEvidence(t *testing.T) {
	for _, damage := range []string{"audit", "completion", "missing-owner", "missing-audit"} {
		t.Run(damage, func(t *testing.T) {
			binding, security, refs, before := retainedRecoveryFixture(t)
			if _, err := RecoverOpenShellRetainedInitialization(binding, refs, retainedRecoveryHash(before), true); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "audit":
				if err := os.WriteFile(filepath.Join(binding.StateRoot, retainedRecoveryAuditFile), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "completion":
				if err := os.WriteFile(filepath.Join(binding.StateRoot, retainedRecoveryCompleteFile), []byte(`{"AuditSHA256":"wrong"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-owner":
				if err := os.Remove(filepath.Join(binding.StateRoot, "owner.json")); err != nil {
					t.Fatal(err)
				}
			case "missing-audit":
				if err := os.Remove(filepath.Join(binding.StateRoot, retainedRecoveryAuditFile)); err != nil {
					t.Fatal(err)
				}
			}
			conn, err := newOpenShellConnection(binding.Gateway, security)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := reserveOpenShellCustody(binding, conn, refs)
			if owner != nil {
				defer owner.Close()
			}
			if err == nil {
				t.Fatal("damaged recovery evidence allowed custody")
			}
		})
	}
}
