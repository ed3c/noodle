package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestHostOpenShellProfileRefusalPrecedesRuntimeOrContext(t *testing.T) {
	root := t.TempDir()
	req := hostTestRequest(t, root)
	data := []byte("mode='supervised'\n[server]\nenabled=false\n[runtime.openshell]\nprofile='missing-profile.json'\n")
	if err := os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	req.ConfigSHA256 = hostDigest(data)
	h := &noodleHost{dir: t.TempDir(), instanceID: "instance", binaryPath: "/native/noodle", binarySHA: "test-binary", budget: admission.NewBudget(2)}
	var err error
	h.residents, err = admission.OpenResidents(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime { called = true; return nil }
	response := h.attach(context.Background(), req)
	if response.Status != "refused" || called || len(h.contexts) != 0 || len(h.receipts) != 0 || h.openShell != nil {
		t.Fatalf("unsafe attach: %+v called=%t", response, called)
	}
	records, err := h.residents.Snapshot()
	if err != nil || len(records) != 0 {
		t.Fatalf("resident spawned on refusal: %v %v", records, err)
	}
}

func TestHostOpenShellKeepsEachContextPolicy(t *testing.T) {
	first := loopruntime.OpenShellProfile{Gateway: loopruntime.OpenShellGatewaySpec{Binding: loopruntime.OpenShellCustodyBinding{ConfigSource: "/shared/gateway.toml", ConfigFingerprint: "same-deployment"}}, Routes: map[string]loopruntime.OpenShellRoute{"codex": {Binary: "first-task"}}}
	owner := &loopruntime.OpenShellGateway{}
	h := &noodleHost{openShell: owner, openShellProfile: &first, contexts: map[string]*hostContext{"first": {openShell: true, done: make(chan error)}}}
	second := first
	second.Routes = map[string]loopruntime.OpenShellRoute{"codex": {Binary: "second-task"}}
	selected, err := h.bindOpenShellProfile(second)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Routes["codex"].Binary != "second-task" || h.openShell != owner || h.openShellProfile != &first {
		t.Fatal("second context reused first worker policy or replaced gateway owner")
	}
	changed := second
	changed.Gateway.Binding.ConfigFingerprint = "changed-deployment"
	if _, err = h.bindOpenShellProfile(changed); err == nil {
		t.Fatal("changed shared gateway admitted")
	}
	if h.openShell != owner {
		t.Fatal("refusal lost original gateway owner")
	}
}

func TestHostOpenShellTwoAttachedContextsSelectDistinctPolicies(t *testing.T) {
	profile := hostDeferralProfile(t)
	directory := filepath.Dir(profile)
	firstSpec := `{"template":{"image":"fixture"},"providers":["noodle-codex"],"command":["hold"],"policy":{"networkPolicies":{"docs":{"endpoints":[{"host":"first.example.invalid","port":443}]}}}}`
	if err := os.WriteFile(filepath.Join(directory, "spec.json"), []byte(firstSpec), 0600); err != nil {
		t.Fatal(err)
	}
	secondSpec := strings.ReplaceAll(firstSpec, "first.example.invalid", "second.example.invalid")
	if err := os.WriteFile(filepath.Join(directory, "second-spec.json"), []byte(secondSpec), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	var secondProfile map[string]any
	if err = json.Unmarshal(data, &secondProfile); err != nil {
		t.Fatal(err)
	}
	secondProfile["routes"].(map[string]any)["codex"].(map[string]any)["spec_file"] = "second-spec.json"
	data, err = json.Marshal(secondProfile)
	if err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(directory, "second-profile.json")
	if err = os.WriteFile(secondPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{dir: t.TempDir(), instanceID: "instance", binaryPath: "/native/noodle", binarySHA: "test-binary", budget: admission.NewBudget(2), completed: make(chan string, 4)}
	h.residents, err = admission.OpenResidents(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	h.residents.BindBudget(h.budget)
	now := time.Now().Add(-time.Second)
	for i := range 4 {
		h.budget.Observe(admission.Sample{At: now.Add(time.Duration(i) * time.Millisecond), Pressure: admission.Normal}, nil)
	}
	var selected []string
	var gatewayOwners []*loopruntime.OpenShellGateway
	h.newRuntime = func(_ string, _ string, _ config.Config, deps loop.Dependencies) hostRuntime {
		selected = append(selected, deps.OpenShellProfile.Routes["codex"].Spec.GetPolicy().GetNetworkPolicies()["docs"].GetEndpoints()[0].GetHost())
		gatewayOwners = append(gatewayOwners, deps.OpenShellGateway)
		return &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
	}
	t.Cleanup(func() {
		for _, c := range h.contexts {
			if c.done != nil {
				c.cancel()
				h.finish(c, <-c.done)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.residents.WaitClosed(ctx); err != nil {
			t.Error(err)
		}
		if h.openShell != nil {
			if err := h.openShell.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	for _, filename := range []string{profile, secondPath} {
		root := t.TempDir()
		request := hostTestRequest(t, root)
		data := []byte(fmt.Sprintf("mode='supervised'\n[server]\nenabled=false\n[runtime.openshell]\nprofile=%q\n", filename))
		if err := os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
			t.Fatal(err)
		}
		request.ConfigSHA256 = hostDigest(data)
		if result := h.handle(context.Background(), request); result.Status != "ok" || result.ContextState != "active" {
			t.Fatalf("attach: %+v", result)
		}
	}
	if len(selected) != 2 || selected[0] != "first.example.invalid" || selected[1] != "second.example.invalid" || gatewayOwners[0] != gatewayOwners[1] {
		t.Fatalf("context profiles/owner differ: %v", selected)
	}
	secondProfile["gateway"].(map[string]any)["version"] = "changed-deployment"
	data, err = json.Marshal(secondProfile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(secondPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	request := hostTestRequest(t, root)
	data = []byte(fmt.Sprintf("mode='supervised'\n[server]\nenabled=false\n[runtime.openshell]\nprofile=%q\n", secondPath))
	if err = os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	request.ConfigSHA256 = hostDigest(data)
	if result := h.handle(context.Background(), request); result.Status != "refused" || len(selected) != 2 || len(h.contexts) != 2 {
		t.Fatalf("changed deployment admitted: %+v", result)
	}
	records, err := h.residents.Snapshot()
	if err != nil || len(records) != 1 || len(records[0].Contexts) != 2 {
		t.Fatalf("gateway sharing: %+v %v", records, err)
	}
}
