package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type vmRetirementFixtureConfig struct {
	Storage openShellVMStorage
	Mode    string
}

// This server runs only in the existing fixture child. Its deletion target is
// an empty directory under that child's own temporary root, never a live VM.
func configureVMRetirementFixture(t *testing.T, root string, g *custodyGateway, native *openShellNativeServer) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "vm-retirement-fixture.json"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var config vmRetirementFixtureConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || config.Storage.Root != filepath.Join(canonical, "vm") {
		t.Fatal("VM fixture storage is outside its original temporary root", err)
	}
	create := native.create
	native.create = func(ctx context.Context, request *pb.CreateSandboxRequest) (*pb.SandboxResponse, error) {
		response, err := create(ctx, request)
		if err == nil {
			g.mu.Lock()
			g.sandboxes[request.Name].Metadata.ResourceVersion = 1
			g.mu.Unlock()
			response.Sandbox.Metadata.ResourceVersion = 1
		}
		return response, err
	}
	// The old gateway really can delete by name. This control would expose an
	// unsafe fallback, rather than letting a second UNIMPLEMENTED hide it.
	native.deleteLegacy = func(ctx context.Context, request *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if err := os.WriteFile(filepath.Join(root, "legacy-vm-deletions"), []byte(request.Name), 0600); err != nil {
			return nil, err
		}
		original := g.sandboxes[request.Name]
		if original == nil {
			return nil, status.Error(codes.NotFound, "legacy target absent")
		}
		id := original.GetMetadata().GetId()
		delete(g.sandboxes, request.Name)
		if err := os.Remove(filepath.Join(config.Storage.Root, "sandboxes", id)); err != nil {
			return nil, err
		}
		return &pb.DeleteSandboxResponse{SandboxId: id, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
	}
	if config.Mode == "old" {
		return
	}
	native.deleteMatches = func(ctx context.Context, request *pb.DeleteStoppedSandboxIfMatchesRequest) (*pb.DeleteStoppedSandboxIfMatchesResponse, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		file, err := os.OpenFile(filepath.Join(root, "vm-deletions"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := fmt.Fprintln(file, request.ExpectedSandboxId)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return nil, fmt.Errorf("fixture deletion log: %v %v", writeErr, closeErr)
		}
		original := g.sandboxes[request.Name]
		if original != nil && config.Mode == "replace" {
			original.Metadata.Id = "foreign-replacement"
		}
		if original != nil && config.Mode == "start" {
			original.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY
			original.Metadata.ResourceVersion++
		}
		if request.ExpectedResourceVersion == 0 || original == nil || original.GetMetadata().GetResourceVersion() != request.ExpectedResourceVersion || original.GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
			return nil, status.Error(codes.Aborted, "original Stopped observation changed")
		}
		if request.WorkspaceScope.GetWorkspace() != "owned-workspace" || original == nil || original.GetMetadata().GetId() != request.ExpectedSandboxId || original.GetMetadata().GetWorkspace() != request.WorkspaceScope.GetWorkspace() || request.RequestId == "" {
			return nil, status.Error(codes.Aborted, "original identity differs")
		}
		if config.Mode == "accepted" {
			return &pb.DeleteStoppedSandboxIfMatchesResponse{SandboxId: request.ExpectedSandboxId, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_ACCEPTED}, nil
		}
		delete(g.sandboxes, request.Name)
		if config.Mode != "keep-disk" {
			if err := os.Remove(filepath.Join(config.Storage.Root, "sandboxes", request.ExpectedSandboxId)); err != nil {
				return nil, err
			}
		}
		if config.Mode == "lost-ack" {
			return nil, status.Error(codes.Unavailable, "fixture ACK lost after original effect")
		}
		return &pb.DeleteStoppedSandboxIfMatchesResponse{SandboxId: request.ExpectedSandboxId, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
	}
}

func retirementRuntimeFixture(t *testing.T, mode string) (*OpenShellRuntime, CompletedSessionBinding, *managedFixture, openShellVMStorage) {
	t.Helper()
	var storage openShellVMStorage
	r, f, target := openShellRuntimeFixture(t, func(f *managedFixture) {
		root, err := filepath.EvalSymlinks(f.root)
		if err != nil {
			t.Fatal(err)
		}
		storage.Root = filepath.Join(root, "vm")
		if err := os.MkdirAll(filepath.Join(storage.Root, "sandboxes"), 0700); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(root, "vm-gateway.toml")
		if err := os.WriteFile(config, []byte(fmt.Sprintf("[openshell.gateway]\ncompute_driver='vm'\n[openshell.drivers.vm]\nstate_dir=%q\n", storage.Root)), 0600); err != nil {
			t.Fatal(err)
		}
		storage.Config, err = ReadSessionReadbackFile(config, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.spec.ComputeDriver = "vm"
		f.spec.Binding.ConfigSource = storage.Config.Path
		writeReadbackJSON(t, filepath.Join(f.root, "vm-retirement-fixture.json"), vmRetirementFixtureConfig{Storage: storage, Mode: mode})
		managedMarker(t, f.root, "completion-observation")
	})
	r.config.Profile.SourcePath, r.config.Profile.SourceSHA256 = storage.Config.Path, storage.Config.SHA256
	r.config.Profile.sourceFiles = map[string]string{storage.Config.Path: storage.Config.SHA256}
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "retirement", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true, EnvVars: map[string]string{"NOODLE_ORDER_ID": "retirement", "NOODLE_STAGE_INDEX": "0"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("retirement fixture dispatch did not settle")
	}
	if h.Outcome().Status != StatusCompleted {
		t.Fatal("retirement fixture failed", h.Outcome())
	}
	b := CompletedSessionBinding{OrderID: "retirement", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}
	writeReadbackJSON(t, filepath.Join(r.config.RuntimeDir, "sessions", h.ID(), "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
	state := f.owner.custody.Snapshot()
	if len(state.Records) != 1 {
		t.Fatal("fixture original VM identity is ambiguous")
	}
	if err := os.Mkdir(filepath.Join(storage.Root, "sandboxes", state.Records[0].Worker.Identity.Sandbox.SandboxID), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(filepath.Join(f.root, "fixture-retirement-hold")); err == nil {
			return // Expected preservation: only the owning fixture child exits.
		}
		if _, err := f.owner.Release("a"); err != nil {
			t.Error(err)
		}
	})
	return r, b, f, storage
}

func TestCleanupVMRetirementPreservesOriginalEvidenceWithoutReplay(t *testing.T) {
	for _, mode := range []string{"completed", "lost-ack", "keep-disk", "accepted", "old"} {
		t.Run(mode, func(t *testing.T) {
			r, b, f, storage := retirementRuntimeFixture(t, mode)
			result, err := r.RetireCleanupSession(t.Context(), b, b.WorktreePath, StatusCompleted)
			pending := mode == "keep-disk" || mode == "accepted" || mode == "old"
			if pending {
				if err == nil || result.State == CleanupSessionRetired {
					t.Fatalf("unresolved resource claimed retired: %+v %v", result, err)
				}
			} else if err != nil || result.State != CleanupSessionRetired || result.SessionID != b.SessionID || result.NativeAttemptID == "" || result.SandboxID == "" {
				t.Fatalf("original retirement missing: %+v %v", result, err)
			}
			if mode == "keep-disk" {
				if err := os.Remove(filepath.Join(storage.Root, "sandboxes", result.SandboxID)); err != nil {
					t.Fatal(err)
				}
				pending = false
			}
			again, againErr := r.RetireCleanupSession(t.Context(), b, b.WorktreePath, StatusCompleted)
			if !pending && (againErr != nil || again.State != CleanupSessionRetired) {
				t.Fatalf("original effect readback failed: %+v %v", again, againErr)
			}
			calls, err := os.ReadFile(filepath.Join(f.root, "vm-deletions"))
			if _, err := os.Stat(filepath.Join(f.root, "legacy-vm-deletions")); !os.IsNotExist(err) {
				t.Fatal("unsafe name-only fallback was attempted", err)
			}
			if mode == "old" {
				if !os.IsNotExist(err) {
					t.Fatal("old server started a deletion", err)
				}
			} else if err != nil || len(strings.Fields(string(calls))) != 1 {
				t.Fatalf("original delete was replayed: %q %v", calls, err)
			}
			readback, err := r.ReadCleanupSession(t.Context(), b, b.WorktreePath, StatusCompleted)
			if err != nil || readback.Execution.Retirement == nil || readback.Execution.NativePhase == "Stopped" {
				t.Fatalf("retirement replaced or lost original evidence: %+v %v", readback, err)
			}
			assertOriginalReadbackArchives(t, readback.Execution.Sources)
			saved := f.owner.custody.Snapshot()
			if saved.Version != openShellRetirementCustodyVersion {
				t.Fatal("older custody readers could discard the retirement intent")
			}
			if mode == "lost-ack" && (saved.Records[0].Retirement.Acknowledged || saved.Records[0].Retirement.Failure == "") {
				t.Fatal("lost acknowledgement or failure history was rewritten")
			}
			if err := validateOpenShellCustodyLedger(saved); err != nil {
				t.Fatal("retirement ledger cannot be reopened", err)
			}
			f.owner.custody.mu.Lock()
			data, readErr := os.ReadFile(filepath.Join(f.spec.Binding.StateRoot, "owner.json"))
			if readErr == nil {
				readErr = json.Unmarshal(data, &f.owner.custody.state)
			}
			if readErr != nil {
				f.owner.custody.mu.Unlock()
				t.Fatal(readErr)
			}
			err = f.owner.custody.initialize(t.Context())
			f.owner.custody.mu.Unlock()
			if err != nil {
				t.Fatal("original owner startup cannot read retired resources", err)
			}
		})
	}
}

func TestVMRetirementSchemaPreservesCredentialAuthorityAndRefusesDowngrade(t *testing.T) {
	r, b, f, _ := retirementRuntimeFixture(t, "completed")
	if _, err := r.RetireCleanupSession(t.Context(), b, b.WorktreePath, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	state := f.owner.custody.Snapshot()
	foreign := f.owner.custody.Snapshot()
	foreign.Records[0].Retirement.Storage.Config.Path = filepath.Join(t.TempDir(), "foreign.toml")
	if err := validateOpenShellCustodyLedger(foreign); err == nil {
		t.Fatal("retirement adopted another owner's VM configuration")
	}
	state.Version = openShellCustodyVersion
	if err := validateOpenShellCustodyLedger(state); err == nil {
		t.Fatal("retirement history accepted in an older schema")
	}
	state.Version = openShellCredentialRetirementCustodyVersion
	if openShellCustodySupportsProfile(state, OpenShellGatewaySpec{}) {
		t.Fatal("ordinary client resumed credential-capable custody")
	}
	spec := OpenShellGatewaySpec{CredentialSource: &OpenShellCredentialSource{BindingSHA256: "original-source"}}
	if !openShellCustodySupportsProfile(state, spec) {
		t.Fatal("original credential source could not read retired custody")
	}
}

func TestCleanupVMRetirementRefusesActiveOrMismatchedOriginalTask(t *testing.T) {
	r, b, f, _ := retirementRuntimeFixture(t, "completed")
	wrong := b
	wrong.OrderID = "foreign-task"
	if _, err := r.RetireCleanupSession(t.Context(), wrong, b.WorktreePath, StatusCompleted); err == nil {
		t.Fatal("retired a foreign task")
	}
	if _, err := r.RetireCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed); err == nil {
		t.Fatal("retired an outcome different from its canonical attempt")
	}
	r.mu.Lock()
	r.active[b.SessionID] = &openShellSession{}
	r.mu.Unlock()
	_, err := r.RetireCleanupSession(t.Context(), b, b.WorktreePath, StatusCompleted)
	r.mu.Lock()
	delete(r.active, b.SessionID)
	r.mu.Unlock()
	if err == nil {
		t.Fatal("retired a still-active original session")
	}
	if f.owner.custody.Snapshot().Records[0].Retirement != nil {
		t.Fatal("refused task saved a retirement intent")
	}
	if _, err := os.Stat(filepath.Join(f.root, "vm-deletions")); !os.IsNotExist(err) {
		t.Fatal("refused task started native deletion", err)
	}
}

func TestCleanupVMRetirementRejectsSameNameForeignReplacement(t *testing.T) {
	for _, mode := range []string{"replace", "start"} {
		t.Run(mode, func(t *testing.T) {
			r, b, f, storage := retirementRuntimeFixture(t, mode)
			managedMarker(t, f.root, "fixture-retirement-hold")
			result, err := r.RetireCleanupSession(t.Context(), b, b.WorktreePath, StatusCompleted)
			if err == nil || result.State == CleanupSessionRetired {
				t.Fatal("foreign replacement was accepted", result, err)
			}
			if present, err := storage.sandboxPresent(result.SandboxID); err != nil || !present {
				t.Fatal("refused deletion removed the original disk", present, err)
			}
		})
	}
}

func TestCleanupVMRetirementRetainsArchivesAfterCheckoutRemoval(t *testing.T) {
	r, b, f, _ := retirementRuntimeFixture(t, "completed")
	if _, err := r.RetireCleanupSession(t.Context(), b, b.WorktreePath, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(t.TempDir(), "control")
	command := exec.Command("git", "clone", "--quiet", "--no-hardlinks", b.WorktreePath, control)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clone original retained fixture: %v %s", err, output)
	}
	if err := os.RemoveAll(b.WorktreePath); err != nil {
		t.Fatal(err)
	}
	readback, err := r.ReadCleanupSession(t.Context(), b, control, StatusCompleted)
	if err != nil || readback.Execution.Retirement == nil || readback.Execution.Retirement.State != CleanupSessionRetired || readback.Execution.NativePhase != "Retired" {
		t.Fatal("retired VM broke original evidence readback", readback, err)
	}
	assertOriginalReadbackArchives(t, readback.Execution.Sources)
	for key, ref := range readback.Execution.Sources {
		if !strings.HasPrefix(key, "progress_archive_") {
			continue
		}
		managedMarker(t, f.root, "fixture-retirement-hold")
		file, err := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write([]byte("changed retained evidence"))
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal(writeErr, closeErr)
		}
		if _, err := r.ReadCleanupSession(t.Context(), b, control, StatusCompleted); err == nil {
			t.Fatal("retired VM hid corrupted original evidence")
		}
		break
	}
}
