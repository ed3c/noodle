package runtime

import (
	"archive/tar"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
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
		return
	}
	source, dest := args[len(args)-2], args[len(args)-1]
	f, err := os.Create(filepath.Join(dest, filepath.Base(source)))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	name, data := "result.txt", "accepted output\n"
	if filepath.Base(source) == "noodle-session.tar" {
		name, data = "native.jsonl", "native session evidence\n"
	}
	if err = tw.WriteHeader(&tar.Header{Name: "./" + name, Size: int64(len(data)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err = tw.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
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
	for _, args := range [][]string{{"init", "--quiet", "--template="}, {"-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "baseline"}} {
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
	hash, err := openShellPayloadHash(payload)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewOpenShellRuntime(OpenShellRuntimeConfig{ContextID: "a", RuntimeDir: filepath.Join(root, "runtime"), Gateway: owner, MaxConcurrent: 1, Profile: OpenShellProfile{CLI: cli, GatewayName: "fixture", Environment: []string{"NOODLE_TRANSFER_CHILD=1", "XDG_CONFIG_HOME=" + filepath.Join(root, "client")}, PayloadDirectory: payload, PayloadSHA256: hash, Routes: map[string]OpenShellRoute{"codex": {Binary: "/sandbox/payload/codex", Spec: &pb.SandboxSpec{Command: []string{"hold"}}}}}})
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
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "formal", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target})
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
