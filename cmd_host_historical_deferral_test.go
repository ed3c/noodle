package main

import (
	"context"
	"testing"

	"github.com/poteto/noodle/internal/admission"
)

func TestHostHistoricalDeferralRetainsOriginalRequiredInstance(t *testing.T) {
	request := hostRequest{Operation: "status", RequestID: "original-request", RequiredHostInstanceID: "original-host",
		ProjectDir: "/original/control", BinarySHA256: "original-binary", ConfigSHA256: "original-config", Mode: "manual"}
	receipt := hostReceipt{RequestID: request.RequestID, ProjectDir: request.ProjectDir, HostInstanceID: "original-host",
		ContextID: "original-context", ContextState: "detached", BinarySHA256: request.BinarySHA256,
		ConfigSHA256: request.ConfigSHA256, Mode: "manual", AdmissionRefusal: admission.ErrDeferred.Error()}
	host := &noodleHost{instanceID: "selected-host", binarySHA: "selected-binary", receipts: []hostReceipt{receipt}}
	response := host.handle(context.Background(), request)
	if response.Status != "deferred" || response.HostInstanceID != "original-host" || response.ContextID != "original-context" {
		t.Fatalf("historical readback: %+v", response)
	}
	for _, mismatch := range []string{"operation", "binary", "request", "instance", "config", "active", "unknown"} {
		t.Run(mismatch, func(t *testing.T) {
			selected := request
			original := receipt
			switch mismatch {
			case "operation":
				selected.Operation = "attach"
			case "binary":
				selected.BinarySHA256 = "foreign"
			case "request":
				selected.RequestID = "foreign"
			case "instance":
				selected.RequiredHostInstanceID = "foreign"
			case "config":
				selected.ConfigSHA256 = "foreign"
			case "active":
				original.ContextState = "active"
			case "unknown":
				original.AdmissionRefusal = "unknown effect"
			}
			host.receipts = []hostReceipt{original}
			if observed := host.handle(context.Background(), selected); observed.Status != "refused" {
				t.Fatalf("foreign or effectful readback accepted: %+v", observed)
			}
		})
	}
}
