//go:build darwin && cgo

package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func sourceNativeAwait(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("native fixture did not produce", path)
		}
	}
}

// This helper owns its real child handles. The gateway/store and old Host RPC
// are fixtures; kernel controls and the replacement Noodle binary are real.
func TestSourceServiceNativeChainChild(t *testing.T) {
	role, root, profile := os.Getenv("NOODLE_SOURCE_CHAIN_ROLE"), os.Getenv("NOODLE_SOURCE_CHAIN_ROOT"), os.Getenv("NOODLE_SOURCE_CHAIN_PROFILE")
	if role == "" {
		return
	}
	identity, err := procx.ObserveNativeIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, role+".json"), sourcePolicyTestJSON(t, identity), 0600); err != nil {
		t.Fatal(err)
	}
	if role == "driver" {
		time.Sleep(60 * time.Second)
		return
	}
	childRole := "gateway"
	if role == "gateway" {
		childRole = "driver"
	}
	child := exec.Command(os.Args[0], "-test.run=^TestSourceServiceNativeChainChild$")
	child.Env = append(os.Environ(), "NOODLE_SOURCE_CHAIN_ROLE="+childRole)
	if role == "host" {
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if role == "host" {
			_ = child.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- child.Wait() }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = child.Process.Kill()
				<-done
			}
		} else {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	sourceNativeAwait(t, filepath.Join(root, childRole+".json"))
	if role == "host" {
		data, err := os.ReadFile(filepath.Join(root, "generation"))
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "g", string(data))
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", filepath.Join(dir, "host.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				func() {
					defer conn.Close()
					var req hostRequest
					if json.NewDecoder(conn).Decode(&req) != nil {
						return
					}
					raw, err := os.ReadFile(filepath.Join(root, "rpc.json"))
					if err == nil {
						_, _ = conn.Write(raw)
					}
				}()
			}
		}()
		if err = os.WriteFile(filepath.Join(root, "host-ready"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(root, "finish")); err == nil {
				return
			}
		}
		return
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(profile, "server.crt"), filepath.Join(profile, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(filepath.Join(profile, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("fixture CA missing")
	}
	address, err := os.ReadFile(filepath.Join(profile, "address"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", string(address))
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert})))
	pb.RegisterOpenShellServer(server, hostDeferralGateway{})
	go server.Serve(listener)
	defer server.Stop()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	defer signal.Stop(stop)
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		select {
		case <-stop:
			goto drain
		default:
		}
		if _, err := os.Stat(filepath.Join(root, "finish-gateway")); err == nil {
			break
		}
	}
drain:
	server.Stop()
	_ = child.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		_ = child.Process.Kill()
		<-done
	}
}

