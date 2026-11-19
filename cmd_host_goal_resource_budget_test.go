package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

type goalResourceHardware struct {
	swap       uint64
	unknownRAM bool
}

func (h goalResourceHardware) Read(context.Context) (admission.Sample, error) {
	sample := admission.Sample{CPUs: 8, SwapUsedBytes: h.swap}
	if !h.unknownRAM {
		sample.TotalMemoryBytes = 32 << 30
	}
	return sample, nil
}

func legacyGoalResourcePublicationFixture(t *testing.T, registration, template string) loopruntime.GoalProfileReceipt {
	t.Helper()
	source, err := loopruntime.ReadSessionReadbackFile(template, nil)
	if err != nil {
		t.Fatal(err)
	}
	template = source.Path
	original, err := loopruntime.LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := json.Marshal(struct {
		Class, Source string
		Memory        uint64
		CPUs          int
	}{loopruntime.ConservativeGoalResourceBudget, original.SourceSHA256, 32 << 30, 8})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(registration), "resource-templates", hostDigest(identity))
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	receipt, err := loopruntime.MaterializeBudgetedGoalOpenShellProfile(template, root, loopruntime.ConservativeGoalResourceBudget, admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: 8, SwapUsedBytes: 10 << 30}, noGoalMachinePolicy)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = loopruntime.ReadGoalOpenShellProfile(root)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the already-published legacy single-file representation only
	// inside this fixture. Its original policy and receipt are retained below.
	var document map[string]any
	raw, err := os.ReadFile(receipt.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	gateway := document["gateway"].(map[string]any)
	owner := gateway["identity_source"].(string)
	gateway["config_files"] = []string{owner}
	argv := gateway["resident"].(map[string]any)["argv"].([]any)
	for i, arg := range argv {
		if arg == "--config" {
			argv[i+1] = owner
		}
	}
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(receipt.Profile.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	receipt.Profile.SHA256 = hostDigest(raw)
	receipt.Files["profile.json"] = receipt.Profile.SHA256
	delete(receipt.Files, "gateway-native.toml")
	if err = os.Remove(filepath.Join(root, "openshell", "gateway-native.toml")); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "openshell", "materialization.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loopruntime.ReadGoalOpenShellProfile(root); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestHostGoalResourceTemplateFormatCarriesLegacyBaselineWithoutRewriting(t *testing.T) {
	for _, fault := range []string{"none", "source-drift", "missing-receipt", "changed-policy", "compatible-baseline", "conflicting-baseline"} {
		t.Run(fault, func(t *testing.T) {
			root, _, template, _ := goalHostFixture(t)
			registration := isolatedGoalTemplatePath(t, root)
			legacy := legacyGoalResourcePublicationFixture(t, registration, template)
			legacyProfile, err := loopruntime.LoadOpenShellProfile(legacy.Profile.Path)
			if err != nil {
				t.Fatal(err)
			}
			oldClaim, err := loopruntime.OpenShellPreparedWorkerResourceClaim(legacyProfile, root)
			if err != nil {
				t.Fatal(err)
			}
			receiptPath := filepath.Join(legacy.HostRoot, "openshell", "materialization.json")
			switch fault {
			case "compatible-baseline", "conflicting-baseline":
				registeredRoot := filepath.Join(root, "registered-policy")
				if err = os.Mkdir(registeredRoot, 0700); err != nil {
					t.Fatal(err)
				}
				stock := uint64(10 << 30)
				if fault == "conflicting-baseline" {
					stock = 20 << 30
				}
				registered, err := loopruntime.MaterializeBudgetedGoalOpenShellProfile(template, registeredRoot, loopruntime.ConservativeGoalResourceBudget, admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: 8, SwapUsedBytes: stock}, noGoalMachinePolicy)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = registerGoalHostTemplate(context.Background(), registration, registered.Profile.Path); err != nil {
					t.Fatal(err)
				}
			case "source-drift":
				raw, err := os.ReadFile(legacy.Profile.Path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(legacy.Profile.Path, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-receipt":
				if err = os.Remove(receiptPath); err != nil {
					t.Fatal(err)
				}
			case "changed-policy":
				config := legacyProfile.Gateway.Binding.ConfigSource
				raw, err := os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				var document map[string]any
				if _, err = toml.Decode(string(raw), &document); err != nil {
					t.Fatal(err)
				}
				worker := document["noodle_worker_resources"].(map[string]any)
				worker["memory_peak_mib"] = worker["memory_peak_mib"].(int64) + 1
				var encoded bytes.Buffer
				if err = toml.NewEncoder(&encoded).Encode(document); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(config, encoded.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				legacy.Files["gateway.toml"] = hostDigest(encoded.Bytes())
				raw, err = json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(receiptPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err = loopruntime.ReadGoalOpenShellProfile(legacy.HostRoot); err != nil {
					t.Fatal("changed-policy fixture lacks a valid original receipt", err)
				}
			}
			registrationBefore, registrationErr := os.ReadFile(registration)
			if registrationErr != nil && !os.IsNotExist(registrationErr) {
				t.Fatal(registrationErr)
			}
			before := map[string]string{}
			if err = filepath.WalkDir(legacy.HostRoot, func(path string, entry os.DirEntry, err error) error {
				if err != nil || !entry.Type().IsRegular() {
					return err
				}
				raw, err := os.ReadFile(path)
				before[path] = hostDigest(raw)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			issued, err := prepareFixtureBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 100 << 30})
			if fault == "none" || fault == "compatible-baseline" {
				if err != nil || issued == legacy.Profile {
					t.Fatal("new format reused the legacy representation", issued, err)
				}
				profile, err := loopruntime.LoadOpenShellProfile(issued.Path)
				if err != nil {
					t.Fatal(err)
				}
				claim, err := loopruntime.OpenShellPreparedWorkerResourceClaim(profile, root)
				if err != nil || claim.Policy != oldClaim.Policy || claim.Policy.MaxSwapUsedBytes != 11<<30 || claim.MemoryBytes != oldClaim.MemoryBytes || claim.DiskBytes != oldClaim.DiskBytes || claim.CPUs != oldClaim.CPUs {
					t.Fatal("format changed the original resource obligation", claim, oldClaim, err)
				}
				if _, err = os.Stat(filepath.Join(filepath.Dir(issued.Path), "gateway-native.toml")); err != nil {
					t.Fatal(err)
				}
				again, err := prepareFixtureBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 200 << 30})
				if err != nil || again != issued {
					t.Fatal("steady reentry changed the publication or stock", again, err)
				}
			} else if err == nil {
				t.Fatal("unproven original publication was silently replaced", issued)
			} else if fault == "changed-policy" && !strings.Contains(err.Error(), "policy semantics differ") {
				t.Fatal("valid but changed policy was not rejected by the policy reader", err)
			} else if fault == "conflicting-baseline" && !strings.Contains(err.Error(), "baselines conflict") {
				t.Fatal("conflicting original policies were not a named refusal", err)
			}
			for path, digest := range before {
				raw, err := os.ReadFile(path)
				if err != nil || hostDigest(raw) != digest {
					t.Fatal("legacy publication was rewritten", path, err)
				}
			}
			registrationAfter, err := os.ReadFile(registration)
			if os.IsNotExist(registrationErr) {
				if !os.IsNotExist(err) {
					t.Fatal("preparation created a default registration", err)
				}
			} else if err != nil || !bytes.Equal(registrationBefore, registrationAfter) {
				t.Fatal("preparation changed the default registration", err)
			}
		})
	}
}

func TestHostGoalResourceBudgetRegistersOnceAndNormalGoalNeedsNoManualClass(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	registration := filepath.Join(root, "config", "host-template.json")
	issued, err := registerFixtureBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := registerFixtureBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{})
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
	nativeRegistration := isolatedGoalTemplatePath(t, root)
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
	first, err := registerFixtureBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.QualificationGoalResourceBudget, goalResourceHardware{swap: 10 << 30})
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
	second, err := registerFixtureBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 100 << 30})
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
	conflictingRegistration := filepath.Join(root, "conflicting", "host-template.json")
	candidate, err := prepareFixtureBudgetedGoalHostTemplate(context.Background(), conflictingRegistration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	candidateProfile, err := loopruntime.LoadOpenShellProfile(candidate.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registerGoalHostTemplate(context.Background(), conflictingRegistration, first.Path); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareFixtureBudgetedGoalHostTemplate(context.Background(), conflictingRegistration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{unknownRAM: true}); err == nil || !strings.Contains(err.Error(), "baselines conflict") {
		t.Fatal("prepared template replaced a conflicting registered baseline", err)
	}
	if err = loopruntime.VerifyOpenShellProfile(candidateProfile); err != nil {
		t.Fatal("rejected preparation rewrote the existing candidate", err)
	}
}

func noGoalMachinePolicy() (*admission.MachineCapacityPolicyReadback, error) { return nil, nil }

func TestHostGoalResourceBudgetReusesBoundPublicationWhenRAMObservationChanges(t *testing.T) {
	for _, tc := range []struct {
		name           string
		initialUnknown bool
		registered     bool
	}{
		{name: "losing RAM observation on registered template", registered: true},
		{name: "losing RAM observation on prepared template"},
		{name: "recovering RAM observation on prepared template", initialUnknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, template, _ := goalHostFixture(t)
			registration := isolatedGoalTemplatePath(t, root)
			first := goalResourceHardware{swap: 10 << 30, unknownRAM: tc.initialUnknown}
			issue := prepareFixtureBudgetedGoalHostTemplate
			if tc.registered {
				issue = registerFixtureBudgetedGoalHostTemplate
			}
			issued, err := issue(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, first)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := loopruntime.LoadOpenShellProfile(issued.Path)
			if err != nil {
				t.Fatal(err)
			}
			second := goalResourceHardware{swap: 100 << 30, unknownRAM: !tc.initialUnknown}
			again, err := prepareBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, second, func() (*admission.MachineCapacityPolicyReadback, error) {
				t.Error("bound publication read current machine policy")
				return nil, nil
			})
			if err != nil || again != issued {
				t.Fatal("RAM observation changed bound template", again, err)
			}
			if err = loopruntime.VerifyOpenShellProfile(profile); err != nil {
				t.Fatal("reentry rewrote publication bytes", err)
			}
		})
	}
}

