package config

import (
	"slices"
	"testing"
)

func TestOpenShellRuntimeConfigurationIsExplicit(t *testing.T) {
	cfg, err := Parse([]byte("[runtime]\ndefault='openshell'\n[runtime.openshell]\nprofile='/etc/noodle/profile.json'\nmax_concurrent=1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Default != "openshell" || !slices.Contains(cfg.AvailableRuntimes(), "openshell") || cfg.Runtime.MaxConcurrentFor("openshell") != 1 {
		t.Fatalf("OpenShell config lost: %+v", cfg.Runtime)
	}
	if len(Validate(cfg).Fatals()) != 0 {
		t.Fatal("configured OpenShell rejected")
	}
	for _, data := range []string{"[runtime]\ndefault='openshell'\n", "[runtime.openshell]\nmax_concurrent=1\n", "[runtime.openshell]\nprofile='profile.json'\nmax_concurrent=-1\n"} {
		cfg, err = Parse([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if len(Validate(cfg).Fatals()) == 0 {
			t.Fatalf("invalid OpenShell config accepted: %s", data)
		}
	}
}
