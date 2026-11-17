package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func smallGoalTemplateFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	root, goal, template, binary := goalHostFixture(t)
	var document map[string]any
	raw, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, value := range document["routes"].(map[string]any) {
		name := value.(map[string]any)["spec_file"].(string)
		if !filepath.IsAbs(name) {
			name = filepath.Join(filepath.Dir(template), name)
		}
		raw, err = os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var spec map[string]any
		if err = json.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		spec["template"].(map[string]any)["image"] = "nvcr.io/nvidia/base/ubuntu@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686"
		raw, err = json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(name, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := prepareBudgetedGoalHostTemplate(context.Background(), filepath.Join(root, "config", "host-template.json"), template, loopruntime.QualificationGoalResourceBudget, goalResourceHardware{swap: 10 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return root, goal, ref.Path, binary
}

func TestHostQualificationMissingGoalFailsBeforeCustodyAndDifferentGoalsReuseClass(t *testing.T) {
	if goruntime.GOARCH != "arm64" {
		return
	}
	root, goal, template, binary := smallGoalTemplateFixture(t)
	server := &goalHostFixtureServer{blocked: map[string]bool{}}
	if _, err := ensureFixtureGoalHost(context.Background(), root, goal, template, binary, "", server.launch(t)); !errors.Is(err, admission.ErrDeferred) {
		t.Fatal("missing original Goal restriction did not HOLD", err)
	}
	for _, name := range []string{"h", "goals"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatal("missing Goal contract created custody", name, err)
		}
	}
	for _, name := range []string{"first", "second"} {
		source := filepath.Join(root, name+"-goal.json")
		raw, _ := json.Marshal(map[string]any{"goal": name + " accepted qualification Goal", "execution_constraints": map[string]string{"dependency_installation": "forbidden"}})
		if err := os.WriteFile(source, raw, 0600); err != nil {
			t.Fatal(err)
		}
		result, err := ensureFixtureGoalHost(context.Background(), root, source, template, binary, "", server.launch(t))
		if err != nil || result.Status != "ready" {
			t.Fatal("second legal Goal could not reuse same class", result, err)
		}
	}
	if server.launches != 2 {
		t.Fatal("reusable class was coupled to one Goal", server.launches)
	}
	profile, err := loopruntime.LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "first-goal.json")
	ref, err := loopruntime.ReadSessionReadbackFile(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, []byte(`{"goal":"changed","execution_constraints":{"dependency_installation":"allowed"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loopruntime.ReadOpenShellGoalExecutionConstraints(profile, ref); !errors.Is(err, admission.ErrDeferred) {
		t.Fatal("changed Goal inherited old restriction", err)
	}
}

func TestHostTemplatePrepareOnlyLeavesDefaultRegistrationAndReusesBaseline(t *testing.T) {
	root, _, template, _ := goalHostFixture(t)
	path := isolatedGoalTemplatePath(t, root)
	var err error
	if _, err = registerGoalHostTemplate(context.Background(), path, template); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newHostGoalTemplateCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--profile", template, "--prepare-only"})
	if err = cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct{ Status string }
	if err = json.Unmarshal(output.Bytes(), &result); err != nil || result.Status != "prepared" {
		t.Fatal("missing prepared Owner return", err, output.String())
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("prepare-only changed the default")
	}
	partial := newHostGoalTemplateCmd()
	partial.SetOut(&bytes.Buffer{})
	partial.SetErr(&bytes.Buffer{})
	partial.SetArgs([]string{"--profile", template, "--prepare-only", "--gateway-artifact", filepath.Join(root, "missing-manifest.json")})
	if err = partial.Execute(); err == nil {
		t.Fatal("partial pairing manifest flags were not refused")
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("partial pairing changed the default")
	}
	prepared, err := prepareBudgetedGoalHostTemplate(context.Background(), path, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 10 << 30})
	if err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("budget preparation registered before qualification")
	}
	published, err := registerBudgetedGoalHostTemplate(context.Background(), path, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 100 << 30})
	if err != nil || prepared != published {
		t.Fatal("formal registration reissued/racheted prepared policy", err)
	}
	loaded, err := loopruntime.LoadOpenShellProfile(published.Path)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := loopruntime.OpenShellPreparedWorkerResourceClaim(loaded, root)
	if err != nil || claim.Policy.MaxSwapUsedBytes != 11<<30 {
		t.Fatal("prepared original baseline was lost", err, claim.Policy)
	}
}
