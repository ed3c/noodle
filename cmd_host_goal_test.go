package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func goalHostFixture(t *testing.T) (root, goal, template, binary string) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ngh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	goal, binary = filepath.Join(root, "goal.json"), filepath.Join(root, "noodle")
	for path, content := range map[string]string{goal: `{"goal":"original accepted Goal"}`, binary: "immutable fixture binary"} {
		if err = os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	template = hostDeferralProfile(t)
	raw, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]any
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	gateway := profile["gateway"].(map[string]any)
	gateway["identity_source"], gateway["config_files"] = "gateway.toml", []string{"gateway.toml"}
	resident := gateway["resident"].(map[string]any)
	resident["argv"] = []string{binary, "--config", "gateway.toml", "--tls-cert", "server.crt", "--tls-key", "server.key", "--tls-client-ca", "ca.crt"}
	configuration := "[openshell]\nversion=2\n[openshell.gateway]\nname='fixture'\nbind_address='127.0.0.1:12345'\ncompute_driver='vm'\n[openshell.drivers.vm]\ndriver_dir='/immutable/driver'\nstate_dir='/original/vm'\n"
	if err = os.WriteFile(filepath.Join(filepath.Dir(template), "gateway.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(template, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return root, goal, template, binary
}

type goalHostFixtureServer struct {
	mu       sync.Mutex
	launches int
	blocked  map[string]bool
	legacy   bool
	full     bool
}

func (f *goalHostFixtureServer) launch(t *testing.T) func(context.Context, string, string) error {
	t.Helper()
	return func(_ context.Context, binary, root string) error {
		f.mu.Lock()
		f.launches++
		f.mu.Unlock()
		id := strings.Repeat("a", 32)
		ref, err := hostReadbackBinaryRef(binary)
		if err != nil {
			return err
		}
		if err = updateHostPool(root, func(p *hostPool) error {
			p.Active = id
			p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), Binary: ref.Path, SHA: ref.SHA256}
			return nil
		}); err != nil {
			return err
		}
		listener, err := net.Listen("unix", filepath.Join(root, "host.sock"))
		if err != nil {
			return err
		}
		t.Cleanup(func() { listener.Close() })
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
					f.mu.Lock()
					blocked, legacy, full := f.blocked[root], f.legacy, f.full
					f.mu.Unlock()
					resources := admission.Snapshot{AdmissionPolicy: admission.WorkerAdmissionPolicy, Maximum: admission.DefaultWorkerLimit}
					if full {
						resources.Active = admission.DefaultWorkerLimit
						resources.Reason = "concurrency limit"
					}
					row := map[string]any{"protocol": hostProtocol, "host_root": root, "host_instance_id": id, "binary_path": ref.Path, "binary_sha256": ref.SHA256, "resources": resources}
					if !legacy {
						status := "ready"
						if blocked {
							status = "blocked"
						}
						row["admission"] = map[string]any{"status": status, "can_accept": !blocked}
					}
					json.NewEncoder(conn).Encode(row)
				}()
			}
		}()
		return nil
	}
}

func TestHostGoalReuseIndependentHostsAndFullDoesNotRotate(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	f := &goalHostFixtureServer{blocked: map[string]bool{}}
	launch := f.launch(t)
	first, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", launch)
	if err != nil || first.Status != "ready" {
		t.Fatalf("first: %+v %v", first, err)
	}
	f.mu.Lock()
	f.full = true
	f.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			again, err := ensureGoalHost(context.Background(), root, goal, "", binary, "", launch)
			if err != nil || again.HostRoot != first.HostRoot || again.Status != "ready" {
				t.Errorf("full original Host rotated: %+v %v", again, err)
			}
		}()
	}
	wg.Wait()
	other := filepath.Join(root, "other.json")
	if err = os.WriteFile(other, []byte(`{"goal":"independent"}`), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := ensureGoalHost(context.Background(), root, other, template, binary, "", launch)
	if err != nil || second.HostRoot == first.HostRoot || f.launches != 2 {
		t.Fatalf("independent Goal shares Host: %+v %v", second, err)
	}
}

