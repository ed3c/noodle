//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestReplacementGatePrecedesDriverPersistence(t *testing.T) {
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(contracts, &set); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		t.Fatal(err)
	}
	d, err := files.FindDescriptorByName("openshell.credentials.v1.StoreCredentialRequest")
	if err != nil {
		t.Fatal(err)
	}
	m := dynamicpb.NewMessage(d.(protoreflect.MessageDescriptor))
	if err := protojson.Unmarshal([]byte(`{"provider":"fixture","credential_key":"refresh-material","value":"fake-replacement"}`), m); err != nil {
		t.Fatal(err)
	}
	f := newDriverFixture(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer f.storeGate.open()
	done := make(chan error, 1)
	go func() {
		_, err := f.invoke(ctx, "openshell.credentials.v1.CredentialDriver", "StoreCredential", m)
		done <- err
	}()
	select {
	case <-f.storeGate.entered:
	case <-ctx.Done():
		t.Fatal("store gate not entered")
	}
	if _, err := os.Stat(filepath.Join(f.directory, "driver-values.json")); !os.IsNotExist(err) {
		t.Fatalf("store persisted before gate release: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("store returned before release: %v", err)
	default:
	}
	f.storeGate.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.directory, "driver-values.json"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if err := json.Unmarshal(b, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values["fixture-1"] != "fake-replacement" {
		t.Fatal("replacement not persisted")
	}
}

func TestGatewayEnvironmentExcludesAmbientCredentials(t *testing.T) {
	t.Setenv("OPENSHELL_GATEWAY_CONFIG", "/private/config")
	t.Setenv("CODEX_AUTH_ACCESS_TOKEN", "not-for-fixture")
	for _, entry := range isolatedEnv("/tmp/fixture") {
		if entry == "OPENSHELL_GATEWAY_CONFIG=/private/config" || entry == "CODEX_AUTH_ACCESS_TOKEN=not-for-fixture" {
			t.Fatal("ambient credential/config escaped into gateway")
		}
	}
}

func TestFailedBuildReceiptRefusesBeforeExecutingGateway(t *testing.T) {
	failed := 101
	for name, exitCode := range map[string]*int{"failed": &failed, "missing": nil} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			receipt := filepath.Join(dir, "build.json")
			binary := filepath.Join(dir, "gateway-does-not-exist")
			body, err := json.Marshal(buildReceipt{SchemaVersion: 1, Action: "build", ExitCode: exitCode, SourceUnchanged: true, Binary: binary, SourceTreeSHA256: strings.Repeat("a", 64), SourcePin: sourcePin{Upstream: "NVIDIA/OpenShell", Tag: "v0.1.5", Commit: "bdb956f2cd65a635f1294fccf1d138d263bf650f", ArchiveSHA256: "6299e61bfe41a5eb67985c5da986688b87c495a4602ca90ffcf9b90328afe29f", PatchSHA256: strings.Repeat("b", 64), BuildVersion: "0.1.5+noodle.refresh-drain.1"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(receipt, body, 0600); err != nil {
				t.Fatal(err)
			}
			err = run(binary, receipt, dir, &evidence{})
			if err == nil || !strings.Contains(err.Error(), "build receipt does not bind") {
				t.Fatalf("failed/incomplete build not refused before executable access: %v", err)
			}
		})
	}
}

func TestGroupTermRefusesChangedIdentityThenSignalsOriginalGroup(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &gatewayProcess{cmd: cmd, done: make(chan struct{}), pgid: cmd.Process.Pid}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	defer p.stopCleanup()
	p.pgid++
	if err := p.terminate(true); err == nil {
		t.Fatal("changed original group accepted")
	}
	select {
	case <-p.done:
		t.Fatal("identity refusal signalled child")
	default:
	}
	p.pgid = cmd.Process.Pid
	if err := p.terminate(true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("group TERM did not reach child")
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("unexpected exit: %v", p.err)
	}
	var e scenarioEvidence
	if err := p.verifyGroupExited(&e); err != nil {
		t.Fatal(err)
	}
	if err := p.terminate(true); err == nil {
		t.Fatal("exited original child accepted")
	}
}
