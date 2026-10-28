package runtime

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/guestprogress"
)

func TestOpenShellTransferChild(t *testing.T) {
	if os.Getenv("NOODLE_TRANSFER_CHILD") != "1" {
		return
	}
	args := os.Args
	direction := ""
	for _, v := range args {
		if v == "upload" || v == "download" {
			direction = v
		}
	}
	if direction == "upload" {
		if filename := os.Getenv("NOODLE_TRANSFER_LOG"); filename != "" {
			f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintln(f, args[len(args)-2]); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	source, dest := args[len(args)-2], args[len(args)-1]
	// The native CLI only downloads from its sandbox workspace.
	if !strings.HasPrefix(source, "/sandbox/") || path.Clean(source) != source {
		fmt.Fprintln(os.Stderr, "download source is outside the sandbox workspace (/sandbox)")
		os.Exit(1)
	}
	var content []byte
	if filepath.Base(source) == "result.json" {
		data, err := os.ReadFile(os.Getenv("NOODLE_PROGRESS_FIXTURE_CUSTODY"))
		if err != nil {
			t.Fatal(err)
		}
		var state OpenShellCustodyState
		if err = json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		record := state.Records[len(state.Records)-1]
		_, binding, err := unwrapOpenShellProgress(record.Exec.Command)
		if err != nil {
			t.Fatal(err)
		}
		tokenHash := sha256.Sum256([]byte(binding.Token))
		receipt := guestprogress.Receipt{Schema: 1, Identity: guestprogress.Identity{SessionID: binding.SessionID, CommandSHA256: binding.CommandSHA256, TokenSHA256: hex.EncodeToString(tokenHash[:])}, Status: "archived", ChildrenReaped: true}
		if record.ProgressStop != nil {
			receipt.RequestID = record.ProgressStop.Command.RequestID
			receipt.ExitCode = 143
		}
		for _, name := range []string{"noodle-result.tar", "noodle-session.tar"} {
			data := progressFixtureArchive(t, name)
			hash := sha256.Sum256(data)
			receipt.Archives = append(receipt.Archives, guestprogress.Archive{Name: name, SHA256: hex.EncodeToString(hash[:]), Bytes: int64(len(data))})
		}
		content, err = json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		content = progressFixtureArchive(t, filepath.Base(source))
		if os.Getenv("NOODLE_PROGRESS_FIXTURE_BAD_ARCHIVE") == "1" {
			content = append(content, 'x')
		}
	}
	if err := os.WriteFile(filepath.Join(dest, filepath.Base(source)), content, 0600); err != nil {
		t.Fatal(err)
	}
}
func progressFixtureArchive(t *testing.T, archive string) []byte {
	t.Helper()
	var output bytes.Buffer
	tw := tar.NewWriter(&output)
	name, data := "result.txt", "accepted output\n"
	if archive == "noodle-session.tar" {
		name, data = "native.jsonl", "native session evidence\n"
	}
	if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Size: int64(len(data)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
func openShellRuntimeFixture(t *testing.T) (*OpenShellRuntime, *managedFixture, string) {
	t.Helper()
	f := newManagedFixture(t, true)
	managedMarker(t, f.root, "runtime-dispatch")
	owner := f.open(t)
	root := t.TempDir()
	target := filepath.Join(root, "repo")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet", "--template="}, {"config", "user.name", "Noodle Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "baseline"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = target
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(root, "transfer")
	if err = os.WriteFile(cli, []byte("#!/bin/sh\nexec "+strconv.Quote(executable)+" -test.run=^TestOpenShellTransferChild$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "payload")
	if err = os.Mkdir(payload, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, "noodle-progress"), []byte("fixture supervisor"), 0700); err != nil {
		t.Fatal(err)
	}
	hash, err := openShellPayloadHash(payload)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewOpenShellRuntime(OpenShellRuntimeConfig{ContextID: "a", RuntimeDir: filepath.Join(root, "runtime"), Gateway: owner, MaxConcurrent: 1, Profile: OpenShellProfile{ProgressSupervisor: "/sandbox/payload/noodle-progress", CLI: cli, GatewayName: "fixture", Environment: []string{"NOODLE_PROGRESS_FIXTURE_CUSTODY=" + filepath.Join(f.spec.Binding.StateRoot, "owner.json"), "NOODLE_TRANSFER_CHILD=1", "XDG_CONFIG_HOME=" + filepath.Join(root, "client")}, PayloadDirectory: payload, PayloadSHA256: hash, Routes: map[string]OpenShellRoute{"codex": {Binary: "/sandbox/payload/codex", Spec: &pb.SandboxSpec{Command: []string{"hold"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	runtime.config.Profile.Gateway = f.spec
	client := filepath.Join(root, "client", "openshell", "gateways", "fixture")
	if err := os.MkdirAll(filepath.Join(client, "mtls"), 0700); err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(f.spec.TLS.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	var cert []byte
	for _, der := range f.spec.TLS.Certificate.Certificate {
		cert = append(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	metadata, _ := json.Marshal(map[string]string{"name": "fixture", "gateway_endpoint": f.spec.Binding.Gateway, "auth_mode": "mtls"})
	for name, data := range map[string][]byte{"metadata.json": metadata, "mtls/ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.spec.TLS.Certificate.Certificate[1]}), "mtls/tls.crt": cert, "mtls/tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})} {
		if err := os.WriteFile(filepath.Join(client, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	runtime.config.Profile.ClientSHA256, err = openShellClientHash(runtime.config.Profile)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, f, target
}
func TestOpenShellRuntimeReturnsArtifactsAfterOriginalStop(t *testing.T) {
	r, f, target := openShellRuntimeFixture(t)
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "formal", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("attempt unresolved: %+v", h.Outcome())
	}
	if h.Outcome().Status != StatusCompleted {
		t.Fatalf("outcome=%+v", h.Outcome())
	}
	data, err := os.ReadFile(filepath.Join(target, "result.txt"))
	if err != nil || string(data) != "accepted output\n" {
		t.Fatalf("returned artifact: %s %v", data, err)
	}
	s := h.(*openShellSession)
	if s.receipt.CandidateHead == "" || s.receipt.CandidateHead == s.receipt.Baseline.Head {
		t.Fatal("returned changes were not committed for native review")
	}
	if s.worker.Record().Boundary != OpenShellBoundaryStopped {
		t.Fatal("delivery preceded original stopped boundary")
	}
	if !r.CapacityAvailable() {
		t.Fatal("settled attempt retained capacity")
	}
	if _, err := f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
}
func TestOpenShellRuntimeRecoveryStopsOnlyOnExplicitRequest(t *testing.T) {
	r, f, _ := openShellRuntimeFixture(t)
	c, err := f.owner.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	record := custodyCreate(t, c, "recovery")
	dir := filepath.Join(r.config.RuntimeDir, "sessions", record.Task.SessionID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := openShellDispatchReceipt{Version: 1, ContextID: "a", OrderID: record.Task.OrderID, AttemptID: record.Task.AttemptID, SessionID: record.Task.SessionID, Phase: "executing"}
	b, _ := json.Marshal(receipt)
	if err = os.WriteFile(filepath.Join(dir, "openshell-dispatch.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := r.Recover(t.Context())
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recovery %v %+v", err, recovered)
	}
	h := recovered[0].SessionHandle
	select {
	case <-h.Done():
		t.Fatal("live recovered VM treated as complete")
	default:
	}
	if err = h.Terminate(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("explicit recovered stop did not reach original VM")
	}
	if _, err = f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellRecoveryRetainsUncertainDelivery(t *testing.T) {
	for _, phase := range []string{"delivery-in-progress", "delivery-unknown", "publishing"} {
		t.Run(phase, func(t *testing.T) {
			r, f, _ := openShellRuntimeFixture(t)
			c, err := f.owner.Acquire(t.Context(), "a")
			if err != nil {
				t.Fatal(err)
			}
			record := custodyCreate(t, c, "pending-delivery")
			if err = c.StopSandbox(t.Context(), record.Worker.Identity.Sandbox); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(r.config.RuntimeDir, "sessions", record.Task.SessionID)
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(openShellDispatchReceipt{Version: 1, ContextID: "a", OrderID: record.Task.OrderID, AttemptID: record.Task.AttemptID, SessionID: record.Task.SessionID, Phase: phase})
			if err = os.WriteFile(filepath.Join(dir, "openshell-dispatch.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			recovered, err := r.Recover(t.Context())
			if err != nil || len(recovered) != 1 {
				t.Fatalf("recovery=%v %v", recovered, err)
			}
			h := recovered[0].SessionHandle
			_ = h.Terminate()
			select {
			case <-h.Done():
				t.Fatal("VM stop settled unknown local delivery")
			case <-time.After(20 * time.Millisecond):
			}
			if r.CapacityAvailable() {
				t.Fatal("uncertain publication allowed another attempt")
			}
		})
	}
}

func waitProgressFixture(t *testing.T, root, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("progress fixture did not reach %s", name)
}

func TestOpenShellRuntimeStopPreservesProgressAfterBothNativeExits(t *testing.T) {
	r, f, target := openShellRuntimeFixture(t)
	managedMarker(t, f.root, "completion-observation")
	managedMarker(t, f.root, "progress-await-stop")
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "interrupted", EnvVars: map[string]string{"NOODLE_ORDER_ID": "interrupted", "NOODLE_STAGE_INDEX": "0"}, Provider: "codex", Model: "fixture", Prompt: "produce partial result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	waitProgressFixture(t, f.root, "progress-supervisor-started")
	if err := h.Terminate(); err != nil {
		t.Fatal(err)
	}
	waitProgressFixture(t, f.root, "progress-control-started")
	session := h.(*openShellSession)
	assertRetained := func() {
		t.Helper()
		select {
		case <-h.Done():
			t.Fatal("pending native execution released session")
		default:
		}
		record := session.custody.Snapshot().Records[0]
		if record.StopRequestID != "" || record.Stopped != nil {
			t.Fatal("VM stopped before both native exits and artifact return")
		}
		if r.CapacityAvailable() {
			t.Fatal("pending stop released runtime slot")
		}
	}
	assertRetained()
	managedMarker(t, f.root, "progress-control-release")
	deadline := time.Now().Add(5 * time.Second)
	for {
		record := session.custody.Snapshot().Records[0]
		if record.ProgressStop != nil && record.ProgressStop.Result.ExitObserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control completion missing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertRetained()
	managedMarker(t, f.root, "progress-supervisor-release")
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("progress did not settle: %+v", h.Outcome())
	}
	if h.Outcome().Status != StatusCancelled || h.Outcome().ExitCode != 143 {
		t.Fatalf("interruption was mislabeled: %+v", h.Outcome())
	}
	session.mu.Lock()
	receipt := session.receipt
	session.mu.Unlock()
	if receipt.ProgressHead == "" || receipt.CandidateHead != "" || receipt.Progress == nil || receipt.Progress.ExitCode != 143 || receipt.Phase != "settled" || receipt.Worker.Boundary != OpenShellBoundaryStopped {
		t.Fatal("saved progress promoted to candidate or missing stop boundary")
	}
	data, err := os.ReadFile(filepath.Join(target, "result.txt"))
	if err != nil || string(data) != "accepted output\n" {
		t.Fatal("guest progress not restored to original worktree")
	}
	sessionData, err := os.ReadFile(filepath.Join(session.dir, "guest-session", "native.jsonl"))
	if err != nil || string(sessionData) != "native session evidence\n" {
		t.Fatal("guest session not retained")
	}
	writeReadbackJSON(t, filepath.Join(session.dir, "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
	binding := CompletedSessionBinding{OrderID: "interrupted", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}
	observed, err := r.ReadSavedProgress(t.Context(), binding)
	if err != nil || observed.Status != "saved" || observed.ProgressHead != receipt.ProgressHead || observed.HarnessExitCode != 143 || observed.OutcomeStatus != StatusCancelled {
		t.Fatalf("saved progress readback: %+v %v", observed, err)
	}
	if _, err := r.ReadCompletedSession(t.Context(), binding); err == nil {
		t.Fatal("interrupted progress accepted as successful candidate")
	}
	cleanup, cleanupErr := r.ReadCleanupSession(t.Context(), binding, target, StatusCancelled)
	if cleanupErr != nil || cleanup.OutcomeStatus != StatusCancelled || cleanup.PublishedHead != receipt.ProgressHead {
		t.Fatalf("cleanup lost saved interrupted outcome: %+v %v", cleanup, cleanupErr)
	}
	if _, err := r.ReadCleanupSession(t.Context(), binding, target, StatusCompleted); err == nil {
		t.Fatal("cleanup promoted interrupted progress to completion")
	}
	session.custody.mu.Lock()
	originalResult := session.custody.state.Records[0].ProgressStop.Result
	session.custody.state.Records[0].ProgressStop.Result.ExitObserved = false
	session.custody.mu.Unlock()
	if _, err := r.ReadSavedProgress(t.Context(), binding); err == nil {
		t.Fatal("original unknown control result replaced by saved JSON")
	}
	session.custody.mu.Lock()
	session.custody.state.Records[0].ProgressStop.Result = originalResult
	session.custody.mu.Unlock()
	if _, err := r.ReadSavedProgress(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellRuntimeRetainsUnknownProgress(t *testing.T) {
	for _, kind := range []string{"control", "archive"} {
		t.Run(kind, func(t *testing.T) {
			r, f, target := openShellRuntimeFixture(t)
			if kind == "control" {
				managedMarker(t, f.root, "progress-await-stop")
				managedMarker(t, f.root, "progress-control-unknown")
			} else {
				r.config.Profile.Environment = append(r.config.Profile.Environment, "NOODLE_PROGRESS_FIXTURE_BAD_ARCHIVE=1")
			}
			h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "unknown-progress", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "control" {
				waitProgressFixture(t, f.root, "progress-supervisor-started")
				if err := h.Terminate(); err != nil {
					t.Fatal(err)
				}
				waitProgressFixture(t, f.root, "progress-control-started")
				managedMarker(t, f.root, "progress-control-release")
				managedMarker(t, f.root, "progress-supervisor-release")
			}
			session := h.(*openShellSession)
			deadline := time.Now().Add(10 * time.Second)
			for {
				session.mu.Lock()
				phase := session.receipt.Phase
				session.mu.Unlock()
				if phase == "progress-unknown" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("unknown evidence was not retained: %+v", h.Outcome())
				}
				time.Sleep(5 * time.Millisecond)
			}
			select {
			case <-h.Done():
				t.Fatal("unknown progress released original session")
			default:
			}
			if r.CapacityAvailable() {
				t.Fatal("unknown progress released runtime slot")
			}
			record := session.custody.Snapshot().Records[0]
			if record.StopRequestID != "" || record.Stopped != nil {
				t.Fatal("unknown progress stopped VM")
			}
			if _, err := os.Stat(filepath.Join(target, "result.txt")); !os.IsNotExist(err) {
				t.Fatal("unknown progress applied local results")
			}
			if _, err := f.owner.Release("a"); err == nil {
				t.Fatal("unknown progress released gateway")
			}
		})
	}
}