func TestHostGoalResourceBudgetReusesPreviousPreparedPublication(t *testing.T) {
	root, _, template, _ := goalHostFixture(t)
	registration := isolatedGoalTemplatePath(t, root)
	source, err := loopruntime.ReadSessionReadbackFile(template, nil)
	if err != nil {
		t.Fatal(err)
	}
	original, err := loopruntime.LoadOpenShellProfile(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	const class = loopruntime.ConservativeGoalResourceBudget
	previousRoot, err := previousGoalResourceTemplateRoot(registration, class, original.SourceSHA256, 32<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(previousRoot, 0700); err != nil {
		t.Fatal(err)
	}
	old, err := loopruntime.MaterializeBudgetedGoalOpenShellProfile(source.Path, previousRoot, class, admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: 8, SwapUsedBytes: 10 << 30}, noGoalMachinePolicy)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := prepareFixtureBudgetedGoalHostTemplate(context.Background(), registration, source.Path, class, goalResourceHardware{swap: 100 << 30})
	if err != nil || issued != old.Profile {
		t.Fatal("verified previous publication was replaced", issued, err)
	}
}

func prepareFixtureBudgetedGoalHostTemplate(ctx context.Context, path, profile, class string, sampler admission.Sampler) (loopruntime.SessionReadbackFileRef, error) {
	return prepareBudgetedGoalHostTemplate(ctx, path, profile, class, sampler, noGoalMachinePolicy)
}

func registerFixtureBudgetedGoalHostTemplate(ctx context.Context, path, profile, class string, sampler admission.Sampler) (loopruntime.SessionReadbackFileRef, error) {
	ref, err := prepareFixtureBudgetedGoalHostTemplate(ctx, path, profile, class, sampler)
	if err != nil {
		return ref, err
	}
	return registerGoalHostTemplate(ctx, path, ref.Path)
}

func TestHostGoalResourceBudgetPublishesFixedPolicyBesidePriorFormat(t *testing.T) {
	root, _, template, _ := goalHostFixture(t)
	registration := isolatedGoalTemplatePath(t, root)
	source, err := loopruntime.ReadSessionReadbackFile(template, nil)
	if err != nil {
		t.Fatal(err)
	}
	original, err := loopruntime.LoadOpenShellProfile(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := json.Marshal(struct {
		Class, Source string
		Memory        uint64
		CPUs          int
	}{loopruntime.ConservativeGoalResourceBudget, original.SourceSHA256, 32 << 30, 8})
	if err != nil {
		t.Fatal(err)
	}
	identity, err = json.Marshal(struct {
		Format   string
		Identity json.RawMessage
	}{"native-dual-config-v1", identity})
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := filepath.Join(filepath.Dir(registration), "resource-templates", hostDigest(identity))
	if err = os.MkdirAll(oldRoot, 0700); err != nil {
		t.Fatal(err)
	}
	old, err := loopruntime.MaterializeBudgetedGoalOpenShellProfile(source.Path, oldRoot, loopruntime.ConservativeGoalResourceBudget, admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: 8, SwapUsedBytes: 10143 << 20}, noGoalMachinePolicy)
	if err != nil {
		t.Fatal(err)
	}
	oldProfile, err := loopruntime.LoadOpenShellProfile(old.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, path := range []string{old.Profile.Path, oldProfile.Gateway.Binding.ConfigSource, filepath.Join(oldRoot, "openshell", "materialization.json")} {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	fixed := admission.MachineCapacityPolicyReadback{Source: "/native/machine-capacity/capacity.json", Capacity: admission.CapacityPolicy{MemoryBytes: 12 << 30, CPUs: 6, DiskSafetyBytes: 2 << 30, MaxSwapUsedBytes: 11080 << 20, MaxSwapoutBytesPerSecond: 1 << 20, RecoverySamples: 2}}
	issued, err := prepareBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 10143 << 20}, func() (*admission.MachineCapacityPolicyReadback, error) { return &fixed, nil })
	if err != nil || issued == old.Profile {
		t.Fatal("fixed producer reused obsolete policy publication", issued, err)
	}
	profile, err := loopruntime.LoadOpenShellProfile(issued.Path)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := loopruntime.OpenShellPreparedWorkerResourceClaim(profile, root)
	if err != nil || claim.Policy != fixed.Capacity {
		t.Fatal("new format did not inherit fixed machine policy", claim.Policy, err)
	}
	if _, ok, err := loopruntime.GoalResourceBudgetBaseline(profile, original, admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: 8}); err != nil || !ok {
		t.Fatal("new publication cannot verify itself", ok, err)
	}
	again, err := prepareBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalResourceHardware{swap: 100 << 30}, func() (*admission.MachineCapacityPolicyReadback, error) {
		t.Error("steady reentry read current policy")
		return nil, nil
	})
	if err != nil || again != issued {
		t.Fatal("fixed publication drifted on reentry", again, err)
	}
	for path, raw := range before {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("old publication was rewritten", path, err)
		}
	}
	oldClaim, err := loopruntime.OpenShellPreparedWorkerResourceClaim(oldProfile, root)
	if err != nil || oldClaim.Policy.MaxSwapUsedBytes != 11167<<20 {
		t.Fatal("old policy history changed", oldClaim.Policy, err)
	}
	if _, err = os.Lstat(registration); !os.IsNotExist(err) {
		t.Fatal("preparation changed global default", err)
	}
}

