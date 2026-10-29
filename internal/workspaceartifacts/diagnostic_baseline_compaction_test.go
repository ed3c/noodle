package workspaceartifacts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func diagnosticBaselineFixture(t *testing.T) (string, string, *Baseline, map[string]string) {
	t.Helper()
	root, base := publicationFixture(t)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "execution")
	testGit(t, root, "worktree", "add", "--detach", target, "HEAD")
	session := filepath.Join(base, "diagnostic-session")
	if err := os.Mkdir(session, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := Capture(t.Context(), target, filepath.Join(session, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, name := range []string{"openshell-dispatch.json", "spawn.json", "meta.json", "input.txt", "logs/stderr.log", "instructions/manifest.json"} {
		put(t, session, name, []byte(name), 0600)
		sources[filepath.Join(session, name)] = digest([]byte(name))
	}
	return root, target, b, sources
}

func TestDiagnosticBaselineCompactionPinsOriginalCleanHead(t *testing.T) {
	root, target, b, sources := diagnosticBaselineFixture(t)
	if err := VerifyDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
		t.Fatal(err)
	}
	if err := CompactDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(b.Directory); !os.IsNotExist(err) {
		t.Fatal("full baseline copy survived")
	}
	testGit(t, root, "worktree", "remove", target)
	testGit(t, root, "gc", "--prune=now")
	for i := 0; i < 2; i++ {
		if err := VerifyDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
			t.Fatal(err)
		}
		if err := CompactDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range sources {
		raw, err := os.ReadFile(path)
		if err != nil || digest(raw) != want {
			t.Fatal("original diagnostic evidence lost")
		}
	}
}

func TestDiagnosticBaselineCompactionResumesOnlyOriginalSurvivors(t *testing.T) {
	root, target, b, sources := diagnosticBaselineFixture(t)
	calls := 0
	stop := errors.New("injected remove interruption")
	err := compactDiagnosticBaseline(t.Context(), root, b, target, sources, func(path string) error {
		calls++
		// The durable manifest and both pins must precede every deletion.
		if err := VerifyDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
			t.Fatalf("unverified removal: %v", err)
		}
		if calls == 2 {
			return stop
		}
		return os.Remove(path)
	})
	if !errors.Is(err, stop) {
		t.Fatalf("interruption not retained: %v", err)
	}
	if err := CompactDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticBaselineCompactionRefusesChangedInputs(t *testing.T) {
	for _, kind := range []string{"dirty-target", "index", "copy", "source", "protected-copy", "linked-copy", "redirected-layout", "missing-copy", "missing-source", "wrong-head"} {
		t.Run(kind, func(t *testing.T) {
			root, target, b, sources := diagnosticBaselineFixture(t)
			switch kind {
			case "dirty-target":
				put(t, target, "untracked.txt", []byte("unsaved"), 0600)
			case "index":
				b.IndexSHA256 = digest([]byte("foreign index"))
			case "copy":
				put(t, b.Directory, "file.txt", []byte("changed"), 0644)
			case "source":
				for path := range sources {
					put(t, filepath.Dir(path), filepath.Base(path), []byte("changed"), 0600)
					break
				}
			case "protected-copy":
				put(t, b.Directory, "evidence.log", []byte("preserve"), 0600)
			case "linked-copy":
				original := b.Directory
				if err := os.Rename(original, original+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original+"-saved", original); err != nil {
					t.Fatal(err)
				}
			case "redirected-layout":
				b.Directory = root
			case "missing-copy":
				if err := os.RemoveAll(b.Directory); err != nil {
					t.Fatal(err)
				}
			case "missing-source":
				for path := range sources {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					break
				}
			case "wrong-head":
				b.Head = "0123456789012345678901234567890123456789"
			}
			calls := 0
			if err := compactDiagnosticBaseline(t.Context(), root, b, target, sources, func(string) error { calls++; return nil }); err == nil {
				t.Fatal("changed original input accepted")
			}
			if calls != 0 {
				t.Fatal("invalid evidence reached removal")
			}
		})
	}
}

func TestDiagnosticBaselineCompactionRefusesChangedPinnedEvidence(t *testing.T) {
	for _, kind := range []string{"source", "intent", "pin", "survivor"} {
		t.Run(kind, func(t *testing.T) {
			root, target, b, sources := diagnosticBaselineFixture(t)
			stop := errors.New("pause before removal")
			if err := compactDiagnosticBaseline(t.Context(), root, b, target, sources, func(string) error { return stop }); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			switch kind {
			case "source":
				for path := range sources {
					put(t, filepath.Dir(path), filepath.Base(path), []byte("changed"), 0600)
					break
				}
			case "intent":
				put(t, filepath.Dir(b.Directory), DiagnosticBaselineCompactionName, []byte(`{}`), 0600)
			case "pin":
				testGit(t, root, "update-ref", "-d", diagnosticBaselinePrefix(&diagnosticBaselineCompaction{Baseline: *b})+"/base")
			case "survivor":
				put(t, b.Directory, "unexpected", []byte("unsaved"), 0600)
			}
			calls := 0
			if err := compactDiagnosticBaseline(t.Context(), root, b, target, sources, func(string) error { calls++; return nil }); err == nil {
				t.Fatal("changed pinned evidence accepted")
			}
			if calls != 0 {
				t.Fatal("changed evidence reached removal")
			}
		})
	}
}

func TestDiagnosticBaselineCompactionUnpinnedIntentRequiresFullOriginalInputs(t *testing.T) {
	root, target, b, sources := diagnosticBaselineFixture(t)
	m, err := prepareDiagnosticBaseline(t.Context(), root, b, target, sources)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Dir(b.Directory), DiagnosticBaselineCompactionName, raw, 0600)
	// A crash after durable intent but before pinning grants no deletion; the
	// runtime may inspect and explicitly resume only with every original input.
	if err := VerifyDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
		t.Fatal(err)
	}
	if err := CompactDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDiagnosticBaseline(t.Context(), root, b, target, sources); err != nil {
		t.Fatal(err)
	}
}
