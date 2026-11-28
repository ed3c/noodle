package admission

import (
	"path/filepath"
	"testing"
)

func TestRejectedReviewSettlementRequiresOriginalReleasedQuotaAndExactNonce(t *testing.T) {
	b, _, lease, proof := settlementFixture(t)
	proof.CheckoutOperation = RetainedRejectedReviewCheckout
	proof.Sources = map[string]MachineSettlementFile{}
	for _, role := range []string{"canonical_snapshot", "rejected_review_events", "rejected_review_release", "native_custody"} {
		proof.Sources[role] = emptyRetirementFile(t, filepath.Join(proof.Binding.ProjectDir, role+".json"), []byte(role))
	}
	release := proof.Sources["rejected_review_release"]
	if got, err := ReadRejectedReviewMachineSettlement(b.machine.dir, lease, release); err != nil || got != nil {
		t.Fatalf("original held lease was not observed: %+v %v", got, err)
	}
	nonce, err := b.shared.acquire(lease.ContextID, b.maximum, false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SettleMachineSession(lease, proof); err == nil {
		t.Fatal("settled while original shared worker quota remained active")
	}
	if err := b.shared.release(nonce); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.SettleMachineSession(lease, proof); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ReadRejectedReviewMachineSettlement(b.machine.dir, lease, release); err != nil || got == nil || got.Binding != proof.Binding || got.CheckoutOperation != proof.CheckoutOperation {
		t.Fatalf("original retained checkout settlement missing: %+v %v", got, err)
	}
	proof.Sources["rejected_review_release"] = proof.Sources["canonical_snapshot"]
	if err := b.SettleMachineSession(lease, proof); err == nil {
		t.Fatal("replay accepted another release receipt")
	}
	lease.ID += "-foreign"
	if got, err := ReadRejectedReviewMachineSettlement(b.machine.dir, lease, release); err == nil || got != nil {
		t.Fatalf("another original nonce claimed settlement: %+v %v", got, err)
	}
}
