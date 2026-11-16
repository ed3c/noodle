package workspaceartifacts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationScratchReleasePreservesRetainedReadback(t *testing.T) {
	for _, unchanged := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "unchanged"}[unchanged], func(t *testing.T) {
			root, base := publicationFixture(t)
			b, returned := captureForTest(t, root, base)
			if !unchanged {
				put(t, returned, "file.txt", []byte("worker result\n"), 0755)
			}
			r, err := Apply(t.Context(), root, b, returned, filepath.Join(base, "evidence"))
			if err != nil {
				t.Fatal(err)
			}
			head, err := Publish(t.Context(), root, b, r, "feat: preserve exact worker output for review")
			if err != nil {
				t.Fatal(err)
			}
			if err := ReleasePublicationScratch(t.Context(), root, b, r, head); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(base, "evidence", "patch-repository")); !os.IsNotExist(err) {
				t.Fatal("original scratch remains", err)
			}
			if err := VerifyRetainedPublication(t.Context(), root, b, r, head); err != nil {
				t.Fatal("scratch release broke original retained reader", err)
			}
			for _, path := range []string{b.Directory, returned, filepath.Join(base, "evidence", "baseline"), filepath.Join(base, "evidence", "returned"), r.PatchPath, filepath.Join(base, "evidence", "result.json"), filepath.Join(base, "evidence", "publish.json")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("required evidence removed", path, err)
				}
			}
		})
	}
}

func TestPublicationScratchRefusesFailedTamperedForeignOrReloaded(t *testing.T) {
	for _, variant := range []string{"unpublished", "failed", "unknown", "tampered", "foreign", "reloaded", "receipt", "returned"} {
		t.Run(variant, func(t *testing.T) {
			root, base := publicationFixture(t)
			b, r := appliedForPublication(t, root, base)
			scratch := filepath.Join(base, "evidence", "patch-repository")
			head := b.Head
			if variant != "unpublished" && variant != "failed" {
				var err error
				head, err = Publish(t.Context(), root, b, r, "feat: retain original worker output")
				if err != nil {
					t.Fatal(err)
				}
			}
			switch variant {
			case "failed":
				put(t, base, "hooks/pre-commit", []byte("#!/bin/sh\nexit 1\n"), 0755)
				if _, err := Publish(t.Context(), root, b, r, "feat: preserve failed publication evidence"); err == nil {
					t.Fatal("failed hook accepted")
				}
			case "unknown":
				r.State, r.Applied = "unknown", false
			case "tampered":
				put(t, scratch, "foreign", []byte("keep"), 0644)
			case "foreign":
				if err := os.Rename(scratch, scratch+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(scratch, 0700); err != nil {
					t.Fatal(err)
				}
				put(t, scratch, "foreign", []byte("keep"), 0644)
			case "reloaded":
				raw, _ := json.Marshal(r)
				var reloaded Result
				if err := json.Unmarshal(raw, &reloaded); err != nil {
					t.Fatal(err)
				}
				r = &reloaded
			case "receipt":
				put(t, base, "evidence/publish.json", []byte(`{}`), 0600)
			case "returned":
				put(t, r.ReturnedDirectory, "file.txt", []byte("changed evidence"), 0644)
			}
			err := ReleasePublicationScratch(context.Background(), root, b, r, head)
			if err == nil {
				t.Fatal("unqualified scratch release accepted")
			}
			onlyScratch := variant == "tampered" || variant == "foreign" || variant == "reloaded"
			if errors.Is(err, ErrPublicationScratchHeld) != onlyScratch {
				t.Fatal("required publication failure confused with held scratch", variant, err)
			}
			if _, err := os.Lstat(scratch); err != nil {
				t.Fatal("refused scratch was changed", err)
			}
		})
	}
}
