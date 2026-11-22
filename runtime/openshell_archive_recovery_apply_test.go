package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

func TestApplyOpenShellArchiveRecovery(t *testing.T) {
	for _, mode := range []string{"confirmed", "credential refresh after Stop", "resident observed same Stop", "resident observed different compute", "wrong VM", "changed archive", "missing stop ACK after credential refresh", "current custody changed after qualification"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target, session := filepath.Join(root, "repo"), filepath.Join(root, "original-session")
			for _, directory := range []string{target, session, filepath.Join(root, "capture")} {
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = target
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %q: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "--quiet", "--template=", "--initial-branch=integration")
			git("config", "user.name", "Noodle Fixture")
			git("config", "user.email", "fixture@example.invalid")
			git("commit", "--allow-empty", "-m", "frozen baseline")
			baseline, err := workspaceartifacts.Capture(ctx, target, filepath.Join(root, "baseline"))
			if err != nil {
				t.Fatal(err)
			}
			fileRef := func(path string) SessionReadbackFileRef {
				t.Helper()
				ref, err := ReadSessionReadbackFile(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				return ref
			}
			write := func(path string, value any) SessionReadbackFileRef {
				t.Helper()
				if err := maintenanceWrite(path, value); err != nil {
					t.Fatal(err)
				}
				return fileRef(path)
			}
			archive := filepath.Join(root, "capture", "workspace.tar")
			if err := os.WriteFile(archive, progressFixtureArchive(t, "noodle-result.tar"), 0600); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(root, "reviewed")
			manifest, err := workspaceartifacts.ExtractWithManifest(ctx, archive, restored)
			if err != nil {
				t.Fatal(err)
			}
			manifestRef := write(filepath.Join(root, "capture", "workspace-manifest.json"), manifest)
			scope := workspaceartifacts.LocalTaskScope{IntegrationBranch: "integration", WritePaths: []string{"result.txt"}}
			tree, err := workspaceartifacts.InspectArchiveCandidate(ctx, target, filepath.Join(root, "scratch"), restored, baseline, scope)
			if err != nil {
				t.Fatal(err)
			}
			const name, id, main = "owned-name", "native-vm", "original-main"
			var stopped atomic.Bool
			sandbox := func() *pb.Sandbox {
				s := custodySandbox(name, id, pb.SandboxPhase_SANDBOX_PHASE_READY)
				s.Status.MainProcessInstanceId = main
				if mode == "resident observed different compute" {
					s.Status.AgentPod = "original-compute"
				}
				if stopped.Load() {
					s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED
				}
				return s
			}
			server := &openShellNativeServer{
				list: func(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
					return &pb.ListSandboxesResponse{Sandboxes: []*pb.Sandbox{sandbox()}}, nil
				},
				get: func(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
					return &pb.SandboxResponse{Sandbox: sandbox()}, nil
				},
				stop: func(context.Context, *pb.StopSandboxRequest) (*pb.SandboxResponse, error) {
					stopped.Store(true)
					return &pb.SandboxResponse{Sandbox: sandbox()}, nil
				},
			}
			vm, tls := openShellTestGateway(t, server)
			conn, err := newOpenShellConnection(vm.Sandbox.Gateway, tls)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			spec, err := openShellBytes(sandbox().GetSpec())
			if err != nil {
				t.Fatal(err)
			}
			record := OpenShellCustodyRecord{Name: name, MainProcessInstanceID: main, Spec: spec, Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{Sandbox: vm.Sandbox}}}
			state := OpenShellCustodyState{Binding: OpenShellCustodyBinding{Workspace: vm.Sandbox.Workspace}, Records: []OpenShellCustodyRecord{record}}
			in := storageFailureInputs{state: state, record: record}
			custodyPath := filepath.Join(root, "owner.json")
			historicalCustody := write(custodyPath, state)
			currentCustody := historicalCustody
			capture := OpenShellWorkspaceCapture{OrderID: "original-order", NativeAttemptID: "native-attempt", SessionID: "original-session", SandboxID: id, MainProcessInstanceID: main, Archive: fileRef(archive), Sources: map[string]SessionReadbackFileRef{"workspace_manifest": manifestRef, "custody": historicalCustody}}
			captureRef := write(filepath.Join(root, "capture", "workspace-capture.json"), capture)
			reviewRef := write(filepath.Join(root, "review.json"), map[string]any{"schema": 1, "status": "archive_recovery_review_evidence", "original_return": "UNKNOWN", "attempt_id": "logical-attempt", "capture": captureRef, "tree": tree, "authorizes_landing": false})
			p := ProgressMaintenance{SessionDirectory: session, Request: write(filepath.Join(root, "request.json"), map[string]string{"identity": "original"})}
			hold := ArchiveRecoveryDispatchHold{PauseCommandID: "original-pause"}
			checkOwner := func() error { return nil }
			stopDir := filepath.Join(session, "archive-recovery-stop")
			stop, err := stopOpenShellArchiveRecovery(ctx, pb.NewOpenShellClient(conn), in, p, reviewRef, captureRef, hold, stopDir, checkOwner)
			if err != nil {
				t.Fatal(err)
			}
			input := OpenShellArchiveRecoveryApplyInput{Review: reviewRef, Capture: captureRef, Stop: stop, AttemptID: "logical-attempt", DispatchHold: hold, Tree: tree, Scope: scope, Target: target}
			refreshCredentials := func() {
				t.Helper()
				state.Credentials = append(state.Credentials, OpenShellCredentialDelivery{Provider: "renewed fixture"})
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(custodyPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var want string
			switch mode {
			case "credential refresh after Stop":
				refreshCredentials()
				currentCustody = fileRef(custodyPath)
			case "resident observed same Stop", "resident observed different compute":
				in.record.Stopped = openShellStoppedObservation(sandbox())
				if mode == "resident observed different compute" {
					in.record.Stopped.ComputeInstanceID = "foreign-compute"
					want = "stopped compute instance differs from custody"
				}
				in.state.Records[0] = in.record
				state.Records[0] = in.record
				refreshCredentials()
				currentCustody = fileRef(custodyPath)
			case "current custody changed after qualification":
				want = "maintenance source changed"
				refreshCredentials()
			case "wrong VM":
				want = "sandbox identity changed"
			case "changed archive":
				want = "archive or manifest differs"
				if err := os.WriteFile(archive, append(progressFixtureArchive(t, "noodle-result.tar"), make([]byte, 512)...), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing stop ACK after credential refresh":
				refreshCredentials()
				currentCustody = fileRef(custodyPath)
				want = "native-stop-ack.json"
				if err = os.Remove(filepath.Join(stopDir, "native-stop-ack.json")); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			observe := func(context.Context) (*pb.SandboxResponse, error) {
				reads++
				s := sandbox()
				if mode == "wrong VM" {
					s.Status.MainProcessInstanceId = "foreign-main"
				}
				return &pb.SandboxResponse{Sandbox: s}, nil
			}
			apply := func() (SessionReadbackFileRef, error) {
				return applyOpenShellArchiveRecovery(ctx, p, input, capture, baseline, in, currentCustody, checkOwner, observe)
			}
			originalStop := fileRef(stop.Path)
			result, err := apply()
			if want != "" {
				if err == nil || !strings.Contains(err.Error(), want) || git("rev-parse", "HEAD") != baseline.Head || git("status", "--porcelain") != "" {
					t.Fatalf("expected refusal %q and original untouched checkout: %v", want, err)
				}
				if _, err := os.Lstat(filepath.Join(session, "archive-recovery-apply-intent.json")); !os.IsNotExist(err) {
					t.Fatalf("refused attempt reserved an Apply: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var candidate archiveRecoveryCandidate
			if err := maintenanceVerify(result, &candidate); err != nil {
				t.Fatal(err)
			}
			if candidate.OriginalReturn != "UNKNOWN" || candidate.AuthorizesLanding || candidate.Request != p.Request || candidate.Stop != originalStop || candidate.CandidateTree != git("rev-parse", "HEAD^{tree}") || git("rev-list", "--count", baseline.Head+"..HEAD") != "1" || git("status", "--porcelain") != "" {
				t.Fatalf("unexpected reviewed local candidate: %+v", candidate)
			}
			if again, err := apply(); err != nil || again != result || reads != 1 {
				t.Fatalf("observed Apply replayed or changed: %v", err)
			}
			if err := os.Remove(result.Path); err != nil {
				t.Fatal(err)
			}
			if _, err := apply(); err == nil || !strings.Contains(err.Error(), "UNKNOWN") || reads != 1 || git("rev-list", "--count", baseline.Head+"..HEAD") != "1" {
				t.Fatalf("lost completion was retried or declared complete: %v", err)
			}
		})
	}
}
