package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

type goalResourceHardware struct{ swap uint64 }

func (h goalResourceHardware) Read(context.Context) (admission.Sample, error) {
	return admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: 8, SwapUsedBytes: h.swap}, nil
}

func TestHostGoalResourceBudgetRegistersOnceAndNormalGoalNeedsNoManualClass(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	registration := filepath.Join(root, "config", "host-template.json")
	issued, err := registerBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := registerBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{})
	if err != nil || again != issued {
		t.Fatal("same policy required another selection", err)
	}
	var installed goalHostTemplateRegistration
	raw, err := os.ReadFile(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &installed); err != nil || installed.Profile != issued {
		t.Fatal("original registration was bypassed", err)
	}
	profile, err := loopruntime.LoadOpenShellProfile(issued.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = loopruntime.OpenShellPreparedWorkerResourceClaim(profile, root); err != nil {
		t.Fatal(err)
	}
	// The normal consumer already selects the registered profile; no task field
	// carries estimates or policy decisions. Keep its existing native server seam.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "native-config"))
	nativeRegistration, err := goalHostTemplatePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registerGoalHostTemplate(context.Background(), nativeRegistration, issued.Path); err != nil {
		t.Fatal(err)
	}
	server := &goalHostFixtureServer{blocked: map[string]bool{}}
	result, err := ensureFixtureGoalHost(context.Background(), root, goal, "", binary, "", server.launch(t))
	if err != nil || result.Status != "ready" {
		t.Fatal("normal Goal required per-task budget assembly", result, err)
	}
	loaded, err := loopruntime.LoadOpenShellProfile(result.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = loopruntime.OpenShellLifecycleResourceClaim(loaded, "host-resident", root); err != nil {
		t.Fatal("class was lost at materialization", err)
	}
}

func TestHostGoalResourceBudgetClassesShareOriginalStockPolicyWithoutRatcheting(t *testing.T) {
	if goruntime.GOARCH != "arm64" {
		return
	} // The separate runtime control verifies this typed unsupported architecture.
	root, _, template, _ := goalHostFixture(t)
	var profile map[string]any
	raw, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	for _, value := range profile["routes"].(map[string]any) {
		name := value.(map[string]any)["spec_file"].(string)
		if !filepath.IsAbs(name) {
			name = filepath.Join(filepath.Dir(template), name)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var spec map[string]any
		if err = json.Unmarshal(data, &spec); err != nil {
			t.Fatal(err)
		}
		spec["template"].(map[string]any)["image"] = "nvcr.io/nvidia/base/ubuntu@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686"
		data, err = json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	registration := filepath.Join(root, "config", "host-template.json")
	first, err := registerBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.QualificationGoalResourceBudget, goalResourceHardware{swap: 10 << 30})
	if err != nil {
		t.Fatal(err)
	}
	firstProfile, err := loopruntime.LoadOpenShellProfile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	firstClaim, err := loopruntime.OpenShellPreparedWorkerResourceClaim(firstProfile, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registerBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 100 << 30})
	if err != nil {
		t.Fatal(err)
	}
	secondProfile, err := loopruntime.LoadOpenShellProfile(second.Path)
	if err != nil {
		t.Fatal(err)
	}
	secondClaim, err := loopruntime.OpenShellPreparedWorkerResourceClaim(secondProfile, root)
	if err != nil {
		t.Fatal(err)
	}
	if firstClaim.Policy != secondClaim.Policy || firstClaim.Policy.MaxSwapUsedBytes != 11<<30 {
		t.Fatal("class change resampled or ratcheted machine stock policy", firstClaim.Policy, secondClaim.Policy)
	}
}
