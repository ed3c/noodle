package main

import (
	"encoding/json"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestStorageFailurePreservesCompleteOriginalHostOwnership(t *testing.T) {
	fixture := func() (storageFailureHostPacket, loopruntime.StorageFailureStopPlan, hostRequestBinding, []hostReceipt) {
		p := loopruntime.StorageFailureStopPlan{HostInstance: "original", HostPID: 123, ContextID: "failed-attempt"}
		b := hostRequestBinding{HostDir: "/host", BinarySHA256: "binary", RequestID: "request", ProjectDir: "/control", ConfigPath: "/config", ConfigSHA256: "config"}
		packet := storageFailureHostPacket{Schema: 1, Protocol: hostProtocol, Instance: p.HostInstance, PID: p.HostPID, Binary: b.BinarySHA256, Directory: b.HostDir, Contexts: 3}
		packet.Resources.Workers, packet.Resources.Residents, packet.Resources.Allocations = 1, 1, 2
		packet.Residents.Error = json.RawMessage(`"original persistence failure"`)
		packet.Residents.Records = []admission.Resident{{ID: "gateway", State: "running", PID: 456, ProcessGroup: 456, Started: "original start", GuardID: "original guard", Contexts: []string{"public-a", "failed-attempt"}}}
		rows := []hostReceipt{}
		for _, id := range []string{"public-a", "public-b", "failed-attempt"} {
			rows = append(rows, hostReceipt{HostInstanceID: p.HostInstance, HostPID: p.HostPID, HostDir: b.HostDir, BinarySHA256: b.BinarySHA256, ContextState: "active", ContextID: id, RequestID: id, ProjectDir: b.ProjectDir, ConfigPath: b.ConfigPath, ConfigSHA256: b.ConfigSHA256})
		}
		rows[2].RequestID = b.RequestID
		return packet, p, b, rows
	}
	for _, kind := range []string{"original", "replaced-host", "missing-context", "added-context", "foreign-lease", "unobserved-fault", "shutting-down", "other-worker", "missing-generation"} {
		t.Run(kind, func(t *testing.T) {
			packet, p, b, rows := fixture()
			switch kind {
			case "replaced-host":
				packet.Instance = "replacement"
			case "missing-context":
				rows = rows[1:]
			case "added-context":
				packet.Contexts++
			case "foreign-lease":
				packet.Residents.Records[0].Contexts = append(packet.Residents.Records[0].Contexts, "unknown")
			case "unobserved-fault":
				packet.Residents.Error = json.RawMessage(`null`)
			case "shutting-down":
				packet.Shutdown.Requested = true
			case "other-worker":
				packet.Resources.Workers++
			case "missing-generation":
				packet.Residents.Records[0].Started = ""
			}
			err := storageFailureHostIdentity(packet, p, b, rows)
			if (kind == "original") != (err == nil) {
				t.Fatalf("ownership result for %s: %v", kind, err)
			}
		})
	}
}

func TestStorageFailureCommandDoesNotImplicitlyAbandonWorkspace(t *testing.T) {
	cmd := newStorageFailureCmd()
	cmd.SetArgs([]string{"prepare", "--request-file", "/request", "--session-id", "session", "--output", "/evidence", "--native-implementation", "/native", "--launcher-intent", "/launch", "--launcher-call", "/call", "--launcher-session", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("missing explicit abandonment reached preparation")
	}
}
