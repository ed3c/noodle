package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/procx"
	"google.golang.org/protobuf/proto"
)

func physicalHistoryFixture(t *testing.T) (OpenShellCustodyState, SessionReadbackFileRef) {
	t.Helper()
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("original cold receipt requires native generation support")
	}
	f, release, ref := coldReleaseFixture(t)
	if _, err := UpgradeOpenShellCustodyV3(f.state.Binding, release.ControlRoot, f.plan.Originals["custody"], ref, filepath.Join(release.SourceRoot, "handover.json")); err != nil {
		t.Fatal(err)
	}
	var state OpenShellCustodyState
	if _, err := ReadSessionReadbackFile(f.plan.Custody.Path, &state); err != nil {
		t.Fatal(err)
	}
	later := state.Records[0]
	later.Name, later.MainProcessInstanceID = "later", "later-main"
	later.Task = OpenShellTask{"later-order", "later-attempt", "later-session"}
	later.CreateRequestID = "later-create"
	var intent pb.CreateSandboxRequest
	if err := proto.Unmarshal(later.CreateIntent, &intent); err != nil {
		t.Fatal(err)
	}
	intent.Name, intent.RequestId = later.Name, later.CreateRequestID
	var err error
	if later.CreateIntent, err = proto.Marshal(&intent); err != nil {
		t.Fatal(err)
	}
	later.Worker.Identity.OrderID, later.Worker.Identity.AttemptID, later.Worker.Identity.SessionID = later.Task.OrderID, later.Task.AttemptID, later.Task.SessionID
	later.Worker.Identity.SandboxName, later.Worker.Identity.Sandbox.SandboxID = later.Name, "later-id"
	later.StopRequestID, later.StopAcknowledged = "later-stop", true
	later.Stopped = &OpenShellStoppedObservation{}
	// Physical Stop is independent of the still-unknown original business result.
	later.Worker.Boundary = OpenShellBoundaryUnknown
	later.Worker.StopPending = false
	state.Records = append(state.Records, later)
	return state, storageFailureWrite(t, f.plan.Custody.Path, state)
}

func TestSourcePhysicalHistoryReusesOriginalNativeClosureWithoutWrites(t *testing.T) {
	state, ref := physicalHistoryFixture(t)
	before, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	history, err := InspectOpenShellPhysicalHistory(ref)
	if err != nil || history.OriginalRecords != len(state.Records)-1 || len(history.LaterStopped) != 1 || history.LaterStopped[0].Name != "later" || history.LaterStopped[0].MainProcessInstanceID != "later-main" {
		t.Fatal("original closure and later Stop were not consumed", history, err)
	}
	after, err := os.ReadFile(ref.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("history observation changed business unknown or custody bytes", err)
	}
	if state.Records[0].Exec.Result.ExitObserved || state.Records[0].Stopped != nil || state.Records[len(state.Records)-1].Worker.Boundary != OpenShellBoundaryUnknown {
		t.Fatal("fixture lost the independent business uncertainty")
	}
}

func TestSourcePhysicalHistoryRefusesMissingStopOrChangedPhysicalIdentity(t *testing.T) {
	_, ref := physicalHistoryFixture(t)
	baseline, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"old-id", "old-main", "missing-original", "later-ack", "later-observation", "later-pending", "v5", "pending-retained", "missing-handover"} {
		t.Run(fault, func(t *testing.T) {
			var changed OpenShellCustodyState
			if err := json.Unmarshal(baseline, &changed); err != nil {
				t.Fatal(err)
			}
			last := &changed.Records[len(changed.Records)-1]
			switch fault {
			case "old-id":
				changed.Records[0].Worker.Identity.Sandbox.SandboxID = "other-id"
			case "old-main":
				changed.Records[0].MainProcessInstanceID = "other-main"
			case "missing-original":
				changed.Records = changed.Records[1:]
			case "later-ack":
				last.StopAcknowledged = false
			case "later-observation":
				last.Stopped = nil
			case "later-pending":
				last.Worker.StopPending = true
			case "v5":
				changed.Version = 5
			case "pending-retained":
				changed.PendingRetained = []OpenShellRetainedSandbox{{Name: "pending", SandboxID: "pending-id", Workspace: changed.Binding.Workspace}}
			case "missing-handover":
				changed.Handover = nil
			}
			current := storageFailureWrite(t, ref.Path, changed)
			before, err := os.ReadFile(ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := InspectOpenShellPhysicalHistory(current); err == nil {
				t.Fatal("missing or mismatched physical evidence accepted", fault)
			}
			after, err := os.ReadFile(ref.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed original bytes", err)
			}
		})
	}
}