func sourceNativeFullPlan(t *testing.T, target string) (hostMaintenancePlan, []byte, *exec.Cmd, <-chan error) {
	t.Helper()
	p, _ := sourcePolicyTestPlan(t)
	root, err := filepath.EvalSymlinks(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	profile := hostDeferralProfile(t)
	profileRoot := filepath.Dir(profile)
	var profileDoc map[string]json.RawMessage
	raw, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &profileDoc); err != nil {
		t.Fatal(err)
	}
	var gateway map[string]json.RawMessage
	if err = json.Unmarshal(profileDoc["gateway"], &gateway); err != nil {
		t.Fatal(err)
	}
	var binding map[string]string
	if err = json.Unmarshal(gateway["binding"], &binding); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(binding["state_root"], 0700); err != nil {
		t.Fatal(err)
	}
	binding["state_root"], err = filepath.EvalSymlinks(binding["state_root"])
	if err != nil {
		t.Fatal(err)
	}
	gateway["binding"] = sourcePolicyTestJSON(t, binding)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gateway["resident"] = sourcePolicyTestJSON(t, admission.ResidentSpec{Name: "host-openshell", Argv: []string{exe, "-test.run=^TestSourceServiceNativeChainChild$", "--"}, Directory: profileRoot, Environment: []string{"NOODLE_SOURCE_CHAIN_ROLE=gateway", "NOODLE_SOURCE_CHAIN_ROOT=" + root, "NOODLE_SOURCE_CHAIN_PROFILE=" + profileRoot}, Sharing: "shared", Transport: "network"})
	profileDoc["gateway"] = sourcePolicyTestJSON(t, gateway)
	if err = os.WriteFile(profile, sourcePolicyTestJSON(t, profileDoc), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "generation"), []byte(p.Source.Instance), 0600); err != nil {
		t.Fatal(err)
	}
	old := exec.Command(exe, "-test.run=^TestSourceServiceNativeChainChild$")
	old.Env = append(os.Environ(), "NOODLE_SOURCE_CHAIN_ROLE=host", "NOODLE_SOURCE_CHAIN_ROOT="+root, "NOODLE_SOURCE_CHAIN_PROFILE="+profileRoot)
	old.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	log, err := os.Create(filepath.Join(root, "native-fixture.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	old.Stdout, old.Stderr = log, log
	if err = old.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- old.Wait(); close(done) }()
	t.Cleanup(func() {
		if pool, err := readHostPool(root); err == nil {
			for _, g := range pool.Generations {
				if g.ID == p.Source.Instance || g.SourceService == nil || g.Binary != target || g.Process.PID <= 1 {
					continue
				}
				_ = os.WriteFile(filepath.Join(root, "finish-gateway"), nil, 0600)
				for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
					var gateway procx.NativeIdentity
					if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(root, "gateway.json"), &gateway); err == nil {
						if gone, err := procx.ObserveNativeExit(gateway); err == nil && gone {
							break
						}
					}
				}
				if current, err := procx.ObserveNativeIdentity(g.Process.PID); err == nil && !current.Zombie {
					_ = procx.SignalNativeIdentity(current, true)
				}
			}
		}
		if current, err := procx.ObserveNativeIdentity(old.Process.Pid); err == nil && !current.Zombie {
			if current.Stopped {
				_ = procx.SetNativeSuspended(current, false)
			}
			_ = os.WriteFile(filepath.Join(root, "finish"), nil, 0600)
		}
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			_ = old.Process.Kill()
			<-done
		}
	})
	sourceNativeAwait(t, filepath.Join(root, "host-ready"))
	sourceNativeAwait(t, filepath.Join(root, "driver.json"))
	scope := &hostOperatorNativeScope{Schema: 1}
	for role, actor := range map[string]*hostOperatorActor{"host": &scope.Host, "gateway": &scope.Gateway, "driver": &scope.Driver} {
		if _, err = loopruntime.ReadSessionReadbackFile(filepath.Join(root, role+".json"), &actor.Process); err != nil {
			t.Fatal(err)
		}
		actor.Binary, err = hostReadbackBinaryRef(exe)
		if err != nil {
			t.Fatal(err)
		}
	}
	namespace, _ := sourceComputeLocalFixture(t)
	scope.StateDirectory = namespace.StateDirectory
	spec, err := loopruntime.LoadOpenShellGatewaySpec(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(spec.Binding.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	scope.GatewayConfig, err = loopruntime.ReadSessionReadbackFile(spec.Binding.ConfigSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	op.NativeScope, op.ProfileSource, op.Binding = scope, profile, spec.Binding
	op.Profile, err = loopruntime.ReadSessionReadbackFile(profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := loopruntime.PrepareSourceOperatorPolicy(op.Profile, profile, filepath.Join(profileRoot, "operator.json"))
	if err != nil {
		t.Fatal(err)
	}
	op.OperatorPolicy = &policy
	pool, err := readHostPool(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	for i := range op.Contexts {
		r := &op.Contexts[i]
		r.HostPID = scope.Host.Process.PID
		r.BinaryPath = exe
		r.BinarySHA256 = scope.Host.Binary.SHA256
		owner := pool.Owners[r.RequestID]
		owner.Original = *r
		owner.Current = *r
		owner.Current.ContextState = "active"
		pool.Owners[r.RequestID] = owner
	}
	g := pool.Generations[p.Source.Instance]
	g.Directory = filepath.Join(root, "g", p.Source.Instance)
	g.Binary, g.SHA = exe, scope.Host.Binary.SHA256
	g.Process = procx.ProcessRecord{PID: scope.Host.Process.PID, SessionID: g.ID, StartedAt: time.Now()}
	pool.Generations[g.ID] = g
	if err = os.WriteFile(filepath.Join(root, "pool.json"), sourcePolicyTestJSON(t, pool), 0600); err != nil {
		t.Fatal(err)
	}
	op.Pool, err = loopruntime.ReadSessionReadbackFile(filepath.Join(root, "pool.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(op.History.Path, sourcePolicyTestJSON(t, op.Contexts), 0600); err != nil {
		t.Fatal(err)
	}
	op.History, err = loopruntime.ReadSessionReadbackFile(op.History.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	custody := sourcePolicyTestJSON(t, loopruntime.OpenShellCustodyState{Version: 4, ControlRoot: root, Binding: spec.Binding, Blocked: "original business unknown"})
	path := filepath.Join(spec.Binding.StateRoot, "owner.json")
	if err = os.WriteFile(path, custody, 0600); err != nil {
		t.Fatal(err)
	}
	op.Custody, err = loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	op.Resident = admission.Resident{ID: spec.Resident.Name, Name: spec.Resident.Name, PID: scope.Gateway.Process.PID, ProcessGroup: scope.Gateway.Process.Group, Contexts: []string{op.Contexts[0].ContextID}}
	op.Source = sourcePolicyTestJSON(t, map[string]any{"protocol": hostProtocol, "host_instance_id": p.Source.Instance, "host_pid": scope.Host.Process.PID, "contexts": len(op.Contexts), "rolling_host_version": 1, "host_root": root})
	if err = os.WriteFile(filepath.Join(root, "rpc.json"), op.Source, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "g", p.Source.Instance, "host.sock"), filepath.Join(root, "host.sock")); err != nil {
		t.Fatal(err)
	}
	if err = saveHostBootstrap(root, hostBootstrap{Schema: 1, Phase: "active", Source: g, Target: exe, TargetSHA: scope.Host.Binary.SHA256, ProcessIdentity: "owned-fixture"}); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "quota"), 0700); err != nil {
		t.Fatal(err)
	}
	quota := sourcePolicyTestJSON(t, map[string]any{"schema": 2, "maximum": 4, "swap_used_limit_bytes": uint64(15_000_000_000), "leases": map[string]any{"original-resident-lease": map[string]any{"owner": p.Source.Instance, "context": op.Contexts[0].ContextID, "resident": true, "resident_key": op.Resident.ID}}})
	if err = os.WriteFile(filepath.Join(root, "quota", "quota.json"), quota, 0600); err != nil {
		t.Fatal(err)
	}
	op.Quota, err = observeSourceQuota(op)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(root, "full-snapshot.json")
	if err = os.WriteFile(snapshot, sourcePolicyTestJSON(t, op), 0600); err != nil {
		t.Fatal(err)
	}
	p, err = prepareSourceServiceRestore(t.Context(), snapshot, target)
	if err != nil {
		t.Fatal(err)
	}
	return p, custody, old, done
}

func TestSourceServiceNativeMaintenanceAndReplacement(t *testing.T) {
	target := os.Getenv("NOODLE_SOURCE_RESTORE_FIXTURE_BINARY")
	if target == "" {
		t.Skip("immutable replacement Noodle executable not selected")
	}
	if !procx.NativeFixtureControlAvailable() {
		t.Fatal("native generation control unavailable")
	}
	p, custody, _, oldDone := sourceNativeFullPlan(t, target)
	dir := filepath.Join(p.Root, "maintenance", p.PlanSHA)
	var j restrictedMaintenance
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(30 * time.Millisecond) {
		var output bytes.Buffer
		if err := runSourceServiceRestore(t.Context(), p, dir, json.NewEncoder(&output)); err != nil {
			if pool, readErr := readHostPool(p.Root); readErr == nil {
				for _, g := range pool.Generations {
					if g.SourceService != nil {
						if raw, readErr := os.ReadFile(filepath.Join(g.Directory, "host.log")); readErr == nil {
							t.Log("replacement Host log:", string(raw))
						}
					}
				}
			}
			t.Fatal("full fixture maintenance", err)
		}
		if err := json.Unmarshal(output.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		if j.Phase == "source-service-active" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture maintenance did not converge", j.Phase)
		}
	}
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("old owned Host was not reaped")
	}
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(op.Custody.Path)
	if err != nil || !bytes.Equal(custody, after) {
		t.Fatal("Source restore changed business custody", err)
	}
	pool, err := readHostPool(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Active != j.Generation.ID || len(pool.Queue) != 1 || pool.Queue[0].State != "queued" {
		t.Fatal("Source role changed old queue", pool)
	}
	if err = sourceServicePoolHeld(pool, p); err != nil {
		t.Fatal(err)
	}
	resources, err := nativeHostRPC(t.Context(), j.Generation.Directory, hostRequest{Operation: "resources"})
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Contexts  int `json:"contexts"`
		Admission struct {
			CanAccept bool   `json:"can_accept"`
			Status    string `json:"status"`
		} `json:"admission"`
		Resources admission.Snapshot `json:"resources"`
	}
	if err = json.Unmarshal(resources, &observed); err != nil || observed.Contexts != 0 || observed.Admission.CanAccept || observed.Admission.Status != "service_only" || observed.Resources.Maximum != 9 || observed.Resources.Active != 0 {
		t.Fatal("Source native role/resource boundary failed", string(resources), err)
	}
	raw, err := nativeHostRPC(t.Context(), j.Generation.Directory, hostRequest{Operation: "attach"})
	if err != nil {
		t.Fatal(err)
	}
	var rejected hostResponse
	if json.Unmarshal(raw, &rejected) != nil || rejected.Status != "refused" {
		t.Fatal("Source role accepted business dispatch", string(raw))
	}
	if output := os.Getenv("NOODLE_SOURCE_RESTORE_FIXTURE_EVIDENCE"); output != "" {
		observations := map[string]json.RawMessage{}
		for _, name := range []string{"source-compute-qualified.json", "source-gateway-normal-exit.json", "source-host-stop.json", "source-host-stop-ack.json", "source-driver-stop.json", "source-driver-stop-ack.json", "source-gateway-term.json", "source-gateway-term-ack.json", "source-host-kill.json", "source-host-kill-ack.json", "source-quota-before.json", "source-quota-empty.json", "source-quota-migrated.json", "source-retired-phase.json"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			observations[name] = data
		}
		if j.SourceService.Compute != nil {
			data, err := os.ReadFile(j.SourceService.Compute.Path)
			if err != nil {
				t.Fatal(err)
			}
			observations["qualified-compute"] = data
		}
		service, err := nativeHostRPC(t.Context(), j.Generation.Directory, hostRequest{Operation: "source_service_readback"})
		if err != nil {
			t.Fatal(err)
		}
		targetRef, err := hostReadbackBinaryRef(target)
		if err != nil {
			t.Fatal(err)
		}
		evidence := map[string]any{"scope": "isolated synthetic Source snapshot/old Host RPC/gateway store; real replacement Noodle/kernel controls; not production Source/operator/worker acceptance", "target": targetRef, "original_snapshot": op, "journal": j, "observations": observations, "custody_before": json.RawMessage(custody), "custody_after": json.RawMessage(after), "pool": pool, "resources": json.RawMessage(resources), "business_refusal": rejected, "new_source_service": json.RawMessage(service)}
		if err = os.WriteFile(output, sourcePolicyTestJSON(t, evidence), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(p.Root, "finish-gateway"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var gateway procx.NativeIdentity
		if _, err = loopruntime.ReadSessionReadbackFile(filepath.Join(p.Root, "gateway.json"), &gateway); err != nil {
			t.Fatal(err)
		}
		gone, err := procx.ObserveNativeExit(gateway)
		if err == nil && gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new fixture gateway normal cleanup incomplete", err)
		}
	}
	// Only the isolated fixture Host created above is retired after its child
	// naturally exits. This is test cleanup, not product shutdown acceptance.
	current, err := procx.ObserveNativeIdentity(j.Generation.Process.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err = procx.SignalNativeIdentity(current, true); err != nil {
		t.Fatal(err)
	}
}
