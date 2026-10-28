//go:build darwin

package main

import "testing"

func TestLegacyShutdownRejectsAcceptedOrMissingUnixConnections(t *testing.T) {
	if err := validateLegacyUnixFDs("p123\nf7\ntunix\nn/original/host.sock\n"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", "p123\nf7\ntunix\nn/original/host.sock\nf8\ntunix\nn0x123->0x456\n", "p123\nf7\ntunix\nn/foreign/socket\n"} {
		if err := validateLegacyUnixFDs(raw); err == nil {
			t.Fatal("unproven empty listener accepted", raw)
		}
	}
}
