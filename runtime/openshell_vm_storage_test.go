package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func vmStorageFixture(t *testing.T) openShellVMStorage {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sandboxes"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "gateway.toml")
	data := fmt.Sprintf("[openshell.gateway]\ncompute_driver='vm'\n[openshell.drivers.vm]\nstate_dir=%q\n", root)
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := ReadSessionReadbackFile(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	return openShellVMStorage{Config: ref, Root: root}
}

func TestVMStorageReadbackUsesOnlyOriginalIdentity(t *testing.T) {
	storage := vmStorageFixture(t)
	original := filepath.Join(storage.Root, "sandboxes", "original")
	foreign := filepath.Join(storage.Root, "sandboxes", "foreign")
	for _, path := range []string{original, foreign} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if present, err := storage.sandboxPresent("original"); err != nil || !present {
		t.Fatalf("original state presence not observed: %t %v", present, err)
	}
	if present, err := storage.sandboxPresent("missing"); err != nil || present {
		t.Fatalf("original state absence not observed: %t %v", present, err)
	}
	for _, path := range []string{original, foreign} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("readback changed VM state: %v", err)
		}
	}
	for _, id := range []string{"", "..", "../foreign", "/foreign", "bad\x00id"} {
		if _, err := storage.sandboxPresent(id); err == nil {
			t.Fatalf("accepted invalid VM identity %q", id)
		}
	}
}

func TestVMStorageReadbackRejectsDriftAndLinks(t *testing.T) {
	t.Run("changed configuration", func(t *testing.T) {
		storage := vmStorageFixture(t)
		if err := os.WriteFile(storage.Config.Path, []byte("[openshell]\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.sandboxPresent("original"); err == nil {
			t.Fatal("accepted changed original configuration")
		}
	})
	t.Run("symlinked sandbox", func(t *testing.T) {
		storage := vmStorageFixture(t)
		if err := os.Symlink(t.TempDir(), filepath.Join(storage.Root, "sandboxes", "original")); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.sandboxPresent("original"); err == nil {
			t.Fatal("accepted symlinked VM state")
		}
	})
	t.Run("symlinked parent", func(t *testing.T) {
		storage := vmStorageFixture(t)
		path := filepath.Join(storage.Root, "sandboxes")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), path); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.sandboxPresent("original"); err == nil {
			t.Fatal("accepted foreign sandbox root")
		}
	})
}
