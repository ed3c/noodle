package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoalTemplatePairingLegacyNOOPAndPartialRefusal(t *testing.T) {
	root := t.TempDir()
	registration := filepath.Join(root, "host-template.json")
	const original = "/original/profile.json"
	if got, err := prepareGoalTemplatePairing(t.Context(), registration, original, "", ""); err != nil || got != original {
		t.Fatal("legacy template registration changed", got, err)
	}
	if _, err := prepareGoalTemplatePairing(t.Context(), registration, original, "/original/native-artifact.json", ""); err == nil {
		t.Fatal("partial pairing reached original registration")
	}
	if _, err := os.Stat(filepath.Join(root, "template-pairings")); !os.IsNotExist(err) {
		t.Fatal("NOOP or rejected pairing created publication state", err)
	}
}