func TestHostGoalFaultReplacementAndExplicitSelectionAreIdempotent(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	f := &goalHostFixtureServer{blocked: map[string]bool{}}
	launch := f.launch(t)
	first, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", launch)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.blocked[first.HostRoot] = true
	f.mu.Unlock()
	second, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", launch)
	if err != nil || second.HostRoot == first.HostRoot {
		t.Fatalf("confirmed fault retained: %+v %v", second, err)
	}
	operation := filepath.Join(root, "operation.json")
	if err = os.WriteFile(operation, []byte(`{"new_host":"original operation"}`), 0600); err != nil {
		t.Fatal(err)
	}
	third, err := ensureGoalHost(context.Background(), root, goal, template, binary, operation, launch)
	if err != nil || third.HostRoot == second.HostRoot {
		t.Fatalf("explicit new Host missing: %+v %v", third, err)
	}
	for _, request := range []string{operation, "", operation} {
		again, err := ensureGoalHost(context.Background(), root, goal, "", binary, request, launch)
		if err != nil || again.HostRoot != third.HostRoot || f.launches != 3 {
			t.Fatalf("selection replayed: %+v %v", again, err)
		}
	}
}

func TestHostGoalUnknownCreationAndLegacyReadbackDoNotReplay(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	launches := 0
	launch := func(context.Context, string, string) error { launches++; return errors.New("creation outcome lost") }
	for i := 0; i < 2; i++ {
		result, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", launch)
		if err == nil || result.Status != "unknown" || launches != 1 {
			t.Fatalf("unknown creation replayed: %+v %v", result, err)
		}
	}
	root, goal, template, binary = goalHostFixture(t)
	f := &goalHostFixtureServer{blocked: map[string]bool{}, legacy: true}
	for i := 0; i < 2; i++ {
		result, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", f.launch(t))
		if err == nil || result.Status != "unknown" || f.launches != 1 {
			t.Fatalf("legacy became healthy: %+v %v", result, err)
		}
	}
}

func TestHostAdmissionDiagnosticSeparatesCapacityObservationAndCustody(t *testing.T) {
	b := admission.NewBudget(admission.DefaultWorkerLimit)
	b.Register("context")
	b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Critical, SwapUsedBytes: 90 << 30}, errors.New("sample unavailable"))
	var permits []*admission.Permit
	for range admission.DefaultWorkerLimit {
		permit, err := b.TryAcquire("context")
		if err != nil {
			t.Fatal(err)
		}
		permits = append(permits, permit)
	}
	t.Cleanup(func() {
		for _, permit := range permits {
			permit.Release()
		}
	})
	h := &noodleHost{budget: b}
	if row := h.admissionDiagnostic(); row["status"] != "ready" || row["can_accept"] != true {
		t.Fatalf("capacity or observation became Host fault: %+v", row)
	}
	b.SetUnresolvedResidents(1)
	if row := h.admissionDiagnostic(); row["status"] != "blocked" || row["error"] == "" {
		t.Fatalf("custody fault became healthy: %+v", row)
	}
	b.SetUnresolvedResidents(0)
	h.blocked = true
	if row := h.admissionDiagnostic(); row["status"] != "blocked" || row["can_accept"] != false {
		t.Fatalf("context fault became healthy: %+v", row)
	}
}

