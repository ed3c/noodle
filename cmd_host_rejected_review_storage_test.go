package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
)

func failedReviewStorageFixture(t *testing.T) (string, string) {
	t.Helper()
	host, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(host, "openshell")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return host, dir
}

func writeFailedReviewStorageFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original bytes"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFailedReviewStorageReclaimRetainsCheckoutAndCustody(t *testing.T) {
	host, dir := failedReviewStorageFixture(t)
	instance := strings.Repeat("a", 32)
	bootstrap := "sandbox-bootstrap-rootfs-ext4-v6-openshell-0.1.5-guest-" + strings.Repeat("a", 64) + "-" + strings.Repeat("b", 64)
	generated := []string{
		"vm/images/" + bootstrap + "/rootfs.ext4",
		"vm/images/overlay-templates/sandbox-overlay-ext4-v1/10737418240.ext4",
		"vm/images/preparation.lock", "vm/host-runtime/openshell-supervisor",
		"gateway.db", "gateway.db-shm", "gateway.db-wal",
	}
	kept := []string{
		filepath.Join(dir, "custody", "owner.json"),
		filepath.Join(dir, "materialization.json"),
		filepath.Join(host, "controls", "original", "checkout", "original"),
	}
	for _, name := range generated {
		writeFailedReviewStorageFixture(t, filepath.Join(dir, name))
	}
	for _, name := range kept {
		writeFailedReviewStorageFixture(t, name)
	}
	for _, name := range []string{"vm/sandboxes", "vm/run", "vm/rootfs-tar-staging", "vm/images/preparations"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	verify := func() error { return nil }
	present, err := reclaimFailedReviewStorageTree(dir, instance, false, verify)
	if err != nil || !present {
		t.Fatalf("preview lost the original runtime: present %t, error %v", present, err)
	}
	for _, name := range generated {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("preview changed %s: %v", name, err)
		}
	}
	for _, apply := range []bool{true, false, true} {
		if present, err := reclaimFailedReviewStorageTree(dir, instance, apply, verify); err != nil || present {
			t.Fatalf("retirement readback differs, apply %t, present %t, error %v", apply, present, err)
		}
	}
	for _, name := range kept {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != "original bytes" {
			t.Fatalf("retained data changed at %s: %v", name, err)
		}
	}
}

func TestFailedReviewStorageRefusesRetainedAndForeignResidue(t *testing.T) {
	for _, test := range []struct {
		path string
		link bool
	}{
		{path: "vm/sandboxes/retained/overlay.ext4"},
		{path: "vm/run/unresolved"},
		{path: "vm/images/foreign.ext4"},
		{path: "vm/host-runtime/openshell-supervisor", link: true},
		{path: "gateway.db-wal", link: true},
		{path: "gateway.db-journal"},
		{path: ".vm-retiring-" + strings.Repeat("b", 32) + "/run/foreign"},
	} {
		t.Run(test.path, func(t *testing.T) {
			_, dir := failedReviewStorageFixture(t)
			foreign := filepath.Join(t.TempDir(), "external")
			writeFailedReviewStorageFixture(t, foreign)
			database := filepath.Join(dir, "gateway.db")
			writeFailedReviewStorageFixture(t, database)
			original := filepath.Join(dir, test.path)
			if test.link {
				if err := os.MkdirAll(filepath.Dir(original), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, original); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFailedReviewStorageFixture(t, original)
			}
			if _, err := reclaimFailedReviewStorageTree(dir, strings.Repeat("a", 32), true, func() error { return nil }); err == nil {
				t.Fatal("reclaimed an unresolved or foreign runtime")
			}
			for _, path := range []string{database, original, foreign} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("rejected operation changed %s: %v", path, err)
				}
			}
		})
	}
}

func TestFailedReviewStorageRequiresFreshProofBeforeContinuingExactStage(t *testing.T) {
	_, dir := failedReviewStorageFixture(t)
	instance := strings.Repeat("a", 32)
	staged := filepath.Join(dir, ".vm-retiring-"+instance)
	if err := os.MkdirAll(filepath.Join(staged, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(dir, "gateway.db")
	writeFailedReviewStorageFixture(t, database)
	proof := errors.New("exact custody changed")
	if _, err := reclaimFailedReviewStorageTree(dir, instance, true, func() error { return proof }); !errors.Is(err, proof) {
		t.Fatalf("lost original proof refusal: %v", err)
	}
	for _, path := range []string{staged, database} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("refused proof changed %s: %v", path, err)
		}
	}
	if present, err := reclaimFailedReviewStorageTree(dir, instance, true, func() error { return nil }); err != nil || present {
		t.Fatalf("exact original staging was not reconciled: present %t, error %v", present, err)
	}
}

func TestFailedReviewStorageRequiresNativeExitAndReleasedCustody(t *testing.T) {
	host, dir := failedReviewStorageFixture(t)
	lockPath := filepath.Join(dir, "custody", "owner.lock")
	writeFailedReviewStorageFixture(t, lockPath)
	lock, err := lockfile.TryLockExisting(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	retired := loopruntime.SessionReadbackFileRef{Path: filepath.Join(dir, "custody", "owner.json"), SHA256: strings.Repeat("a", 64)}
	database := filepath.Join(dir, "gateway.db")
	writeFailedReviewStorageFixture(t, database)
	for _, proof := range []func() error{nil, func() error { return errors.New("original gateway exit unobserved") }, func() error { return nil }} {
		if remaining, err := reclaimFailedReviewRuntimeStorage(host, strings.Repeat("a", 32), retired, retired, retired, true, proof); err == nil || !remaining {
			t.Fatalf("runtime trim accepted missing exit or held custody, remaining=%t err=%v", remaining, err)
		}
	}
	if _, err := os.Stat(database); err != nil {
		t.Fatal("live Gateway database was changed", err)
	}
}
