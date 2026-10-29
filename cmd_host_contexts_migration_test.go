package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextMigrationPreservesRequestAndArchivesExactSources(t *testing.T) {
	dir, before, _ := contextTestRegistration(t)
	after := before
	after.OriginalHost.InstanceID = "selected-host"
	after.OriginalHost.BinarySHA256 = strings.Repeat("b", 64)
	expected := contextVersion(before)
	if err := archiveContextMigration(dir, before, after, expected); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "context-control-migrations", expected+"-"+contextVersion(after)+".json")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveContextMigration(dir, before, after, expected); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("migration replay changed retained evidence")
	}
	entries, err := loadContextRegistrations(dir)
	if err != nil || contextVersion(entries[before.ID]) != expected {
		t.Fatal("archive silently replaced registration")
	}
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := archiveContextMigration(dir, before, after, expected); err == nil {
		t.Fatal("changed archive overwritten")
	}
}

func TestContextMigrationRejectsStaleVersionAndRetargeting(t *testing.T) {
	for _, mismatch := range []string{"version", "id", "request", "directory", "source"} {
		t.Run(mismatch, func(t *testing.T) {
			dir, before, _ := contextTestRegistration(t)
			after := before
			after.OriginalHost.InstanceID = "selected-host"
			expected := contextVersion(before)
			switch mismatch {
			case "version":
				expected = strings.Repeat("f", 64)
			case "id":
				after.ID = "other"
			case "request":
				after.Request.SHA256 = strings.Repeat("f", 64)
			case "directory":
				after.OriginalHost.HostDir = t.TempDir()
			case "source":
				if err := os.WriteFile(before.Launcher.Entry.Path, []byte("changed owner"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := archiveContextMigration(dir, before, after, expected); err == nil {
				t.Fatal("invalid migration accepted")
			}
		})
	}
}
