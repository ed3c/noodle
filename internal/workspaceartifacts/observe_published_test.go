package workspaceartifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedPublicationSurvivesCheckoutAdvanceAndRejectsChangedEvidence(t *testing.T) {
	root, base := publicationFixture(t)
	b, result := appliedForPublication(t, root, base)
	head, err := Publish(t.Context(), root, b, result, "test: publish retained result")
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "commit", "--allow-empty", "-m", "later accepted work")
	t.Setenv("GIT_DIR", filepath.Join(base, "foreign.git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(base, "foreign.index"))
	if err := VerifyRetainedPublication(t.Context(), root, b, result, head); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{b.Directory, result.ReturnedDirectory, filepath.Join(filepath.Dir(result.PatchPath), "returned")} {
		file := filepath.Join(directory, "file.txt")
		original, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(file, []byte("changed"), 0644); err != nil {
			t.Fatal(err)
		}
		if err = VerifyRetainedPublication(t.Context(), root, b, result, head); err == nil {
			t.Fatal("changed retained bytes accepted")
		}
		if err = os.WriteFile(file, original, 0644); err != nil {
			t.Fatal(err)
		}
	}
	changed := *b
	changed.Head = head
	if err := VerifyRetainedPublication(t.Context(), root, &changed, result, head); err == nil {
		t.Fatal("wrong original parent accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := VerifyRetainedPublication(ctx, root, b, result, head); err == nil {
		t.Fatal("cancelled read continued")
	}
}
