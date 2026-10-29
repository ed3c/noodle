package config

import "testing"

func TestNativeCodexRuntimeSelectionIsNotNormalizedToProcess(t *testing.T) {
	cfg, err := Parse([]byte("[runtime]\ndefault = 'codex-app-server'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Default != "codex-app-server" {
		t.Fatalf("native runtime silently changed to %q", cfg.Runtime.Default)
	}
	available := cfg.AvailableRuntimes()
	if len(available) != 2 || available[0] != "process" || available[1] != "codex-app-server" {
		t.Fatalf("native runtime selection missing: %v", available)
	}
	if DefaultConfig().Runtime.Default != "process" {
		t.Fatal("unbound legacy default changed")
	}
}
