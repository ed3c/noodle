package workspaceartifacts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetainedUnchangedPublicationRequiresOriginalHeadAndBytes(t *testing.T) {
	for _, variant := range []string{"clean", "head", "patch", "applied", "contents", "unknown"} {
		t.Run(variant, func(t *testing.T) {
			root, base := publicationFixture(t)
			b, returned := captureForTest(t, root, base)
			result, err := Apply(t.Context(), root, b, returned, filepath.Join(base, "evidence"))
			if err != nil {
				t.Fatal(err)
			}
			head, err := Publish(t.Context(), root, b, result, "retain original unchanged result")
			if err != nil {
				t.Fatal(err)
			}
			testGit(t, root, "commit", "--allow-empty", "-m", "later accepted work")
			switch variant {
			case "head":
				head = strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
			case "patch":
				put(t, filepath.Dir(result.PatchPath), filepath.Base(result.PatchPath), []byte("not empty"), 0600)
			case "applied":
				result.Applied = true
			case "contents":
				for _, directory := range []string{result.ReturnedDirectory, filepath.Join(filepath.Dir(result.PatchPath), "returned")} {
					put(t, directory, "file.txt", []byte("changed"), 0644)
				}
			case "unknown":
				result.State = "unknown"
			}
			err = VerifyRetainedPublication(t.Context(), root, b, result, head)
			if (err == nil) != (variant == "clean") {
				t.Fatalf("retained unchanged outcome: %v", err)
			}
		})
	}
}

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
