package loop

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/reducer"
)

func TestAdmissionPendingIntentBoundaries(t *testing.T) {
	for _, name := range []string{"later_mailbox", "changed_revision", "multiple_intents", "corrupt_archive", "subject_dispatch", "metadata_identity"} {
		t.Run(name, func(t *testing.T) {
			tc, original := recoveryFixture(t)
			r := InspectAdmission(tc.projectDir, "noodle")
			if r.Status != "recoverable" {
				t.Fatalf("fixture: %+v", r)
			}
			receipt := admissionReceipt{Owner: admissionOwner, Subject: r.Subject, Revision: r.Revision, Proposal: original, Reason: r.Invalid, CreatedAt: time.Now().UTC()}
			if err := persistAdmissionReceipt(tc.runtimeDir, receipt); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "later_mailbox":
				initialProposal(t, tc.loop, r.Revision, "C")
			case "changed_revision":
				recoverySnapshot(t, tc, func(s *reducer.DurableSnapshot) { s.OrderRevision = strings.Repeat("1", 32) })
			case "multiple_intents":
				later := initialProposal(t, tc.loop, strings.Repeat("0", 32), "C")
				// Derive the second subject from a separate valid readback, not from an
				// arbitrary fake hash that an earlier receipt check would reject.
				if err := os.Remove(admissionReceiptPath(tc.runtimeDir, r.Subject.SHA256, r.Revision)); err != nil {
					t.Fatal(err)
				}
				c := InspectAdmission(tc.projectDir, "noodle")
				if c.Status != "recoverable" {
					t.Fatalf("second fixture: %+v", c)
				}
				if err := persistAdmissionReceipt(tc.runtimeDir, admissionReceipt{Owner: admissionOwner, Subject: c.Subject, Revision: c.Revision, Proposal: later, Reason: c.Invalid, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				if err := persistAdmissionReceipt(tc.runtimeDir, receipt); err != nil {
					t.Fatal(err)
				}
			case "corrupt_archive":
				recoveryWrite(t, admissionReceiptPath(tc.runtimeDir, r.Subject.SHA256, r.Revision), []byte("{"))
			case "subject_dispatch":
				recoverySnapshot(t, tc, func(s *reducer.DurableSnapshot) {
					s.EffectLedger = append(s.EffectLedger, reducer.EffectLedgerRecord{EffectID: "dispatch-B", Status: reducer.EffectLedgerPending, Effect: reducer.Effect{EffectID: "dispatch-B", Type: reducer.EffectDispatch, Payload: json.RawMessage(`{"order_id":"B"}`)}})
				})
			case "metadata_identity":
				recoverySession(t, tc, "exited", 99999999)
				recoveryJSON(t, filepath.Join(tc.runtimeDir, "sessions", "worker", "meta.json"), map[string]any{"session_id": "different", "status": "exited"})
			}
			before, _ := os.ReadFile(tc.loop.deps.OrdersNextFile)
			got := InspectAdmission(tc.projectDir, "noodle")
			if got.Status != "refused" || got.Next.Required == "" || got.Next.ProvidedBy == "" || len(got.Next.ReadbackArgv) != 5 {
				t.Fatalf("ambiguous continuation: %+v", got)
			}
			after, _ := os.ReadFile(tc.loop.deps.OrdersNextFile)
			if !bytes.Equal(before, after) {
				t.Fatal("inspection modified mailbox")
			}
		})
	}
}

func TestAdmissionNoProposalEndsRecovery(t *testing.T) {
	tc, _ := recoveryFixture(t)
	r := InspectAdmission(tc.projectDir, "noodle")
	if got := RetireAdmission(tc.projectDir, "noodle", r.Subject.SHA256, r.Revision); got.Status != "retired" {
		t.Fatalf("%+v", got)
	}
	got := InspectAdmission(tc.projectDir, "noodle")
	if got.Status != "no_proposal" || len(got.Next.Argv) != 0 || got.Next.ProvidedBy == "" || got.Revision == "" {
		t.Fatalf("recovery did not end with explicit owner handoff: %+v", got)
	}
}
