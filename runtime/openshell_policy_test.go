package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	policy "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/protobuf/proto"
)

func taskNetworkPolicy(host string) *policy.SandboxPolicy {
	return &policy.SandboxPolicy{NetworkPolicies: map[string]*policy.NetworkPolicyRule{"docs": {Endpoints: []*policy.NetworkEndpoint{{Host: host, Port: 443}}}}}
}
func TestOpenShellRuntimeUsesEachContextsFrozenNativePolicy(t *testing.T) {
	first, f, target := openShellRuntimeFixture(t)
	first.config.Profile.Routes["codex"].Spec.Policy = taskNetworkPolicy("first.example.invalid")
	targetB := filepath.Join(t.TempDir(), "repo-b")
	for _, args := range [][]string{{"clone", "--quiet", "--template=", target, targetB}, {"-C", targetB, "config", "user.name", "Fixture"}, {"-C", targetB, "config", "user.email", "fixture@example.invalid"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture repository: %v %s", err, output)
		}
	}
	profileB := freezeOpenShellProfile(first.config.Profile)
	profileB.Routes["codex"].Spec.Policy = taskNetworkPolicy("second.example.invalid")
	second, err := NewOpenShellRuntime(OpenShellRuntimeConfig{ContextID: "b", RuntimeDir: filepath.Join(t.TempDir(), "runtime-b"), Gateway: f.owner, Profile: profileB, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	profileB.Routes["codex"].Spec.Policy.NetworkPolicies["docs"].Endpoints[0].Host = "unadmitted.example.invalid"
	expected := map[string]string{}
	for _, selected := range []struct {
		runtime            *OpenShellRuntime
		target, name, host string
	}{{first, target, "task-a", "first.example.invalid"}, {second, targetB, "task-b", "second.example.invalid"}} {
		handle, err := selected.runtime.Dispatch(t.Context(), DispatchRequest{Name: selected.name, Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: selected.target, AllowPrimaryCheckout: true})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-handle.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("native fixture did not settle")
		}
		if handle.Outcome().Status != StatusCompleted {
			t.Fatalf("native fixture: %+v", handle.Outcome())
		}
		expected[handle.ID()] = selected.host
		if handle.(*openShellSession).receipt.ProfileSHA256 == "" {
			t.Fatal("dispatch profile digest missing")
		}
	}
	snapshot := f.owner.custody.Snapshot()
	if len(snapshot.Records) != 2 {
		t.Fatalf("native records=%d", len(snapshot.Records))
	}
	for _, record := range snapshot.Records {
		spec := new(pb.SandboxSpec)
		if err = proto.Unmarshal(record.Spec, spec); err != nil {
			t.Fatal(err)
		}
		if got := spec.GetPolicy().GetNetworkPolicies()["docs"].GetEndpoints()[0].GetHost(); got != expected[record.Task.SessionID] {
			t.Fatalf("native policy=%s for %s", got, record.Task.SessionID)
		}
	}
	f.spawns(t, 1)
	if _, err = f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.Release("b"); err != nil {
		t.Fatal(err)
	}
}
func TestOpenShellTaskProfileDriftRefusesBeforeNativeAdmission(t *testing.T) {
	runtime, f, target := openShellRuntimeFixture(t)
	filename, _ := profileFixture(t)
	profile, err := LoadOpenShellProfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	runtime.config.Profile = profile
	if err = os.WriteFile(filepath.Join(filepath.Dir(filename), "spec.json"), []byte(`{"template":{"image":"changed"},"providers":["noodle-codex"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.Dispatch(t.Context(), DispatchRequest{Name: "drift", Provider: "codex", Model: "fixture", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true}); err == nil {
		t.Fatal("changed policy dispatched")
	}
	if len(runtime.active) != 0 {
		t.Fatal("changed policy retained a dispatched handle")
	}
	records, err := f.residents.Snapshot()
	if err != nil || len(records) != 0 {
		t.Fatalf("changed policy acquired native resident: %+v %v", records, err)
	}
}
