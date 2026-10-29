package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func originalTaskAuthorityFixture(t *testing.T, orderID, sessionID, head string, evidence SessionReadbackFileRef) SessionReadbackFileRef {
	t.Helper()
	dir := t.TempDir()
	acceptance := filepath.Join(dir, "outer-acceptance.json")
	writeReadbackJSON(t, acceptance, TaskDeliveryAcceptance{Schema: 1, OrderID: orderID, SessionID: sessionID, CandidateHead: head, Status: "PASS", Conditions: []TaskDeliveryCondition{{Condition: "original acceptance", Status: "PASS", Evidence: []SessionReadbackFileRef{evidence}}}})
	accepted, err := ReadSessionReadbackFile(acceptance, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "task-closure.json")
	writeReadbackJSON(t, path, map[string]any{"order": map[string]any{"order_id": orderID, "status": "completed"}, "reason": "original task owner accepted completed delivery", "diagnostics": map[string]SessionReadbackFileRef{"owner_acceptance": accepted}})
	ref, err := ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestReclamationEvidencePreservesTransitiveAuthorityOutsideCopies(t *testing.T) {
	for _, variant := range []string{"safe", "nested-overlap", "aliased-overlap", "authority-overlap", "wrong-order", "changed-evidence"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "execution")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "evidence.txt")
			if variant == "nested-overlap" || variant == "aliased-overlap" {
				path = filepath.Join(root, "evidence.txt")
			}
			if err := os.WriteFile(path, []byte("original diagnostic evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			ref, err := ReadSessionReadbackFile(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			authority := originalTaskAuthorityFixture(t, "task", "session", strings.Repeat("1", 40), ref)
			if variant == "aliased-overlap" {
				alias := filepath.Join(dir, "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				root = alias
			}
			if variant == "authority-overlap" {
				root = filepath.Dir(authority.Path)
			}
			if variant == "changed-evidence" {
				if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			orderID := "task"
			if variant == "wrong-order" {
				orderID = "foreign-task"
			}
			refs, err := prepareReclamationEvidence(authority, nil, []string{root}, orderID)
			if (err == nil) != (variant == "safe") {
				t.Fatalf("unsafe authority persistence accepted or safe path rejected: %v", err)
			}
			if err == nil && (refs["acceptance_condition_0_0"] != ref || refs["task_closure"] != authority) {
				t.Fatal("transitive source omitted")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("evidence preparation deleted input")
			}
		})
	}
}