func TestHostGoalSocketFailureRequiresOriginalProcessAbsence(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	f := &goalHostFixtureServer{blocked: map[string]bool{}}
	launch := f.launch(t)
	first, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", launch)
	if err != nil {
		t.Fatal(err)
	}
	setProcess := func(record procx.ProcessRecord) {
		t.Helper()
		if err := updateHostPool(first.HostRoot, func(p *hostPool) error {
			g := p.Generations[p.Active]
			record.SessionID = g.ID
			g.Process = record
			p.Generations[p.Active] = g
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setProcess(procx.ProcessRecord{PID: os.Getpid(), StartedAt: time.Now()})
	if err = os.Remove(filepath.Join(first.HostRoot, "host.sock")); err != nil {
		t.Fatal(err)
	}
	unknown, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", launch)
	if err == nil || unknown.Status != "unknown" || f.launches != 1 {
		t.Fatalf("socket failure became death proof: %+v %v", unknown, err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^$")
	started := time.Now()
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	if err = child.Wait(); err != nil {
		t.Fatal(err)
	}
	setProcess(procx.ProcessRecord{PID: child.Process.Pid, StartedAt: started})
	replacement, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", launch)
	if err != nil || replacement.HostRoot == first.HostRoot || f.launches != 2 {
		t.Fatalf("confirmed absent Host blocked fresh task: %+v %v", replacement, err)
	}
}

func TestHostGoalInstalledTemplatePinsDependencies(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	registration, err := goalHostTemplatePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registerGoalHostTemplate(context.Background(), registration, template); err != nil {
		t.Fatal(err)
	}
	f := &goalHostFixtureServer{blocked: map[string]bool{}}
	first, err := ensureGoalHost(context.Background(), root, goal, "", binary, "", f.launch(t))
	if err != nil || first.Status != "ready" {
		t.Fatalf("normal dispatch required template assembly: %+v %v", first, err)
	}
	config := filepath.Join(filepath.Dir(template), "gateway.toml")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other.json")
	if err = os.WriteFile(other, []byte(`{"goal":"independent"}`), 0600); err != nil {
		t.Fatal(err)
	}
	refused, err := ensureGoalHost(context.Background(), root, other, "", binary, "", f.launch(t))
	if err == nil || refused.Status != "refused" || f.launches != 1 {
		t.Fatalf("changed template dependency admitted: %+v %v", refused, err)
	}
}

func goalHostProfileSelection(t *testing.T, root, goal, template, name string) goalHostSelection {
	t.Helper()
	goalRef, err := loopruntime.ReadSessionReadbackFile(goal, nil)
	if err != nil {
		t.Fatal(err)
	}
	templateRef, err := loopruntime.ReadSessionReadbackFile(template, nil)
	if err != nil {
		t.Fatal(err)
	}
	selected := goalHostSelection{Schema: 1, Goal: goalRef, Root: filepath.Join(root, "h", name), Template: templateRef, Phase: "materializing"}
	if err := os.MkdirAll(selected.Root, 0700); err != nil {
		t.Fatal(err)
	}
	return selected
}

func TestGoalHostProfilesConcurrentGoalsRejectRepeatedNativeCandidate(t *testing.T) {
	root, goal, template, _ := goalHostFixture(t)
	otherGoal := filepath.Join(root, "other-goal.json")
	if err := os.WriteFile(otherGoal, []byte(`{"goal":"another original Goal"}`), 0600); err != nil {
		t.Fatal(err)
	}
	selections := []goalHostSelection{
		goalHostProfileSelection(t, root, goal, template, "first"),
		goalHostProfileSelection(t, root, otherGoal, template, "second"),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	type observation struct {
		profile loopruntime.GoalProfileReceipt
		calls   int
		err     error
	}
	start := make(chan struct{})
	results := make(chan observation, len(selections))
	for _, selected := range selections {
		go func() {
			<-start
			calls := 0
			profile, err := materializeGoalHostProfile(context.Background(), selected, func() (net.Listener, error) {
				calls++
				if calls == 1 {
					return net.Listen("tcp", address)
				}
				return net.Listen("tcp", "127.0.0.1:0")
			})
			results <- observation{profile: profile, calls: calls, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent original profile selection failed: %v; %v", first.err, second.err)
	}
	if first.profile.Gateway == second.profile.Gateway || first.calls+second.calls != 3 {
		t.Fatalf("saved endpoint reused instead of rejecting the repeated candidate: %s (%d calls); %s (%d calls)", first.profile.Gateway, first.calls, second.profile.Gateway, second.calls)
	}
	if first.profile.Gateway != "https://"+address && second.profile.Gateway != "https://"+address {
		t.Fatal("neither original Goal retained the selected native candidate")
	}
	for _, selected := range selections {
		original, err := loopruntime.ReadGoalOpenShellProfile(selected.Root)
		if err != nil {
			t.Fatal(err)
		}
		again, err := materializeGoalHostProfile(context.Background(), selected, func() (net.Listener, error) {
			t.Error("existing original profile selected another endpoint")
			return nil, errors.New("unexpected endpoint selection")
		})
		if err != nil || again.Profile != original.Profile || again.Gateway != original.Gateway {
			t.Fatalf("existing original profile changed: %+v %v", again, err)
		}
	}
}

func TestGoalHostProfileRefusesMalformedOriginalBeforeEndpointSelection(t *testing.T) {
	for _, fault := range []string{"malformed", "missing", "missing-profile", "endpoint"} {
		t.Run(fault, func(t *testing.T) {
			root, goal, template, _ := goalHostFixture(t)
			original := goalHostProfileSelection(t, root, goal, template, "original")
			profile, err := materializeGoalHostProfile(context.Background(), original, func() (net.Listener, error) {
				return net.Listen("tcp", "127.0.0.1:0")
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(original.Root, "openshell", "materialization.json")
			switch fault {
			case "missing":
				err = os.Remove(path)
			case "missing-profile":
				err = os.Remove(profile.Profile.Path)
			case "malformed":
				err = os.WriteFile(path, []byte("{"), 0600)
			case "endpoint":
				profile.Gateway = "https://127.0.0.1:1"
				var raw []byte
				raw, err = json.Marshal(profile)
				if err == nil {
					err = os.WriteFile(path, raw, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			selected := goalHostProfileSelection(t, root, goal, template, "next")
			_, err = materializeGoalHostProfile(context.Background(), selected, func() (net.Listener, error) {
				t.Error("unreadable original profile was ignored during endpoint selection")
				return nil, errors.New("unexpected endpoint selection")
			})
			if err == nil {
				t.Fatal("unreadable original profile allowed another profile to publish")
			}
			if _, err := os.Stat(filepath.Join(selected.Root, "openshell")); !os.IsNotExist(err) {
				t.Fatalf("refused selection published a profile: %v", err)
			}
		})
	}
}

func TestGoalHostEndpointReservationDoesNotReadHistoricalDispatchDependencies(t *testing.T) {
	for _, fault := range []string{"template", "payload"} {
		t.Run(fault, func(t *testing.T) {
			root, goal, template, _ := goalHostFixture(t)
			original := goalHostProfileSelection(t, root, goal, template, "original")
			profile, err := materializeGoalHostProfile(context.Background(), original, func() (net.Listener, error) {
				return net.Listen("tcp", "127.0.0.1:0")
			})
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "template":
				err = os.Remove(template)
			case "payload":
				payload := filepath.Join(filepath.Dir(template), "payload")
				err = os.Rename(payload, payload+"-unavailable")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loopruntime.ReadGoalOpenShellProfile(original.Root); err == nil {
				t.Fatal("historical dispatch validation ignored its missing dependency")
			}
			_, nextGoal, nextTemplate, _ := goalHostFixture(t)
			selected := goalHostProfileSelection(t, root, nextGoal, nextTemplate, "next")
			calls := 0
			next, err := materializeGoalHostProfile(context.Background(), selected, func() (net.Listener, error) {
				calls++
				if calls == 1 {
					return net.Listen("tcp", strings.TrimPrefix(profile.Gateway, "https://"))
				}
				return net.Listen("tcp", "127.0.0.1:0")
			})
			if err != nil || next.Gateway == profile.Gateway || calls != 2 {
				t.Fatalf("historical dependency affected endpoint reservation: %s (%d calls), %v", next.Gateway, calls, err)
			}
			if _, err := loopruntime.ReadGoalOpenShellProfile(selected.Root); err != nil {
				t.Fatalf("current Goal did not retain full dispatch validation: %v", err)
			}
		})
	}
}

func TestHostGoalMissingProfileReadbackDoesNotRecreateOrReplace(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	f := &goalHostFixtureServer{blocked: map[string]bool{}}
	launch := f.launch(t)
	first, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", func(ctx context.Context, binary, hostRoot string) error {
		lock, err := lockfile.TryLock(filepath.Join(filepath.Dir(hostRoot), "profiles.lock"))
		if err != nil {
			return fmt.Errorf("profile lock still held during Host launch: %w", err)
		}
		lock.Close()
		return launch(ctx, binary, hostRoot)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(first.HostRoot, "openshell")); err != nil {
		t.Fatal(err)
	}
	unknown, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", launch)
	if err == nil || unknown.Status != "unknown" || unknown.HostRoot != first.HostRoot || f.launches != 1 {
		t.Fatalf("missing original profile changed the Host selection: %+v %v", unknown, err)
	}
	if _, err := os.Stat(filepath.Join(first.HostRoot, "openshell")); !os.IsNotExist(err) {
		t.Fatalf("readback recreated the missing original profile: %v", err)
	}
}
