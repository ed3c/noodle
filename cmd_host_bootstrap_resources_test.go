package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/admission"
)

func TestHostBootstrapAdmissionPreservesOriginalJournalBeforeAnyEffect(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "interrupted"}[historical], func(t *testing.T) {
			root := t.TempDir()
			original := []byte(`{"phase":"draining","unknown":"original failure evidence"}`)
			if historical {
				if err := os.WriteFile(filepath.Join(root, "bootstrap.json"), original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := bootstrapLegacyHost(context.Background(), root, "/unavailable-target", "original-digest", nil, json.NewEncoder(io.Discard))
			if !errors.Is(err, admission.ErrDeferred) {
				t.Fatal("legacy migration was not refused before effects", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			expected := 0
			if historical {
				expected = 1
			}
			if len(entries) != expected {
				t.Fatal("lock/custody/process effects preceded admission", entries)
			}
			if historical {
				raw, err := os.ReadFile(filepath.Join(root, "bootstrap.json"))
				if err != nil || string(raw) != string(original) {
					t.Fatal("original migration evidence changed", err)
				}
			}
		})
	}
}
