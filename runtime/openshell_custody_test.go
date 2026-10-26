package runtime

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/lockfile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type custodyGateway struct {
	mu                                          sync.Mutex
	sandboxes                                   map[string]*pb.Sandbox
	creates, stops, execs, lists                int
	pageSize                                    int
	createErr, stopErr, execErr                 error
	afterCreate, afterStop, afterList, afterGet func()
	stopPhase                                   pb.SandboxPhase
}

func (g *custodyGateway) mutate(fn func()) { g.mu.Lock(); defer g.mu.Unlock(); fn() }
func (g *custodyGateway) counts() (int, int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.creates, g.stops, g.execs
}
func custodySandbox(name, id string, phase pb.SandboxPhase) *pb.Sandbox {
	return &pb.Sandbox{Metadata: &dm.ObjectMeta{Name: name, Id: id, Workspace: "owned-workspace"}, Spec: &pb.SandboxSpec{Command: []string{"hold"}}, Status: &pb.SandboxStatus{Phase: phase}}
}

type custodyOpener func(context.Context, OpenShellCustodyBinding, OpenShellTLS, []OpenShellRetainedSandbox) (*OpenShellCustody, error)
type custodyFactory func(*testing.T) (*custodyGateway, OpenShellCustodyBinding, OpenShellTLS, custodyOpener)

func custodyFixture(t *testing.T) (*custodyGateway, OpenShellCustodyBinding, OpenShellTLS, custodyOpener) {
	g, server := custodyServer(t)
	target, security := openShellTestGateway(t, server)
	binding := custodyBinding(t, target.Sandbox.Gateway)
	return g, binding, security, OpenOpenShellCustody
}
func custodyBinding(t *testing.T, gateway string) OpenShellCustodyBinding {
	return OpenShellCustodyBinding{StateRoot: filepath.Join(t.TempDir(), "custody"), Gateway: gateway, Workspace: "owned-workspace", Owner: "owner-1", ConfigSource: "test-selection", ConfigFingerprint: "test-config-v1"}
}

