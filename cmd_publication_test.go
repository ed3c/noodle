package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/loop"
)

func TestPublicationClaimCommandWritesExactFreshOutput(t *testing.T) {
	original := inspectPublicationClaim
	t.Cleanup(func() { inspectPublicationClaim = original })
	project := t.TempDir()
	inspectPublicationClaim = func(gotProject, order, subject string) (loop.PublicationClaim, error) {
		if gotProject != project || order != "order-1" || subject != "example/project#7" {
			t.Fatalf("inspect args = %q %q %q", gotProject, order, subject)
		}
		return loop.PublicationClaim{SchemaVersion: 1, Owner: "Noodle", Repository: "example/project", Subject: subject}, nil
	}
	app := &App{projectDir: project}
	output := filepath.Join(t.TempDir(), "claim.json")
	command := newPublicationClaimCmd(app)
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetArgs([]string{"order-1", "example/project#7", "--output", output})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != string(written) || !strings.Contains(string(written), `"owner": "Noodle"`) {
		t.Fatalf("stdout/output mismatch:\n%s\n%s", stdout.String(), written)
	}

	command = newPublicationClaimCmd(app)
	command.SetArgs([]string{"order-1", "example/project#7", "--output", output})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("overwrite error = %v", err)
	}
}

func TestPublicationClaimCommandRequiresAbsoluteOutput(t *testing.T) {
	original := inspectPublicationClaim
	t.Cleanup(func() { inspectPublicationClaim = original })
	inspectPublicationClaim = func(_, _, _ string) (loop.PublicationClaim, error) {
		return loop.PublicationClaim{SchemaVersion: 1, Owner: "Noodle"}, nil
	}
	command := newPublicationClaimCmd(&App{projectDir: t.TempDir()})
	command.SetArgs([]string{"order-1", "example/project#7", "--output", "claim.json"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative output error = %v", err)
	}
}
