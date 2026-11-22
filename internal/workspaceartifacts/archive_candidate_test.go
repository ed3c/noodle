package workspaceartifacts

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInspectArchiveCandidateUsesOriginalScope(t *testing.T) {
	for _, test := range []struct {
		name, failure   string
		divergeBaseline bool
	}{
		{name: "allowed"},
		{name: "outside", failure: "violates original scope"},
		{name: "outside reverted to merge base", failure: "outside original write_paths", divergeBaseline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, root := fixture(t)
			testGit(t, target, "branch", "integration")
			if test.divergeBaseline {
				put(t, target, "outside.txt", []byte("tracked at the frozen parent\n"), 0600)
				testGit(t, target, "add", "outside.txt")
				testGit(t, target, "commit", "-m", "freeze diverged parent")
			}
			baseline, returned := captureForTest(t, target, root)
			put(t, returned, "file.txt", []byte("updated\n"), 0644)
			if test.divergeBaseline {
				if err := os.Remove(filepath.Join(returned, "outside.txt")); err != nil {
					t.Fatal(err)
				}
			} else if test.failure != "" {
				put(t, returned, "outside.txt", []byte("not in the admitted scope\n"), 0600)
			}
			scope := LocalTaskScope{IntegrationBranch: "integration", WritePaths: []string{"file.txt"}}
			result, err := InspectArchiveCandidate(t.Context(), target, filepath.Join(root, "isolated-git"), returned, baseline, scope)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("expected scope refusal %q, got %v", test.failure, err)
				}
			} else if err != nil || result.BaselineTree == result.CandidateTree || !slices.Equal(result.ChangedPaths, []string{"file.txt"}) {
				t.Fatalf("expected the original scope and isolated result, got %+v, %v", result, err)
			}
			if err = VerifyBaseline(t.Context(), target, baseline); err != nil {
				t.Fatal("inspection changed the original checkout", err)
			}
		})
	}
}
