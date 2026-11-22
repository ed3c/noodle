package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutRemovalPreservesSharedStash(t *testing.T) {
	t.Run("loose", func(t *testing.T) { checkCheckoutRemovalPreservesSharedStash(t, false) })
	t.Run("packed", func(t *testing.T) { checkCheckoutRemovalPreservesSharedStash(t, true) })
}

func checkCheckoutRemovalPreservesSharedStash(t *testing.T, packed bool) {
	t.Helper()
	r, _ := checkoutFixture(t, "absent")
	r.Inventory = FileRef{}
	commonCheckout := filepath.Dir(r.Identity.Repo)
	for _, message := range []string{"first shared stash", "second shared stash"} {
		fixtureWrite(t, filepath.Join(commonCheckout, "code.txt"), []byte(message+"\n"))
		fixtureGit(t, commonCheckout, "stash", "push", "-m", message)
	}
	stash := fixtureGit(t, r.Identity.Path, "stash", "list", "--format=%H")
	if len(strings.Split(stash, "\n")) != 2 {
		t.Fatal("fixture did not preserve two shared stash objects")
	}
	names := []string{"logs/refs/stash"}
	loose := filepath.Join(r.Identity.Repo, "refs", "stash")
	if packed {
		fixtureGit(t, commonCheckout, "pack-refs", "--all")
		if _, err := os.Lstat(loose); !os.IsNotExist(err) {
			t.Fatal("fixture stash ref was not packed", err)
		}
		names = append(names, "packed-refs")
	} else {
		names = append(names, "refs/stash")
	}
	before := map[string][]byte{}
	for _, name := range names {
		path := filepath.Join(r.Identity.Repo, filepath.FromSlash(name))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = raw
	}
	packet, err := PrepareCheckout(r)
	if err != nil {
		t.Fatal("retained common stash prevented original checkout preservation", err)
	}
	if err := BeginCheckoutRemoval(packet); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, r.Identity.Repo, "worktree", "remove", "--", r.Identity.Path)
	if _, err := RecordCheckoutRemoval(packet); err != nil {
		t.Fatal(err)
	}
	if head := fixtureGit(t, commonCheckout, "rev-parse", r.Identity.Ref); head != r.Identity.Head {
		t.Fatal("original task branch changed during checkout removal")
	}
	if after := fixtureGit(t, commonCheckout, "stash", "list", "--format=%H"); after != stash {
		t.Fatal("shared stash object list changed during checkout removal")
	}
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(r.Identity.Repo, filepath.FromSlash(name)))
		if err != nil || string(raw) != string(before[name]) {
			t.Fatal("shared stash ref or reflog changed during checkout removal", name, err)
		}
	}
	if packed {
		if _, err := os.Lstat(loose); !os.IsNotExist(err) {
			t.Fatal("checkout removal recreated the original packed stash as a loose ref", err)
		}
	}
}

func TestCheckoutPreservationRefusesAliasedSharedStashLog(t *testing.T) {
	r, c := checkoutFixture(t, "absent")
	commonCheckout := filepath.Dir(r.Identity.Repo)
	fixtureWrite(t, filepath.Join(commonCheckout, "code.txt"), []byte("stash content\n"))
	fixtureGit(t, commonCheckout, "stash", "push", "-m", "shared stash")
	log := filepath.Join(r.Identity.Repo, "logs", "refs", "stash")
	target := filepath.Join(filepath.Dir(r.Config.Path), "aliased-stash-log")
	if err := os.Rename(log, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, log); err != nil {
		t.Fatal(err)
	}
	if list := fixtureGit(t, r.Identity.Path, "stash", "list", "--format=%H"); list == "" {
		t.Fatal("fixture Git did not observe the aliased stash")
	}
	if _, err := InspectSource(c, r.Identity.Path); err == nil || !strings.Contains(err.Error(), "shared stash ownership requires retention: logs/refs/stash") {
		t.Fatalf("aliased shared stash reflog must require retention, got %v", err)
	}
}

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
	if err = ObserveCheckoutRemovalBeforeReceipt(p); err == nil {
		t.Fatal("pending request with a present checkout qualified as removed")
	}
	fixtureGit(t, r.Identity.Repo, "worktree", "remove", "--", r.Identity.Path)
	if err = ObserveCheckoutRemovalBeforeReceipt(p); err != nil {
		t.Fatalf("exact native removal could not be read after lost acknowledgement: %v", err)
	}
	if _, err = InspectCheckoutRemoval(p); !os.IsNotExist(err) {
		t.Fatalf("read-only recovery wrote a removal receipt: %v", err)
	}
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
