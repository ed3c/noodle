package runtime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

func TestOpenShellWorkspaceCapture(t *testing.T) {
	for _, test := range []struct {
		name, refusal string
		changeAfter   bool
		truncate      bool
	}{
		{name: "restorable"},
		{name: "replacement", refusal: "native sandbox identity changed", changeAfter: true},
		{name: "truncated", refusal: "archive end marker is missing", truncate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := progressFixtureArchive(t, "noodle-result.tar")
			if test.truncate {
				archive = archive[:len(archive)-1024]
			}
			server := &openShellNativeServer{}
			server.get = func(_ context.Context, req *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
				if req.GetName() != "owned-name" || req.GetWorkspaceScope().GetWorkspace() != "owned-workspace" {
					t.Error("capture changed original sandbox locator")
				}
				sandbox := custodySandbox("owned-name", "native-vm", pb.SandboxPhase_SANDBOX_PHASE_READY)
				sandbox.Status.MainProcessInstanceId = "original-main"
				if test.changeAfter && server.gets.Load() == 3 {
					sandbox.Status.MainProcessInstanceId = "foreign-main"
				}
				return &pb.SandboxResponse{Sandbox: sandbox}, nil
			}
			server.exec = func(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
				start, err := openShellReceiveStart(stream)
				if err != nil {
					return err
				}
				want := []string{"tar", "--hard-dereference", "-cf", "-", "-C", "/sandbox/repo", "--", "."}
				if !slices.Equal(start.GetCommand(), want) || start.GetWorkdir() != "/sandbox/repo" || start.GetTty() || !start.GetNoLoginShell() || len(start.GetEnvironment()) != 0 {
					t.Error("capture changed fixed command, directory, or credentials")
				}
				if err = stream.Send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Stdout{Stdout: &pb.ExecSandboxStdout{Data: archive}}}); err != nil {
					return err
				}
				return stream.Send(openShellExit(0))
			}
			target, tls := openShellTestGateway(t, server)
			conn, err := newOpenShellConnection(target.Sandbox.Gateway, tls)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			spec, err := openShellBytes(custodySandbox(target.Name, target.Sandbox.SandboxID, pb.SandboxPhase_SANDBOX_PHASE_READY).Spec)
			if err != nil {
				t.Fatal(err)
			}
			record := OpenShellCustodyRecord{Task: OpenShellTask{"order-original", "attempt-original", "session-original"}, Name: target.Name, Spec: spec, MainProcessInstanceID: "original-main", Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{Sandbox: target.Sandbox, SandboxName: target.Name, ConfigSource: target.ConfigSource}}}
			baseline := &workspaceartifacts.Baseline{Head: "original-head", IndexSHA256: "original-index"}
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(parent, "archive-"+test.name)
			ref, err := captureWorkspaceArchive(t.Context(), pb.NewOpenShellClient(conn), record, baseline, output, map[string]SessionReadbackFileRef{}, func() error { return nil })
			if test.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), test.refusal) {
					t.Fatalf("expected refusal %q, got %v", test.refusal, err)
				}
				if _, err := os.Lstat(output); !os.IsNotExist(err) {
					t.Fatal("partial output was retained as an archive")
				}
				return
			}
			if err != nil || server.gets.Load() != 3 || server.execs.Load() != 1 {
				t.Fatalf("capture did not prove original native stream and both identities: %v", err)
			}
			verifyWorkspaceCaptureReceipt(t, ref, archive)
		})
	}
}

func verifyWorkspaceCaptureReceipt(t *testing.T, ref SessionReadbackFileRef, raw []byte) {
	t.Helper()
	var receipt struct {
		OriginalReturn string                            `json:"original_return"`
		BaselineHead   string                            `json:"baseline_head"`
		Archive        SessionReadbackFileRef            `json:"archive"`
		Sources        map[string]SessionReadbackFileRef `json:"sources"`
	}
	if err := maintenanceVerify(ref, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.OriginalReturn != "UNKNOWN" || receipt.BaselineHead != "original-head" {
		t.Fatalf("capture changed original result or baseline: %+v", receipt)
	}
	var manifest workspaceartifacts.VerifiedArchiveManifest
	if err := maintenanceVerify(receipt.Sources["workspace_manifest"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SHA256 != receipt.Archive.SHA256 || len(manifest.Entries) != 1 || manifest.Entries["result.txt"].Bytes != int64(len("accepted output\n")) {
		t.Fatalf("incomplete workspace manifest: %+v", manifest)
	}
	got, err := os.ReadFile(receipt.Archive.Path)
	if err != nil || !slices.Equal(got, raw) {
		t.Fatal("archive bytes differ from the native stream", err)
	}
}
