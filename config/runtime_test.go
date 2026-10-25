package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeConfigUsesBoundSourceAndRefusesDrift(t *testing.T) {
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".noodle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, DefaultConfigPath), []byte("mode = 'auto'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "task.toml")
	data := []byte("mode = 'manual'\n[runtime.process]\nadmitted_orders = ['original-order']\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	source := RuntimeSource{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	if _, err := SaveRuntimeSource(project, source); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveRuntimeSource(project, source); err != nil {
		t.Fatalf("same source retry: %v", err)
	}
	cfg, _, err := LoadRuntime(project)
	if err != nil || cfg.Mode != "manual" || len(cfg.Runtime.Process.AdmittedOrders) != 1 {
		t.Fatalf("selected config: %+v %v", cfg, err)
	}
	if _, err := SaveRuntimeSource(project, RuntimeSource{}); err == nil {
		t.Fatal("default replaced selected source")
	}
	other := source
	other.Path = filepath.Join(project, "other.toml")
	if _, err := SaveRuntimeSource(project, other); err == nil {
		t.Fatal("different source replaced binding")
	}
	if err := os.WriteFile(path, []byte("mode = 'auto'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadRuntime(project); err == nil {
		t.Fatal("source drift used default config")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadRuntime(project); err == nil {
		t.Fatal("missing source used default config")
	}
	if err := os.Symlink(filepath.Join(project, DefaultConfigPath), path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadRuntime(project); err == nil {
		t.Fatal("source symlink used default config")
	}
}

func TestRuntimeConfigWithoutHostSelectionUsesProject(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, DefaultConfigPath), []byte("mode = 'auto'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadRuntime(project)
	if err != nil || cfg.Mode != "auto" {
		t.Fatalf("project config: %+v %v", cfg, err)
	}
}

func TestRuntimeConfigRejectsDanglingBinding(t *testing.T) {
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".noodle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(project, "missing"), runtimeSourcePath(project)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadRuntime(project); err == nil {
		t.Fatal("dangling binding used default config")
	}
}
