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
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// Transfers finish all I/O synchronously. Only this short-lived fixture child
// skips the race runtime's default one-second exit sleep per transfer; the
// parent's instrumentation and inherited race options remain intact.
func openShellTransferFixtureScript(executable string) []byte {
	return []byte("#!/bin/sh\nGORACE=\"${GORACE:+$GORACE }atexit_sleep_ms=0\" exec " + strconv.Quote(executable) + " -test.run=^TestOpenShellTransferChild$ -- \"$@\"\n")
}

func TestOpenShellTransferFixturePreservesParentAndChildRaceOptions(t *testing.T) {
	for _, options := range []string{"", "history_size=3 exitcode=71", "atexit_sleep_ms=1000 strip_path_prefix=/original/"} {
		t.Run(options, func(t *testing.T) {
			t.Setenv("GORACE", options)
			root := t.TempDir()
			child := filepath.Join(root, "child")
			if err := os.WriteFile(child, []byte("#!/bin/sh\nprintf '%s' \"$GORACE\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			shim := filepath.Join(root, "transfer")
			if err := os.WriteFile(shim, openShellTransferFixtureScript(child), 0700); err != nil {
				t.Fatal(err)
			}
			raw, err := exec.Command(shim).Output()
			if err != nil {
				t.Fatal(err)
			}
			want := options
			if want != "" {
				want += " "
			}
			want += "atexit_sleep_ms=0"
			if string(raw) != want {
				t.Fatalf("child race options: got %q, want %q", raw, want)
			}
			if os.Getenv("GORACE") != options {
				t.Fatal("transfer shim changed parent race options")
			}
		})
	}
}

func TestOpenShellTransferChild(t *testing.T) {
	if os.Getenv("NOODLE_TRANSFER_CHILD") != "1" {
		return
	}
	args := os.Args
	for flag, expected := range map[string]string{"--gateway": os.Getenv("NOODLE_TRANSFER_GATEWAY"), "--gateway-endpoint": os.Getenv("NOODLE_TRANSFER_ENDPOINT")} {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == expected && expected != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("transfer did not retain bound %s=%q", flag, expected)
		}
	}
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
		if binding.ArchivePolicy != "" && os.Getenv("NOODLE_PROGRESS_FIXTURE_OLD_SUPERVISOR") != "1" {
			receipt.Schema = 2
			receipt.ArchivePolicySHA256, err = guestprogress.ArchivePolicySHA256(binding.ArchivePolicy)
			if err != nil {
				t.Fatal(err)
			}
			receipt.ExcludedDependencies = []string{"node_modules"}
		}
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
	if os.Getenv("NOODLE_TRANSFER_UNCHANGED") == "1" && archive != "noodle-session.tar" {
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}
	if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Size: int64(len(data)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if archive != "noodle-session.tar" && os.Getenv("NOODLE_PROGRESS_FIXTURE_DEPENDENCIES") == "1" {
		for _, item := range []struct{ name, data string }{{"package.json", `{"dependencies":{"updated":"2"}}`}, {"pnpm-lock.yaml", "legitimately updated lockfile\n"}} {
			if err := tw.WriteHeader(&tar.Header{Name: item.name, Size: int64(len(item.data)), Mode: 0644}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(item.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if archive != "noodle-session.tar" {
		if name := os.Getenv("NOODLE_PROGRESS_FIXTURE_LINK"); name != "" {
			if err := tw.WriteHeader(&tar.Header{Name: name, Linkname: "result.txt", Typeflag: tar.TypeSymlink, Mode: 0777}); err != nil {
				t.Fatal(err)
			}
		}
		if os.Getenv("NOODLE_PROGRESS_FIXTURE_INVALID_NAME") == "1" {
			if err := tw.WriteHeader(&tar.Header{Name: "source/invalid\tname", Mode: 0644}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if archive == "noodle-session.tar" {
		if err := tw.WriteHeader(&tar.Header{Name: "codex/tmp/arg0-linux/codex-arg0fixture/apply_patch", Typeflag: tar.TypeSymlink, Linkname: "/sandbox/payload/codex/bin/codex", Mode: 0777}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
func openShellRuntimeFixture(t *testing.T, prepare ...func(*managedFixture)) (*OpenShellRuntime, *managedFixture, string) {
	t.Helper()
	f := newManagedFixture(t, true)
	for _, configure := range prepare {
		configure(f)
	}
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
	if err = os.WriteFile(cli, openShellTransferFixtureScript(executable), 0700); err != nil {
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
	runtime.config.Profile.Environment = append(runtime.config.Profile.Environment, "NOODLE_TRANSFER_GATEWAY=fixture", "NOODLE_TRANSFER_ENDPOINT="+f.spec.Binding.Gateway)
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
	if _, err := os.Lstat(filepath.Join(s.dir, "delivery", "patch-repository")); !os.IsNotExist(err) {
		t.Fatal("completed publisher retained its diff scratch", err)
	}
	if !r.CapacityAvailable() {
		t.Fatal("settled attempt retained capacity")
	}
	if _, err := f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellLocalTaskChecksFinalCandidateScope(t *testing.T) {
	for _, tc := range []struct {
		name          string
		writePath     string
		priorProgress string
		refused       bool
		roleReadOnly  bool
		link          string
		linkGrant     string
		inheritedLink bool
		linkOutside   bool
		invalidPath   bool
	}{
		{name: "admitted output", writePath: "result.txt"},
		{name: "out of scope output", writePath: "readme.md", refused: true},
		{name: "out of scope unchanged progress", writePath: "readme.md", priorProgress: "result.txt", refused: true},
		{name: "removed out of scope progress", writePath: "result.txt", priorProgress: "dist"},
		{name: "read-only role retains previous candidate", writePath: "result.txt", priorProgress: "result.txt", roleReadOnly: true},
		{name: "read-only role changes admitted task output", writePath: "result.txt", priorProgress: "dist", roleReadOnly: true, refused: true},
		{name: "in-scope relative symlink", writePath: "result.txt", link: "link.txt", refused: true},
		{name: "unchanged admitted relative symlink", writePath: "result.txt", link: "link.txt", inheritedLink: true, refused: true},
		{name: "unchanged ancestor of explicit write path is a symlink", writePath: "result.txt", link: "link.txt", linkGrant: "link.txt/child", inheritedLink: true, refused: true},
		{name: "unrelated relative symlink", writePath: "result.txt", link: "link.txt", inheritedLink: true, linkOutside: true},
		{name: "in-scope invalid path", writePath: "result.txt", invalidPath: true, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, f, target := openShellRuntimeFixture(t)
			managedMarker(t, f.root, "completion-observation")
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = target
				if raw, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, raw)
				}
			}
			if tc.inheritedLink {
				for _, name := range []string{tc.link, "result.txt"} {
					var err error
					if name == tc.link {
						err = os.Symlink("result.txt", filepath.Join(target, name))
					} else {
						err = os.WriteFile(filepath.Join(target, name), []byte("accepted output\n"), 0600)
					}
					if err != nil {
						t.Fatal(err)
					}
					git("add", "--", name)
				}
				git("commit", "-m", "baseline contains existing link")
			}
			git("branch", "integration")
			if tc.priorProgress != "" {
				if err := os.WriteFile(filepath.Join(target, tc.priorProgress), []byte("accepted output\n"), 0600); err != nil {
					t.Fatal(err)
				}
				git("add", "--", tc.priorProgress)
				git("commit", "-m", "retain earlier progress")
			}
			scope := &workspaceartifacts.LocalTaskScope{IntegrationBranch: "integration", WritePaths: []string{tc.writePath}}
			if tc.roleReadOnly {
				cmd := exec.Command("git", "rev-parse", "HEAD")
				cmd.Dir = target
				raw, err := cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
				scope.PriorCandidateHead = strings.TrimSpace(string(raw))
			}
			if tc.link != "" {
				if !tc.linkOutside {
					grant := tc.linkGrant
					if grant == "" {
						grant = tc.link
					}
					scope.WritePaths = append(scope.WritePaths, grant)
				}
				r.config.Profile.Environment = append(r.config.Profile.Environment, "NOODLE_PROGRESS_FIXTURE_LINK="+tc.link)
			}
			if tc.invalidPath {
				scope.WritePaths = append(scope.WritePaths, "source")
				r.config.Profile.Environment = append(r.config.Profile.Environment, "NOODLE_PROGRESS_FIXTURE_INVALID_NAME=1")
			}
			h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "scoped", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true, LocalTaskScope: scope, EnvVars: map[string]string{"NOODLE_ORDER_ID": "scoped", "NOODLE_STAGE_INDEX": "0"}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-h.Done():
			case <-time.After(10 * time.Second):
				t.Fatalf("attempt unresolved: %+v", h.Outcome())
			}
			s := h.(*openShellSession)
			var receipt openShellDispatchReceipt
			dispatchRef, err := ReadSessionReadbackFile(filepath.Join(s.dir, "openshell-dispatch.json"), &receipt)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Phase != "settled" || receipt.Error != "" || receipt.Outcome.ExitCode != 0 || receipt.Delivery == nil {
				t.Fatalf("original guest execution or delivery evidence changed: %+v", receipt)
			}
			if tc.refused {
				if receipt.Outcome.Status != StatusFailed || receipt.CandidateHead != "" || receipt.ProgressHead == "" || !receipt.Delivery.LocalTaskRefused() || !strings.HasPrefix(receipt.Outcome.Reason, LocalTaskCandidateRefusalPrefix) {
					t.Fatalf("out-of-scope result did not settle as stopped progress: %+v, delivery=%+v", receipt, receipt.Delivery)
				}
				if receipt.Worker == nil || receipt.Worker.ModelOutcome.Status != StatusCompleted || receipt.Worker.ModelOutcome.ExitCode != 0 {
					t.Fatalf("host refusal overwrote the provider outcome: %+v", receipt.Worker)
				}
				switch {
				case tc.roleReadOnly:
					if len(receipt.Delivery.RoleScopeViolations) != 2 || len(receipt.Delivery.ScopeViolations) != 0 {
						t.Fatalf("incorrect role scope diagnostics: %+v", receipt.Delivery)
					}
				case tc.link != "":
					if len(receipt.Delivery.UnsupportedPaths) != 1 || receipt.Delivery.UnsupportedPaths[0] != tc.link || len(receipt.Delivery.ScopeViolations) != 0 {
						t.Fatalf("incorrect file type diagnostics: %+v", receipt.Delivery)
					}
				case tc.invalidPath:
					if len(receipt.Delivery.InvalidPaths) != 1 || receipt.Delivery.InvalidPaths[0] != "source/invalid\tname" || len(receipt.Delivery.ScopeViolations) != 0 {
						t.Fatalf("incorrect path diagnostics: %+v", receipt.Delivery)
					}
				default:
					if len(receipt.Delivery.ScopeViolations) != 1 || receipt.Delivery.ScopeViolations[0] != "result.txt" {
						t.Fatalf("incorrect task scope diagnostics: %+v", receipt.Delivery)
					}
				}
				if tc.priorProgress == "result.txt" && receipt.ProgressHead != receipt.Baseline.Head {
					t.Fatal("unchanged refused draft should retain its original commit")
				}
				writeReadbackJSON(t, filepath.Join(s.dir, "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
				binding := CompletedSessionBinding{OrderID: "scoped", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}
				observed, err := r.ReadSavedProgress(t.Context(), binding)
				if err != nil || observed.Status != "saved" || observed.ProgressHead != receipt.ProgressHead || observed.HarnessExitCode != 0 || observed.Sources["openshell-dispatch.json"] != dispatchRef {
					t.Fatalf("original owner could not read refused progress: %+v %v", observed, err)
				}
				if _, err := r.ReadCompletedSession(t.Context(), binding); err == nil {
					t.Fatal("completed candidate reader accepted refused progress")
				}
			} else if receipt.Outcome.Status != StatusCompleted || receipt.CandidateHead == "" || receipt.ProgressHead != "" || receipt.Delivery.LocalTaskRefused() {
				t.Fatalf("in-scope result did not settle as candidate: %+v, delivery=%+v", receipt, receipt.Delivery)
			}
		})
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
	for _, unchanged := range []bool{false, true} {
		t.Run(fmt.Sprintf("unchanged=%t", unchanged), func(t *testing.T) {
			testOpenShellStopProgress(t, unchanged)
		})
	}
}

func testOpenShellStopProgress(t *testing.T, unchanged bool) {
	r, f, target := openShellRuntimeFixture(t)
	if unchanged {
		r.config.Profile.Environment = append(r.config.Profile.Environment, "NOODLE_TRANSFER_UNCHANGED=1")
	}
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
	if unchanged {
		if !os.IsNotExist(err) || receipt.ProgressHead != receipt.Baseline.Head {
			t.Fatal("unchanged progress lost its original HEAD or introduced output")
		}
	} else if err != nil || string(data) != "accepted output\n" {
		t.Fatal("guest progress not restored to original worktree")
	}
	archives, err := progressArchiveSources(t.Context(), session.dir, receipt)
	if err != nil {
		t.Fatal(err)
	}
	sessionData, err := os.ReadFile(archives["progress_archive_noodle-session.tar"].Path)
	if err != nil || !bytes.Equal(sessionData, progressFixtureArchive(t, "noodle-session.tar")) {
		t.Fatal("original native session archive was not retained exactly")
	}
	if _, err := os.Lstat(filepath.Join(session.dir, "guest-session")); !os.IsNotExist(err) {
		t.Fatal("guest-local executable links were materialized on the Host")
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
			case <-session.runExited:
			case <-time.After(5 * time.Second):
				t.Fatal("retained execution did not supply positive run exit")
			}
			path := filepath.Join(session.dir, "openshell-dispatch.json")
			var frozen openShellDispatchReceipt
			if _, err := ReadSessionReadbackFile(path, &frozen); err != nil {
				t.Fatal(err)
			}
			proof := archiveRecoveryVMProof{
				disposition: archiveRecoveryRetirementDisposition{SessionID: session.id},
				request:     ProgressMaintenance{SessionDirectory: session.dir}, dispatch: frozen,
			}
			r.mu.Lock()
			known, verifyErr := r.inertArchiveRecoveryHandle(proof)
			r.mu.Unlock()
			if verifyErr != nil || known != session {
				t.Fatalf("retained exact worker did not qualify as inert: %v", verifyErr)
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
