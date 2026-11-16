package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckoutRemovalPreservesDriftedVerificationCopy(t *testing.T) {
	r, _ := checkoutFixture(t, "absent")
	r.Inventory = FileRef{} // Native Worktree absence, never a fake Test inventory.
	p, err := PrepareCheckout(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = BeginCheckoutRemoval(p); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, r.Identity.Repo, "worktree", "remove", "--", r.Identity.Path)
	if _, err = RecordCheckoutRemoval(p); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(p.Recovery, ".git", "foreign-consumer-output")
	if err = os.WriteFile(foreign, []byte("preserve this new owner data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = ReleaseCheckoutVerification(p); err == nil {
		t.Fatal("drifted producer copy was recursively removed")
	}
	if _, err = os.Stat(foreign); err != nil {
		t.Fatal("foreign data was not retained", err)
	}
}