// Additional real gRPC/mTLS coverage without an OS socket. TCP tests above
// remain mandatory; this transport does not establish TCP/carrier acceptance.
func custodyInProcessFixture(t *testing.T) (*custodyGateway, OpenShellCustodyBinding, OpenShellTLS, custodyOpener) {
	t.Helper()
	g, server := custodyServer(t)
	security, serverTLS := openShellTestTLS(t)
	listener := bufconn.Listen(1024 * 1024)
	rpc := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	pb.RegisterOpenShellServer(rpc, server)
	returned := make(chan struct{})
	go func() { defer close(returned); _ = rpc.Serve(listener) }()
	t.Cleanup(func() { rpc.Stop(); _ = listener.Close(); <-returned })
	open := func(ctx context.Context, b OpenShellCustodyBinding, sec OpenShellTLS, refs []OpenShellRetainedSandbox) (*OpenShellCustody, error) {
		config := serverTLS.Clone()
		config.Certificates = []tls.Certificate{sec.Certificate}
		config.RootCAs = sec.RootCAs
		config.ServerName = sec.ServerName
		conn, err := grpc.NewClient("passthrough:///gateway.test", grpc.WithTransportCredentials(credentials.NewTLS(config)), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithDisableRetry(), grpc.WithDisableServiceConfig(), grpc.WithDefaultCallOptions(grpc.MaxRetryRPCBufferSize(0)))
		if err != nil {
			return nil, err
		}
		return openOpenShellCustody(ctx, b, conn, refs)
	}
	return g, custodyBinding(t, "https://gateway.test"), security, open
}
func custodyServer(t *testing.T) (*custodyGateway, *openShellNativeServer) {
	t.Helper()
	g := &custodyGateway{sandboxes: map[string]*pb.Sandbox{}, pageSize: 1, stopPhase: pb.SandboxPhase_SANDBOX_PHASE_STOPPED}
	server := &openShellNativeServer{}
	server.get = func(_ context.Context, r *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.WorkspaceScope.GetWorkspace() != "owned-workspace" {
			return nil, status.Error(codes.InvalidArgument, "workspace changed")
		}
		s := g.sandboxes[r.Name]
		if s == nil {
			return nil, status.Error(codes.NotFound, "sandbox missing")
		}
		snapshot := proto.Clone(s).(*pb.Sandbox)
		if g.afterGet != nil {
			g.afterGet()
		}
		return &pb.SandboxResponse{Sandbox: snapshot}, nil
	}
	server.list = func(_ context.Context, r *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.lists++
		if r.WorkspaceScope.GetAllWorkspaces() == nil || r.LabelSelector != "" {
			return nil, status.Error(codes.InvalidArgument, "inventory is filtered")
		}
		keys := make([]string, 0, len(g.sandboxes))
		for k := range g.sandboxes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		start := 0
		if r.PageToken != "" {
			var err error
			start, err = strconv.Atoi(r.PageToken)
			if err != nil {
				return nil, err
			}
		}
		response := &pb.ListSandboxesResponse{}
		end := min(start+g.pageSize, len(keys))
		for _, k := range keys[start:end] {
			response.Sandboxes = append(response.Sandboxes, proto.Clone(g.sandboxes[k]).(*pb.Sandbox))
		}
		if end < len(keys) {
			response.NextPageToken = strconv.Itoa(end)
		}
		if g.afterList != nil {
			g.afterList()
		}
		return response, nil
	}
	server.create = func(_ context.Context, r *pb.CreateSandboxRequest) (*pb.SandboxResponse, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.creates++
		if err := (OpenShellCommand{RequestID: r.RequestId, Argv: []string{"check"}, Workdir: "/", Env: map[string]string{}}).validate(); err != nil {
			return nil, err
		}
		if r.WorkspaceScope.GetWorkspace() != "owned-workspace" || !regexp.MustCompile(`^[a-z][a-z0-9]{18}$`).MatchString(r.Name) {
			return nil, status.Error(codes.InvalidArgument, "create locator invalid")
		}
		s := custodySandbox(r.Name, fmt.Sprintf("native-%d", g.creates), pb.SandboxPhase_SANDBOX_PHASE_READY)
		s.Spec = proto.Clone(r.Spec).(*pb.SandboxSpec)
		g.sandboxes[r.Name] = s
		if g.afterCreate != nil {
			g.afterCreate()
		}
		return &pb.SandboxResponse{Sandbox: proto.Clone(s).(*pb.Sandbox)}, g.createErr
	}
	server.stop = func(_ context.Context, r *pb.StopSandboxRequest) (*pb.SandboxResponse, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.stops++
		if r.WorkspaceScope.GetWorkspace() != "owned-workspace" || r.RequestId == "" {
			return nil, status.Error(codes.InvalidArgument, "stop receipt missing")
		}
		s := g.sandboxes[r.Name]
		if s == nil {
			return nil, status.Error(codes.NotFound, "sandbox missing")
		}
		s.Status.Phase = g.stopPhase
		if g.afterStop != nil {
			g.afterStop()
		}
		return &pb.SandboxResponse{Sandbox: proto.Clone(s).(*pb.Sandbox)}, g.stopErr
	}
	server.exec = func(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
		start, err := openShellReceiveStart(stream)
		if err != nil {
			return err
		}
		g.mu.Lock()
		g.execs++
		execErr := g.execErr
		_, found := g.sandboxes[start.Sandbox]
		g.mu.Unlock()
		if !found || start.Tty || !start.NoLoginShell || start.RequestId != openShellRequestID {
			return status.Error(codes.InvalidArgument, "owned exec request changed")
		}
		if execErr != nil {
			return execErr
		}
		return stream.Send(openShellExit(7))
	}
	return g, server
}

