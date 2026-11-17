package workspaceartifacts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRebuildableDependenciesUseOriginalIndexBeforeInstall(t *testing.T) {
	root, base := fixture(t)
	put(t, root, ".gitignore", []byte("node_modules/\n"), 0644)
	put(t, root, "pnpm-lock.yaml", []byte("lockfileVersion: '9.0'\n"), 0644)
	put(t, root, "packages/app/package.json", []byte(`{"name":"app"}`), 0644)
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "-m", "test: declare rebuildable workspace dependencies")
	b, _ := captureForTest(t, root, base)
	proofs, err := RebuildableDependencies(t.Context(), root, b)
	if err != nil || len(proofs) != 1 || proofs[0].Directory != "packages/app/node_modules" || proofs[0].Lockfile != "pnpm-lock.yaml" {
		t.Fatal("late installation has no original proof", proofs, err)
	}
	if _, err := os.Stat(filepath.Join(root, "packages/app/node_modules")); !os.IsNotExist(err) {
		t.Fatal("proof constructed a dependency environment", err)
	}
	put(t, root, "packages/app/node_modules/unread", []byte("dependency cache"), 0000)
	if _, err := RebuildableDependencies(t.Context(), root, b); err != nil {
		t.Fatal("proof inspected ignored dependency contents", err)
	}
}

func TestRebuildableDependenciesRefuseTrackedDependencyAndUnboundSources(t *testing.T) {
	for _, variant := range []string{"tracked", "missing-lock", "symlink-manifest"} {
		t.Run(variant, func(t *testing.T) {
			root, base := fixture(t)
			put(t, root, "package.json", []byte(`{}`), 0644)
			if variant != "missing-lock" {
				put(t, root, "pnpm-lock.yaml", []byte("lockfileVersion: '9.0'\n"), 0644)
			}
			if variant == "tracked" {
				put(t, root, "node_modules/source.js", []byte("tracked source"), 0644)
			}
			if variant == "symlink-manifest" {
				if err := os.Remove(filepath.Join(root, "package.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("file.txt", filepath.Join(root, "package.json")); err != nil {
					t.Fatal(err)
				}
			}
			testGit(t, root, "add", ".")
			testGit(t, root, "commit", "-m", "test: preserve unqualified dependency sources")
			b, _ := captureForTest(t, root, base)
			proofs, err := RebuildableDependencies(t.Context(), root, b)
			if variant == "missing-lock" {
				if err != nil || len(proofs) != 0 {
					t.Fatal("unlocked dependencies received exclusion authority", proofs, err)
				}
			} else if err == nil {
				t.Fatal("tracked or alias source acquired rebuild authority")
			}
		})
	}
}
