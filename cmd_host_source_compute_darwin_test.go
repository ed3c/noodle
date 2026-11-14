//go:build darwin && cgo

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func ownedSourceComputeScope(t *testing.T) *hostOperatorNativeScope {
	t.Helper()
	spawn := func() hostOperatorActor {
		t.Helper()
		child := exec.Command("/bin/sleep", "30")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		// Only this test's unreaped child is controlled or cleaned up.
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
		identity, err := procx.ObserveNativeIdentity(child.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		image, err := procx.ObserveNativeImage(identity)
		if err != nil {
			t.Fatal(err)
		}
		binary, err := hostReadbackBinaryRef(image)
		if err != nil {
			t.Fatal(err)
		}
		return hostOperatorActor{Process: identity, Binary: binary}
	}
	return &hostOperatorNativeScope{Schema: 1, Host: spawn(), Gateway: spawn(), Driver: spawn()}
}

func suspendSourceComputeFixture(t *testing.T, actor hostOperatorActor) {
	t.Helper()
	if err := procx.SetNativeSuspended(actor.Process, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		current, err := procx.ObserveNativeIdentity(actor.Process.PID)
		if err == nil && actor.Process.SameGeneration(current) && current.Stopped {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("owned fixture suspension was not observed", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSourceComputeFenceRequiresSelectedNativeSuspensions(t *testing.T) {
	scope := ownedSourceComputeScope(t)
	if fenced, err := sourceComputeFenced(scope); err != nil || fenced {
		t.Fatal("live native actors were reported suspended", fenced, err)
	}
	suspendSourceComputeFixture(t, scope.Host)
	suspendSourceComputeFixture(t, scope.Driver)
	if fenced, err := sourceComputeFenced(scope); err != nil || !fenced {
		t.Fatal("selected native suspension not observed", fenced, err)
	}
	changed := *scope
	changed.Driver.Process.Version++
	if fenced, err := sourceComputeFenced(&changed); err == nil || fenced {
		t.Fatal("different driver generation accepted")
	}
	suspendSourceComputeFixture(t, scope.Gateway)
	if fenced, err := sourceComputeFenced(scope); err == nil || fenced {
		t.Fatal("suspended credential gateway accepted")
	}
}

type sourceComputeStoreFixture struct {
	pb.UnimplementedOpenShellServer
	count  atomic.Int32
	onList func() error
}

func (s *sourceComputeStoreFixture) ListSandboxes(_ context.Context, _ *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	if s.onList != nil {
		if err := s.onList(); err != nil {
			return nil, err
		}
	}
	return &pb.ListSandboxesResponse{Sandboxes: make([]*pb.Sandbox, int(s.count.Load()))}, nil
}

// This composes real owned kernel suspensions with a fixture mTLS store and
// synthetic immutable scope. It is not actual Source qualification or adoption.
func TestSourceComputeComposesPositiveAndLostFenceReadbacks(t *testing.T) {
	for _, fault := range []string{"", "store-added", "fence-lost"} {
		t.Run(fault, func(t *testing.T) {
			p, _ := sourcePolicyTestPlan(t)
			op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			profile := hostDeferralProfile(t)
			root := filepath.Dir(profile)
			pair, err := tls.LoadX509KeyPair(filepath.Join(root, "server.crt"), filepath.Join(root, "server.key"))
			if err != nil {
				t.Fatal(err)
			}
			ca, err := os.ReadFile(filepath.Join(root, "ca.crt"))
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(ca) {
				t.Fatal("fixture CA unavailable")
			}
			address, err := os.ReadFile(filepath.Join(root, "address"))
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", string(address))
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})))
			store := &sourceComputeStoreFixture{}
			pb.RegisterOpenShellServer(server, store)
			go server.Serve(listener)
			t.Cleanup(func() { server.Stop(); _ = listener.Close() })
			gateway, err := loopruntime.LoadOpenShellGatewaySpec(profile)
			if err != nil {
				t.Fatal(err)
			}
			scope := ownedSourceComputeScope(t)
			namespace, _ := sourceComputeLocalFixture(t)
			scope.StateDirectory = namespace.StateDirectory
			scope.GatewayConfig, err = loopruntime.ReadSessionReadbackFile(gateway.Binding.ConfigSource, nil)
			if err != nil {
				t.Fatal(err)
			}
			op.NativeScope, op.ProfileSource, op.Binding = scope, profile, gateway.Binding
			op.Profile, err = loopruntime.ReadSessionReadbackFile(profile, nil)
			if err != nil {
				t.Fatal(err)
			}
			custody := filepath.Join(t.TempDir(), "owner.json")
			if err = os.WriteFile(custody, sourcePolicyTestJSON(t, loopruntime.OpenShellCustodyState{Version: 4, ControlRoot: p.Root, Binding: gateway.Binding, Blocked: "business unknown retained"}), 0600); err != nil {
				t.Fatal(err)
			}
			op.Custody, err = loopruntime.ReadSessionReadbackFile(custody, nil)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := filepath.Join(t.TempDir(), "snapshot.json")
			if err = os.WriteFile(snapshot, sourcePolicyTestJSON(t, op), 0600); err != nil {
				t.Fatal(err)
			}
			p.SourcePolicy.Snapshot, err = loopruntime.ReadSessionReadbackFile(snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			suspendSourceComputeFixture(t, scope.Host)
			suspendSourceComputeFixture(t, scope.Driver)
			if fault == "store-added" {
				store.count.Store(1)
			}
			if fault == "fence-lost" {
				store.onList = func() error { return procx.SetNativeSuspended(scope.Driver.Process, false) }
			}
			observed := readSourceComputeStatus(t.Context(), p)
			if (fault == "" && observed.State != "observed_zero") || (fault != "" && observed.State != "unknown") {
				t.Fatal("combined compute observation crossed its boundary", fault, observed)
			}
		})
	}
}
