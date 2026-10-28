package main

import "testing"

func TestHostShutdownResidentClientPreservesUnknownOwner(t *testing.T) {
	for _, tc := range []struct {
		name, operation, status, state string
		want                           bool
	}{
		{"release-failed", "release", "unknown_effect", "failed", true},
		{"acquire-failed", "acquire", "unknown_effect", "failed", false},
		{"inspect-failed", "inspect", "unknown_effect", "failed", true},
		{"release-active", "release", "ok", "active", true},
		{"release-unknown-active", "release", "unknown_effect", "active", false},
		{"release-detached", "release", "ok", "detached", false},
		{"release-unavailable", "release", "host_unavailable", "failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := hostResponse{hostReceipt: hostReceipt{HostInstanceID: "original", ContextID: "context", ContextState: tc.state}, Status: tc.status}
			if got := hostResidentRequestAvailable(tc.operation, original); got != tc.want {
				t.Fatalf("original status=%s state=%s admitted=%t", tc.status, tc.state, got)
			}
			original.HostInstanceID = ""
			if hostResidentRequestAvailable(tc.operation, original) {
				t.Fatal("missing original owner identity admitted")
			}
		})
	}
}
