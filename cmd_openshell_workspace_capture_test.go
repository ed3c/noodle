package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/config"
)

func TestWorkspaceCaptureUsesAcceptedDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, config.DefaultConfigPath)
	raw := []byte("mode = 'manual'\n")
	if err := os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
	binding := hostRequestBinding{ProjectDir: dir, ConfigSHA256: hostDigest(raw)}
	got, err := readWorkspaceCaptureConfig(binding)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("default original config did not survive an omitted ConfigPath: %q: %v", got, err)
	}
	binding.ConfigSHA256 = hostDigest([]byte("different original source"))
	if _, err = readWorkspaceCaptureConfig(binding); err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("default config replaced a different original digest: %v", err)
	}
}