func custodyOpen(t *testing.T, b OpenShellCustodyBinding, security OpenShellTLS, retained []OpenShellRetainedSandbox, open custodyOpener) *OpenShellCustody {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := open(ctx, b, security, retained)
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func custodyCreate(t *testing.T, c *OpenShellCustody, n string) OpenShellCustodyRecord {
	t.Helper()
	name, err := c.Create(t.Context(), OpenShellTask{OrderID: "order-" + n, AttemptID: "attempt-" + n, SessionID: "session-" + n}, &pb.SandboxSpec{Command: []string{"hold"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Snapshot().Records {
		if r.Name == name {
			return r
		}
	}
	t.Fatal("created record missing")
	return OpenShellCustodyRecord{}
}
func custodyDisk(t *testing.T, b OpenShellCustodyBinding) OpenShellCustodyState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state OpenShellCustodyState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func testOpenShellCustodyNativeLifecycle(t *testing.T, fixture custodyFactory) {
	g, b, security, open := fixture(t)
	c := custodyOpen(t, b, security, nil, open)
	// The gRPC handler sees a durable intent BEFORE the effect.
	g.mutate(func() {
		g.afterCreate = func() {
			state := custodyDisk(t, b)
			r := state.Records[0]
			var request pb.CreateSandboxRequest
			if err := proto.Unmarshal(r.CreateIntent, &request); err != nil {
				t.Error(err)
			}
			if r.CreateAcknowledged || r.Task.OrderID != "order-one" || request.Name != r.Name || request.RequestId != r.CreateRequestID {
				t.Error("create intent was not persisted before RPC")
			}
		}
	})
	r := custodyCreate(t, c, "one")
	detached := c.Snapshot()
	detached.Records[0].Spec[0] ^= 1
	if reflect.DeepEqual(detached, c.Snapshot()) {
		t.Fatal("Snapshot aliases state")
	}
	handle, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec-one", openShellTestCommand())
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Output() {
	}
	result, err := handle.Wait()
	if err != nil || !result.ExitObserved || result.ExitCode != 7 {
		t.Fatalf("native exit lost: %+v %v", result, err)
	}
	w, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryReady)
	g.mutate(func() {
		g.afterStop = func() {
			pending := custodyDisk(t, b).Records[0]
			if pending.StopRequestID == "" || !pending.Worker.StopPending || pending.StopAcknowledged {
				t.Error("stop intent not durable before RPC")
			}
		}
	})
	if err := w.Stop(t.Context(), c, c); err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
	if got := custodyDisk(t, b).Records[0]; !got.StopAcknowledged || got.Worker.StopPending || got.Exec.Command.RequestID != openShellRequestID {
		t.Fatal("durable evidence lost")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = custodyOpen(t, b, security, nil, open)
	w, err = c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
	if a, s, e := g.counts(); a != 1 || s != 1 || e != 1 {
		t.Fatalf("unexpected replay: %d %d %d", a, s, e)
	}
}

func testOpenShellCustodyStopACKAndFreshStateIndependent(t *testing.T, fixture custodyFactory) {
	for _, lost := range []bool{false, true} {
		t.Run(strconv.FormatBool(lost), func(t *testing.T) {
			g, b, security, open := fixture(t)
			c := custodyOpen(t, b, security, nil, open)
			r := custodyCreate(t, c, "one")
			g.mutate(func() {
				if lost {
					g.stopErr = status.Error(codes.Unavailable, "ACK lost")
				} else {
					g.stopPhase = pb.SandboxPhase_SANDBOX_PHASE_READY
				}
			})
			w, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
			if err != nil {
				t.Fatal(err)
			}
			err = w.Stop(t.Context(), c, c)
			if (err != nil) != lost {
				t.Fatalf("stop error=%v", err)
			}
			if !lost {
				assertOpenShellBoundary(t, w, OpenShellBoundaryReady)
			}
			g.mutate(func() { g.sandboxes[r.Name].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED })
			_ = c.Close()
			c = custodyOpen(t, b, security, nil, open)
			w, err = c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
			if err != nil {
				t.Fatal(err)
			}
			if lost {
				assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
				if !w.Record().StopPending || c.Snapshot().Records[0].StopAcknowledged {
					t.Fatal("lost ACK inferred from Stopped")
				}
				if err := w.Stop(t.Context(), c, c); err == nil {
					t.Fatal("unknown stop replayed")
				}
			} else {
				assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
			}
			if _, stops, _ := g.counts(); stops != 1 {
				t.Fatalf("stop replay count=%d", stops)
			}
		})
	}
}

func testOpenShellCustodyUnknownCreateAndExecNeverReplay(t *testing.T, fixture custodyFactory) {
	t.Run("create", func(t *testing.T) {
		g, b, security, open := fixture(t)
		c := custodyOpen(t, b, security, nil, open)
		g.mutate(func() { g.createErr = status.Error(codes.Unavailable, "ACK lost") })
		task := OpenShellTask{"order", "attempt", "session"}
		name, err := c.Create(t.Context(), task, &pb.SandboxSpec{Command: []string{"hold"}})
		if err == nil || name == "" {
			t.Fatal("lost create treated as rollback")
		}
		old := custodyDisk(t, b)
		if old.Blocked == "" || old.Records[0].CreateAcknowledged || old.Records[0].CreateRequestID == "" {
			t.Fatal("unknown create lost")
		}
		g.mutate(func() { delete(g.sandboxes, name) }) // Removing drift cannot unblock.
		_ = c.Close()
		c, err = open(t.Context(), b, security, nil)
		if c == nil || err == nil {
			t.Fatal("reopen cleared block")
		}
		defer c.Close()
		if _, err := c.Create(t.Context(), task, &pb.SandboxSpec{}); err == nil {
			t.Fatal("unknown create replayed")
		}
		if got := c.Snapshot().Records[0]; got.Name != name || got.CreateRequestID != old.Records[0].CreateRequestID {
			t.Fatal("original reservation lost")
		}
		if creates, _, _ := g.counts(); creates != 1 {
			t.Fatal("replacement VM created")
		}
	})
	t.Run("exec", func(t *testing.T) {
		g, b, security, open := fixture(t)
		c := custodyOpen(t, b, security, nil, open)
		r := custodyCreate(t, c, "one")
		g.mutate(func() { g.execErr = status.Error(codes.Unavailable, "exec ACK lost") })
		handle, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec-attempt", openShellTestCommand())
		if handle == nil {
			t.Fatalf("submission handle lost: %v", err)
		}
		for range handle.Output() {
		}
		result, waitErr := handle.Wait()
		if waitErr == nil || result.ExitObserved {
			t.Fatal("lost exec inferred as complete")
		}
		if c.Snapshot().Records[0].Exec == nil {
			t.Fatal("exec reservation lost")
		}
		_ = c.Close()
		c = custodyOpen(t, b, security, nil, open)
		command := openShellTestCommand()
		command.Argv = []string{"different"}
		if _, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec-attempt", command); err == nil {
			t.Fatal("different command replayed")
		}
		if _, _, execs := g.counts(); execs != 1 {
			t.Fatal("unknown exec replayed")
		}
	})
}

func testOpenShellCustodyInventoryDriftIsSticky(t *testing.T, fixture custodyFactory) {
	for _, kind := range []string{"foreign page", "foreign workspace", "replacement", "spec", "missing", "configuration"} {
		t.Run(kind, func(t *testing.T) {
			g, b, security, open := fixture(t)
			c := custodyOpen(t, b, security, nil, open)
			r := custodyCreate(t, c, "one")
			_ = custodyCreate(t, c, "two")
			g.mutate(func() {
				switch kind {
				case "foreign page":
					g.sandboxes["zz-last"] = custodySandbox("zz-last", "external", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
				case "foreign workspace":
					s := custodySandbox("zz-other", "external", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
					s.Metadata.Workspace = "other"
					g.sandboxes["zz-other"] = s
				case "replacement":
					g.sandboxes[r.Name].Metadata.Id = "replacement"
				case "spec":
					g.sandboxes[r.Name].Spec.Command = []string{"changed"}
				case "missing":
					delete(g.sandboxes, r.Name)
				}
			})
			if kind == "configuration" {
				_ = c.Close()
				changed := b
				changed.ConfigFingerprint = "changed"
				var err error
				c, err = open(t.Context(), changed, security, nil)
				if c == nil || err == nil {
					t.Fatal("configuration drift accepted")
				}
				defer c.Close()
			} else {
				if _, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec", openShellTestCommand()); err == nil {
					t.Fatal("drift accepted")
				}
			}
			if custodyDisk(t, b).Blocked == "" {
				t.Fatal("block not durable")
			}
			if _, err := c.Create(t.Context(), OpenShellTask{"new", "new", "new"}, &pb.SandboxSpec{}); err == nil {
				t.Fatal("blocked create dispatched")
			}
			_ = c.Close()
			c, err := open(t.Context(), b, security, nil)
			if c == nil || err == nil {
				t.Fatal("reopen unblocked drift")
			}
			defer c.Close()
			if _, stops, execs := g.counts(); stops != 0 || execs != 0 {
				t.Fatal("drift produced mutation")
			}
		})
	}
}

func testOpenShellCustodyRetainedIsExplicitReadOnlyHistory(t *testing.T, fixture custodyFactory) {
	for _, mode := range []string{"explicit", "empty", "running", "wrong ID"} {
		t.Run(mode, func(t *testing.T) {
			g, b, security, open := fixture(t)
			g.mutate(func() {
				g.sandboxes["probe"] = custodySandbox("probe", "probe-id", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
				if mode == "running" {
					g.sandboxes["probe"].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY
				}
			})
			retained := []OpenShellRetainedSandbox{{"probe", "probe-id", b.Workspace}}
			if mode == "empty" {
				retained = nil
			}
			if mode == "wrong ID" {
				retained[0].SandboxID = "wrong"
			}
			c, err := open(t.Context(), b, security, retained)
			if c != nil {
				defer c.Close()
			}
			if mode != "explicit" {
				if err == nil {
					t.Fatal("implicit/invalid adoption")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := c.Snapshot().Records[0]
			if !r.Retained || r.Name != "probe" {
				t.Fatal("history missing")
			}
			if _, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec", openShellTestCommand()); err == nil {
				t.Fatal("retained exec allowed")
			}
			if err := c.StopSandbox(t.Context(), r.Worker.Identity.Sandbox); err == nil {
				t.Fatal("retained stop allowed")
			}
			next := custodyCreate(t, c, "new")
			if next.Name == "probe" {
				t.Fatal("retained name reused")
			}
			_ = c.Close()
			c = custodyOpen(t, b, security, nil, open)
			if !c.Snapshot().Records[0].Retained {
				t.Fatal("reopen lost history")
			}
			g.mutate(func() { g.sandboxes["probe"].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY })
			if _, err := c.Create(t.Context(), OpenShellTask{"third", "third", "third"}, &pb.SandboxSpec{}); err == nil {
				t.Fatal("retained drift ignored")
			}
		})
	}
}

func testOpenShellCustodyDurableWriteFailure(t *testing.T, fixture custodyFactory) {
	for _, where := range []string{"before create", "after create", "before exec", "before stop", "after stop"} {
		t.Run(where, func(t *testing.T) {
			g, b, security, open := fixture(t)
			c := custodyOpen(t, b, security, nil, open)
			var r OpenShellCustodyRecord
			if where != "before create" && where != "after create" {
				r = custodyCreate(t, c, "one")
			}
			path := filepath.Join(b.StateRoot, "owner.json")
			backup := filepath.Join(b.StateRoot, "pending.json")
			breakSave := func() {
				if err := os.Rename(path, backup); err != nil {
					t.Error(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Error(err)
				}
			}
			if strings.HasPrefix(where, "before") {
				breakSave()
			} else {
				g.mutate(func() {
					if where == "after create" {
						g.afterCreate = breakSave
					} else {
						g.afterStop = breakSave
					}
				})
			}
			var err error
			switch where {
			case "before create", "after create":
				_, err = c.Create(t.Context(), OpenShellTask{"new", "new", "new"}, &pb.SandboxSpec{Command: []string{"hold"}})
			case "before exec":
				_, err = c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec", openShellTestCommand())
			default:
				err = c.StopSandbox(t.Context(), r.Worker.Identity.Sandbox)
			}
			if err == nil {
				t.Fatal("save failure hidden")
			}
			creates, stops, execs := g.counts()
			if where == "before create" && creates != 0 || where == "before exec" && execs != 0 || where == "before stop" && stops != 0 {
				t.Fatal("effect preceded durable intent")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, path); err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
			c, err = open(t.Context(), b, security, nil)
			if c != nil {
				defer c.Close()
			}
			if where == "after create" {
				if err == nil || c.Snapshot().Records[0].CreateAcknowledged {
					t.Fatal("create save failure lost unknown")
				}
			}
			if where == "after stop" {
				if err != nil {
					t.Fatal(err)
				}
				w, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
				if err != nil {
					t.Fatal(err)
				}
				if !w.Record().StopPending {
					t.Fatal("stop save failure released custody")
				}
				assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
			}
		})
	}
}

func testOpenShellCustodyProcessLock(t *testing.T, fixture custodyFactory) {
	if root := os.Getenv("NOODLE_CUSTODY_LOCK_TEST_ROOT"); root != "" {
		lock, err := lockfile.TryLock(filepath.Join(root, "owner.lock"))
		var held *lockfile.AlreadyLockedError
		if !errors.As(err, &held) {
			if lock != nil {
				_ = lock.Close()
			}
			t.Fatalf("cross-process lock not held: %v", err)
		}
		return
	}
	_, b, security, open := fixture(t)
	c := custodyOpen(t, b, security, nil, open)
	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenShellCustodyProcessLock$")
	cmd.Env = append(os.Environ(), "NOODLE_CUSTODY_LOCK_TEST_ROOT="+b.StateRoot)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process lock: %v\n%s", err, out)
	}
	alias := b
	alias.StateRoot = filepath.Join(filepath.Dir(b.StateRoot), "alias")
	if err := os.Symlink(b.StateRoot, alias.StateRoot); err != nil {
		t.Fatal(err)
	}
	duplicate, err := open(t.Context(), alias, security, nil)
	if duplicate != nil {
		_ = duplicate.Close()
	}
	if err == nil {
		t.Fatal("canonical root lock bypassed")
	}
	_ = c.Close()
	c = custodyOpen(t, alias, security, nil, open)
	canonical, err := filepath.EvalSymlinks(b.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().Binding.StateRoot != canonical {
		t.Fatal("root not canonical")
	}
}

func TestOpenShellCustodyNativeLifecycle(t *testing.T) {
	testOpenShellCustodyNativeLifecycle(t, custodyFixture)
}

func TestOpenShellCustodyStopACKAndFreshStateIndependent(t *testing.T) {
	testOpenShellCustodyStopACKAndFreshStateIndependent(t, custodyFixture)
}

func TestOpenShellCustodyUnknownCreateAndExecNeverReplay(t *testing.T) {
	testOpenShellCustodyUnknownCreateAndExecNeverReplay(t, custodyFixture)
}

func TestOpenShellCustodyInventoryDriftIsSticky(t *testing.T) {
	testOpenShellCustodyInventoryDriftIsSticky(t, custodyFixture)
}

func TestOpenShellCustodyRetainedIsExplicitReadOnlyHistory(t *testing.T) {
	testOpenShellCustodyRetainedIsExplicitReadOnlyHistory(t, custodyFixture)
}

func TestOpenShellCustodyDurableWriteFailure(t *testing.T) {
	testOpenShellCustodyDurableWriteFailure(t, custodyFixture)
}

func TestOpenShellCustodyProcessLock(t *testing.T) {
	testOpenShellCustodyProcessLock(t, custodyFixture)
}

func testOpenShellCustodyFreshIdentityAndStopACK(t *testing.T, fixture custodyFactory) {
	for _, kind := range []string{"Get ID", "Get name", "Get workspace", "Get spec", "Stop ACK"} {
		t.Run(kind, func(t *testing.T) {
			g, b, security, open := fixture(t)
			c := custodyOpen(t, b, security, nil, open)
			r := custodyCreate(t, c, "one")
			if kind == "Stop ACK" {
				g.mutate(func() { g.afterStop = func() { g.sandboxes[r.Name].Metadata.Id = "replacement" } })
				w, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
				if err != nil {
					t.Fatal(err)
				}
				if err := w.Stop(t.Context(), c, c); err == nil {
					t.Fatal("foreign stop ACK accepted")
				}
				if !w.Record().StopPending || c.Snapshot().Records[0].StopAcknowledged {
					t.Fatal("foreign ACK cleared pending")
				}
			} else {
				// The List response describes the old object. Get must independently reject
				// the now visible change; this is not an atomic expected-ID precondition.
				g.mutate(func() {
					g.afterList = func() {
						s := g.sandboxes[r.Name]
						switch kind {
						case "Get ID":
							s.Metadata.Id = "replacement"
						case "Get name":
							s.Metadata.Name = "replacement"
						case "Get workspace":
							s.Metadata.Workspace = "other"
						case "Get spec":
							s.Spec.Command = []string{"changed"}
						}
					}
				})
				if _, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec", openShellTestCommand()); err == nil {
					t.Fatal("fresh Get drift accepted")
				}
			}
			if c.Snapshot().Blocked == "" || custodyDisk(t, b).Blocked == "" {
				t.Fatal("identity drift block not durable")
			}
			if _, _, execs := g.counts(); execs != 0 {
				t.Fatal("identity drift dispatched exec")
			}
		})
	}
}
func TestOpenShellCustodyFreshIdentityAndStopACK(t *testing.T) {
	testOpenShellCustodyFreshIdentityAndStopACK(t, custodyFixture)
}

func TestOpenShellCustodyInProcessMTLS(t *testing.T) {
	for _, scenario := range []struct {
		name string
		run  func(*testing.T, custodyFactory)
	}{
		{"lifecycle", testOpenShellCustodyNativeLifecycle},
		{"stop ACK", testOpenShellCustodyStopACKAndFreshStateIndependent},
		{"unknown create exec", testOpenShellCustodyUnknownCreateAndExecNeverReplay},
		{"inventory drift", testOpenShellCustodyInventoryDriftIsSticky},
		{"retained", testOpenShellCustodyRetainedIsExplicitReadOnlyHistory},
		{"save failure", testOpenShellCustodyDurableWriteFailure},
		{"process lock", testOpenShellCustodyProcessLock},
		{"fresh identity", testOpenShellCustodyFreshIdentityAndStopACK},
		{"stopped history", testOpenShellCustodyStoppedHistory},
		{"explicit new attempt", testOpenShellCustodyExplicitNewAttempt},
		{"unsettled history", testOpenShellCustodyNewAttemptRequiresSettledHistory},
		{"initial provisioning", testOpenShellCustodyInitialProvisioningIsNotRestart},
		{"close cancellation", testOpenShellCustodyCloseCancelsBlockedCreate},
		{"stopped persistence", testOpenShellCustodyStoppedReadMustPersist},
		{"old state format", testOpenShellCustodyRejectsStateWithoutStoppedHistory},
		{"active instance replacement", testOpenShellCustodyActiveInstanceReplacement},
		{"first Ready instance", testOpenShellCustodyFirstReadyInstance},
		{"driver stop cleanup", testOpenShellCustodyDriverStopCleanup},
		{"instance persistence failure", testOpenShellCustodyInstancePersistenceFailure},
	} {
		t.Run(scenario.name, func(t *testing.T) { scenario.run(t, custodyInProcessFixture) })
	}
}
