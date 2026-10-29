package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/execution"
)

func TestPiConfigBindAndDiagnostics(t *testing.T) {
	cfg, diagnostics, err := parseWithDiagnostics([]byte(`[routing.defaults]
provider="pi"
model="openrouter/deepseek/deepseek-v4.1-flash-1017"
[agents.pi]
path="agent"
require_typed_outcome=true
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 || cfg.Routing.Defaults.Provider != "pi" || !cfg.Agents.Pi.RequireTypedOutcome {
		t.Fatalf("cfg=%+v diagnostics=%+v", cfg, diagnostics)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "agent"), 0700); err != nil {
		t.Fatal(err)
	}
	e, err := execution.New(root, []string{})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := Bind(cfg, e)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents.Pi.Path != "agent" || bound.Agents.Pi.Path != filepath.Join(e.Root(), "agent") || !Validate(bound).CanSpawn() {
		t.Fatalf("bound=%+v diagnostics=%+v", bound, Validate(bound))
	}
	bound.Agents.Pi.Path = filepath.Join(root, "missing")
	found := false
	for _, d := range Validate(bound).Fatals() {
		if d.FieldPath == "agents.pi.path" {
			found = true
		}
	}
	if !found {
		t.Fatal("Pi path missing diagnostic")
	}
	cfg.Agents.Pi.Args = []string{"--model", "other"}
	found = false
	for _, d := range Validate(cfg).Fatals() {
		if d.FieldPath == "agents.pi.args" {
			found = true
		}
	}
	if !found {
		t.Fatal("Pi extras missing diagnostic")
	}
}

func TestPiConfigDoesNotGuessModel(t *testing.T) {
	for _, text := range []string{"[routing.defaults]\nprovider='pi'\n", "[routing.defaults]\nprovider='pi'\nmodel=''\n", "[routing.defaults]\nprovider='pi'\nmodel='flash'\n"} {
		cfg, err := Parse([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if Validate(cfg).CanSpawn() {
			t.Fatalf("accepted missing exact model: %+v", cfg.Routing.Defaults)
		}
		if cfg.Routing.Defaults.Model == defaultRoutingModel {
			t.Fatal("Pi inherited Claude default model")
		}
	}
}
