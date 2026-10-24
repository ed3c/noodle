package main

import (
	"bytes"
	"encoding/json"
	"github.com/poteto/noodle/internal/loopview"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostViewsImportOfflineAndInspect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", dir)
	source := filepath.Join(t.TempDir(), "view.json")
	data := `{"schema_version":1,"runs":[{"id":"original","repository":"repo","mode":"outer-only","status":"unknown","stages":[],"cards":[],"notes":[],"breakpoints":[],"next":null}]}`
	if err := os.WriteFile(source, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := NewRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	result := run("host", "views", "import", "--file", source)
	var receipt struct {
		Changed  bool `json:"changed"`
		RunCount int  `json:"run_count"`
	}
	if err := json.Unmarshal([]byte(result), &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Changed || receipt.RunCount != 1 {
		t.Fatalf("receipt: %s", result)
	}
	got := run("host", "views", "inspect")
	if !strings.Contains(got, `"id":"original"`) {
		t.Fatalf("inspect: %s", got)
	}
	if _, err := loopview.Decode([]byte(got)); err != nil {
		t.Fatalf("inspect no longer produces reusable display source: %v", err)
	}
	withWorkspace := run("host", "views", "inspect", "--include-workspace")
	if !strings.Contains(withWorkspace, `"workspace_observation"`) || !strings.Contains(withWorkspace, `"runs"`) {
		t.Fatalf("workspace observation lost display shape: %s", withWorkspace)
	}
	for _, name := range []string{"host.sock", "host.lock", "receipts.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("offline import touched Host lifecycle %s", name)
		}
	}
}