type goalUnknownCPUHardware struct{ cpus int }

func (h goalUnknownCPUHardware) Read(context.Context) (admission.Sample, error) {
	return admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: h.cpus}, nil
}

func TestHostGoalResourceBudgetCPUObservationCountDoesNotChangePublication(t *testing.T) {
	root, _, template, _ := goalHostFixture(t)
	registration := isolatedGoalTemplatePath(t, root)
	issued, err := prepareFixtureBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalUnknownCPUHardware{cpus: 1})
	if err != nil {
		t.Fatal("small CPU count blocked producer", err)
	}
	again, err := prepareFixtureBudgetedGoalHostTemplate(context.Background(), registration, template, loopruntime.ConservativeGoalResourceBudget, goalUnknownCPUHardware{cpus: 0})
	if err != nil || again != issued {
		t.Fatal("CPU count unknown changed publication or blocked producer", again, err)
	}
	p, err := loopruntime.LoadOpenShellProfile(issued.Path)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := loopruntime.OpenShellPreparedWorkerResourceClaim(p, root)
	if err != nil || claim.CPUs != 2 || claim.Policy.CPUs != 0 {
		t.Fatal("count metadata affected VM geometry", claim, err)
	}
	if _, err = os.Lstat(registration); !os.IsNotExist(err) {
		t.Fatal("prepare changed default registry", err)
	}
}
