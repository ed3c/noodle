package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/execution"
)

func TestBoundConfigUsesContextPathsAndCredentials(t *testing.T) {
	t.Setenv("SPRITES_TOKEN", "host-secret")
	first, second := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(first, "agent"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("[agents.codex]\npath='agent'\n[runtime.sprites]\nsprite_name='fixture'\n")
	cfg, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for i, root := range []string{first, second} {
		env := []string{"HOME=" + root}
		if i == 0 {
			env = append(env, "SPRITES_TOKEN=context-secret")
		}
		e, err := execution.New(root, env)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := Bind(cfg, e)
		if err != nil {
			t.Fatal(err)
		}
		if got := bound.Runtime.Sprites.Token(); (i == 0 && got != "context-secret") || (i == 1 && got != "") {
			t.Fatalf("credential leak: %q", got)
		}
		fatals := Validate(bound).Fatals()
		if (i == 0 && len(fatals) != 0) || (i == 1 && len(fatals) == 0) {
			t.Fatalf("context path validation %d: %+v", i, fatals)
		}
		if !filepath.IsAbs(bound.Skills.Paths[0]) {
			t.Fatal("unbound skill path")
		}
	}
	if cfg.Agents.Codex.Path != "agent" || cfg.Skills.Paths[0] != ".agents/skills" {
		t.Fatal("Bind mutated original config")
	}
}
