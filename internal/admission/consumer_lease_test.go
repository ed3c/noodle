package admission

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/poteto/noodle/internal/procx"
)

func TestNativeConsumerConcurrentAcquireHasOneChild(t *testing.T) {
	r, b, spec := residentFixture(t)
	c := nativeTestConsumer(t, "native")
	var wg sync.WaitGroup
	results := make(chan Resident, 12)
	failures := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			row, err := r.AcquireConsumer(spec, c, b)
			if err != nil {
				failures <- err
			}
			results <- row
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first Resident
	for row := range results {
		if first.ID == "" {
			first = row
		}
		if row.PID != first.PID || len(row.Contexts) != 1 {
			t.Fatal("parallel acquire duplicated child or lease", row)
		}
	}
	if _, err := r.ReleaseConsumer(first.ID, c, first.ConsumerLeases[0].ID); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, first.ID)
}

func TestNativeConsumerRegistersInOriginalNonTestHostDirectory(t *testing.T) {
	r, b, spec := residentFixture(t)
	dir, err := os.MkdirTemp("", "swap-original-host-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	r.path = filepath.Join(dir, "residents.json")
	r.fixtures, err = OpenFixtureRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := nativeTestConsumer(t, "native-app")
	v, err := r.AcquireConsumer(spec, c, b)
	if v.ID != "" && len(v.ConsumerLeases) > 0 {
		defer func() {
			if _, e := r.ReleaseConsumer(v.ID, c, v.ConsumerLeases[0].ID); e != nil {
				t.Error(e)
			}
			awaitResidentExit(t, r, v.ID)
		}()
	}
	if err != nil {
		t.Fatal("original Host log is not a Go test directory:", err)
	}
	rows, err := r.FixtureRecoveries().Snapshot()
	if err != nil || len(rows) != 1 || rows[0].Candidate.PID != v.PID || !rows[0].Candidate.ConsumerManaged {
		t.Fatalf("native original owner registration missing: %+v %v", rows, err)
	}
	for _, kind := range []string{"historical", "missing-native", "wrong-log"} {
		candidate := rows[0].Candidate
		switch kind {
		case "historical":
			candidate.Origin = "historical"
			candidate.Native = nil
			candidate.Owner = nil
		case "missing-native":
			candidate.Native = nil
		case "wrong-log":
			candidate.LogPath = filepath.Join(dir, "resident-foreign.log")
		}
		if err := validateFixtureCandidate(candidate); err == nil {
			t.Error("borrowed original owner proof accepted:", kind)
		}
	}
}

func TestNativeConsumerRefusesFixturePurposeBeforeLaunch(t *testing.T) {
	r, b, spec := residentFixture(t)
	dir, err := os.MkdirTemp("", "swap-no-test-purpose-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	spec.Directory = dir
	if _, err = r.AcquireConsumer(spec, nativeTestConsumer(t, "native-app"), b); err == nil {
		t.Fatal("unregistered fixture purpose launched")
	}
	alias := filepath.Join(t.TempDir(), "borrowed-purpose")
	if err = os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	spec.Directory = alias
	if _, err = r.AcquireConsumer(spec, nativeTestConsumer(t, "native-app"), b); err == nil {
		t.Fatal("symlink borrowed a test purpose")
	}
	rows, err := r.Snapshot()
	if err != nil || len(rows) != 0 {
		t.Fatalf("refusal left resident intent: %+v %v", rows, err)
	}
}

func TestNativeWorktreesShareRepositoryButKeepIdentity(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(t.TempDir(), "other")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git fixture %v: %s %v", args, raw, err)
		}
	}
	git("init")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@local")
	if err := os.WriteFile(filepath.Join(root, "content"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "content")
	git("commit", "-m", "Create isolated identity fixture")
	git("worktree", "add", "--detach", sibling, "HEAD")
	a, err := ObserveWorktree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ObserveWorktree(context.Background(), sibling)
	if err != nil {
		t.Fatal(err)
	}
	if a.Root == b.Root || a.GitDir == b.GitDir || a.CommonDir != b.CommonDir || a.HEAD != b.HEAD {
		t.Fatalf("worktree identity collapsed: %+v %+v", a, b)
	}
	if err := os.WriteFile(filepath.Join(sibling, "content"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(a.Root, "content"))
	if err != nil || string(raw) != "one" {
		t.Fatal("worktree file state crossed", string(raw), err)
	}
}

func nativeTestConsumer(t *testing.T, connection string) ResidentConsumer {
	t.Helper()
	p, err := procx.ObserveNativeIdentity(os.Getpid())
	if err != nil {
		t.Skipf("native generation unavailable: %v", err)
	}
	return ResidentConsumer{Connection: connection, SessionID: connection, ContextID: "a", RequestID: "original", Worktree: WorktreeIdentity{Root: "/worktree/one", GitDir: "/git/worktrees/one"}, Process: p, verified: true}
}

func TestNativeConsumerLeasesSeparateUsersInOneContext(t *testing.T) {
	r, b, spec := residentFixture(t)
	a := nativeTestConsumer(t, "one")
	other := nativeTestConsumer(t, "two")
	first, err := r.AcquireConsumer(spec, a, b)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.AcquireConsumer(spec, a, b)
	if err != nil || again.PID != first.PID || len(again.ConsumerLeases) != 1 {
		t.Fatalf("repeat duplicated launch or lease: %+v %v", again, err)
	}
	second, err := r.AcquireConsumer(spec, other, b)
	if err != nil || second.PID != first.PID || len(second.Contexts) != 2 {
		t.Fatalf("independent native lease absent: %+v %v", second, err)
	}
	leaseA := second.ConsumerLeases[0].ID
	leaseB := second.ConsumerLeases[1].ID
	if _, err = r.ReleaseConsumer(first.ID, other, leaseA); err == nil {
		t.Fatal("foreign consumer released lease")
	}
	if err = r.ReleaseContext("a"); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.Snapshot()
	if len(rows[0].Contexts) != 2 {
		t.Fatal("context event ended independent native consumers", rows)
	}
	if err = r.ReconcileConsumerLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	released, err := r.ReleaseConsumer(first.ID, a, leaseA)
	if err != nil || released.State != "running" || len(released.Contexts) != 1 {
		t.Fatalf("one caller interrupted another: %+v %v", released, err)
	}
	if _, err = r.AcquireConsumer(spec, a, b); err == nil {
		t.Fatal("terminal lease was silently reopened")
	}
	if _, err = r.ReleaseConsumer(first.ID, other, leaseB); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, first.ID)
	if got := b.Snapshot().Allocations; got != 0 {
		t.Fatal("native last release retained quota", got)
	}
}

func TestNativeConsumersCannotAuthorizeUnsupportedService(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Argv = []string{spec.Argv[0], "-test.run=^UnregisteredService$"}
	if _, err := r.Acquire(spec, "a", b); err == nil {
		t.Fatal("name/network declaration authorized sharing")
	}
	if _, err := r.AcquireConsumer(spec, nativeTestConsumer(t, "one"), b); err == nil {
		t.Fatal("consumer metadata authorized unsupported I/O")
	}
	if rows, _ := r.Snapshot(); len(rows) != 0 || b.Snapshot().Allocations != 0 {
		t.Fatal("refusal created service state", rows)
	}
}

func TestLostOwnerDoesNotEndIndependentNativeConsumers(t *testing.T) {
	c := nativeTestConsumer(t, "one")
	c.ID = "native-lease"
	c.State = "active"
	dir := t.TempDir()
	candidate := FixtureCandidate{ResidentID: "fixture", PID: 42, Started: "original", LogPath: filepath.Join(dir, "resident-test.log"), ConsumerManaged: true}
	v := &Resident{PID: 42, Started: "original", TestFixture: true, Contexts: []string{c.ID}, ConsumerLeases: []ResidentConsumer{c}}
	write := func() {
		raw, err := json.Marshal(map[string]*Resident{"fixture": v})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "residents.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if err := fixtureConsumersEnded(candidate); err == nil {
		t.Fatal("dead owner borrowed live consumer lease")
	}
	v.ConsumerLeases[0].State = "released"
	v.Contexts = nil
	write()
	if err := fixtureConsumersEnded(candidate); err != nil {
		t.Fatal(err)
	}
	v.Contexts = []string{"unattributed"}
	write()
	if err := fixtureConsumersEnded(candidate); err == nil {
		t.Fatal("unknown lease treated as zero")
	}
	if err := os.Remove(filepath.Join(dir, "residents.json")); err != nil {
		t.Fatal(err)
	}
	if err := fixtureConsumersEnded(candidate); err == nil {
		t.Fatal("missing ledger treated as zero")
	}
}

func TestUnverifiedSharedShellRefusedBeforeLaunch(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Argv = []string{"/bin/sh", "-c", "touch launched"}
	spec.Environment = []string{"PATH=/usr/bin:/bin"}
	if _, err := r.Acquire(spec, "a", b); err == nil {
		t.Fatal("unverified shared service launched")
	}
	rows, err := r.Snapshot()
	if err != nil || len(rows) != 0 || b.Snapshot().Allocations != 0 {
		t.Fatalf("refusal retained effects: %+v %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(spec.Directory, "launched")); !os.IsNotExist(err) {
		t.Fatal("refused process wrote output", err)
	}
}
