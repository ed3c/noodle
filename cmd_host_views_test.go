package main

import (
	"bytes"
	"encoding/json"
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
	if got := run("host", "views", "inspect"); !strings.Contains(got, `"id":"original"`) {
		t.Fatalf("inspect: %s", got)
	}
	if got := run("host", "views", "inspect", "--run-id", "original"); !strings.Contains(got, `"id":"original"`) || strings.Contains(got, `"detail"`) {
		t.Fatal("selected imported run not expanded", got)
	}
	for _, name := range []string{"host.sock", "host.lock", "receipts.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("offline import touched Host lifecycle %s", name)
		}
	}
}
