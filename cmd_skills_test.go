package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/config"
)

func TestSkillsListRespectsPrecedence(t *testing.T) {
	project := t.TempDir()
	user := t.TempDir()

	mustMkdirAll(t, filepath.Join(project, "review"))
	mustWriteFile(t, filepath.Join(project, "review", "SKILL.md"), "# review")
	mustMkdirAll(t, filepath.Join(user, "review"))
	mustMkdirAll(t, filepath.Join(user, "debug"))
	mustWriteFile(t, filepath.Join(user, "debug", "SKILL.md"), "# debug")

	app := &App{
		Config: config.Config{
			Skills: config.SkillsConfig{Paths: []string{project, user}},
		},
	}

	output := captureStdout(t, func() {
		err := runSkillsList(app)
		if err != nil {
			t.Fatalf("runSkillsList: %v", err)
		}
	})

	if !strings.Contains(output, "review\t"+project+"\ttrue\t") {
		t.Fatalf("expected project review skill in output: %q", output)
	}
	if !strings.Contains(output, "debug\t"+user+"\ttrue\t") {
		t.Fatalf("expected user debug skill in output: %q", output)
	}
	if strings.Contains(output, "review\t"+user+"\t") {
		t.Fatalf("expected user review skill to be shadowed: %q", output)
	}
}

func TestSkillsListJSONCLIFlagAndEmptyResolverRefusal(t *testing.T) {
	project := t.TempDir()
	app := &App{Config: config.Config{Skills: config.SkillsConfig{Paths: []string{project}}}}
	cmd := newSkillsListCmd(app)
	cmd.SetArgs([]string{"--json"})
	var resultErr error
	out := captureStdout(t, func() { resultErr = cmd.Execute() })
	if resultErr == nil || !strings.Contains(resultErr.Error(), "no Skills discovered") {
		t.Fatalf("empty resolver must refuse: %v", resultErr)
	}
	var receipt skillResolutionReceipt
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("empty resolver refusal must be machine-readable: %v", err)
	}
	if receipt.Status != "NO_SKILLS_DISCOVERED" || len(receipt.Skills) != 0 ||
		receipt.EffectAuthority || receipt.ActualWorkerSessionObserved {
		t.Fatalf("empty resolution was promoted to proof: %+v", receipt)
	}
	mustMkdirAll(t, filepath.Join(project, "poteto-mode"))
	mustWriteFile(t, filepath.Join(project, "poteto-mode", "SKILL.md"), "# poteto")
	second := newSkillsListCmd(app)
	second.SetArgs([]string{"--json"})
	out = captureStdout(t, func() { resultErr = second.Execute() })
	if resultErr != nil {
		t.Fatalf("flag wiring must use json command: %v", resultErr)
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("actual CLI flag route did not return JSON: %v", err)
	}
	if receipt.Status != "CONFIGURED_RESOLVER_SNAPSHOT" || len(receipt.Skills) != 1 {
		t.Fatalf("route did not observe configured skill: %+v", receipt)
	}
}

func TestSkillsListJSONProvidesExactWinnerEvidence(t *testing.T) {
	project := t.TempDir()
	user := t.TempDir()
	mustMkdirAll(t, filepath.Join(project, "poteto-mode", "references"))
	mustWriteFile(t, filepath.Join(project, "poteto-mode", "SKILL.md"), "# poteto v1")
	mustWriteFile(t, filepath.Join(project, "poteto-mode", "references", "entry.md"), "method")
	mustMkdirAll(t, filepath.Join(user, "impeccable"))
	mustWriteFile(t, filepath.Join(user, "impeccable", "SKILL.md"), "# impeccable")
	app := &App{Config: config.Config{Skills: config.SkillsConfig{Paths: []string{project, user}}}}
	var runErr error
	output := captureStdout(t, func() { runErr = runSkillsListJSON(app) })
	if runErr != nil {
		t.Fatalf("json resolution: %v", runErr)
	}
	var receipt skillResolutionReceipt
	if err := json.Unmarshal([]byte(output), &receipt); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if receipt.Status != "CONFIGURED_RESOLVER_SNAPSHOT" || receipt.CollisionDetected {
		t.Fatalf("unexpected resolver claim: %+v", receipt)
	}
	if len(receipt.Skills) != 2 || receipt.Skills[0].Name != "poteto-mode" {
		t.Fatalf("wrong precedence/order: %+v", receipt.Skills)
	}
	selected := receipt.Skills[0]
	expected := sha256.Sum256([]byte("# poteto v1"))
	if selected.SkillMDSHA256 != hex.EncodeToString(expected[:]) || len(selected.TreeSHA256) != 64 {
		t.Fatalf("wrong Skill bytes: %+v", selected)
	}
	if selected.SourcePath != project || len(selected.ShadowedPaths) != 0 {
		t.Fatalf("wrong source identity: %+v", selected)
	}
	if receipt.EffectiveAgentCatalogVerified || receipt.ActualWorkerSessionObserved ||
		receipt.GlobalSkillInheritanceExcluded || receipt.EffectAuthority {
		t.Fatalf("configured resolver was incorrectly granted Worker authority: %+v", receipt)
	}
	// The bytes are deterministic for unchanged configured discovery.
	second := captureStdout(t, func() { runErr = runSkillsListJSON(app) })
	if runErr != nil || output != second {
		t.Fatalf("read-only recheck drifted: %v", runErr)
	}
	mustWriteFile(t, filepath.Join(project, "poteto-mode", "references", "entry.md"), "tampered")
	changed := captureStdout(t, func() { runErr = runSkillsListJSON(app) })
	if runErr != nil {
		t.Fatal(runErr)
	}
	var newer skillResolutionReceipt
	if err := json.Unmarshal([]byte(changed), &newer); err != nil {
		t.Fatal(err)
	}
	if newer.Skills[0].TreeSHA256 == selected.TreeSHA256 || newer.Skills[0].SkillMDSHA256 != selected.SkillMDSHA256 {
		t.Fatalf("tree change must modify only the tree digest: %+v", newer.Skills[0])
	}
}

func TestSkillsListJSONDisclosesAndRefusesShadowedProviders(t *testing.T) {
	project := t.TempDir()
	global := t.TempDir()
	for _, dir := range []string{project, global} {
		mustMkdirAll(t, filepath.Join(dir, "poteto-mode"))
		mustWriteFile(t, filepath.Join(dir, "poteto-mode", "SKILL.md"), "# distinct provider")
	}
	app := &App{Config: config.Config{Skills: config.SkillsConfig{Paths: []string{project, global}}}}
	var failure error
	output := captureStdout(t, func() { failure = runSkillsListJSON(app) })
	if failure == nil || !strings.Contains(failure.Error(), "shadowed") {
		t.Fatalf("must fail closed on shadowing: %v", failure)
	}
	var receipt skillResolutionReceipt
	if err := json.Unmarshal([]byte(output), &receipt); err != nil {
		t.Fatalf("failure must still include parseable evidence: %v", err)
	}
	if receipt.Status != "SHADOWED_SKILL_SOURCES" ||
		!receipt.CollisionDetected || len(receipt.Skills[0].ShadowedPaths) != 1 ||
		receipt.Skills[0].ShadowedPaths[0] != filepath.Join(global, "poteto-mode") {
		t.Fatalf("hidden Skill not disclosed: %+v", receipt)
	}
	// Existing human-readable listing remains first-match-wins and unchanged.
	legacy := captureStdout(t, func() {
		if err := runSkillsList(app); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(legacy, "poteto-mode\t"+global+"\t") {
		t.Fatalf("legacy precedence changed: %s", legacy)
	}
}

func TestSkillsListJSONRefusesSymlinkedSkillBytes(t *testing.T) {
	project := t.TempDir()
	external := t.TempDir()
	mustMkdirAll(t, filepath.Join(project, "poteto-mode"))
	mustWriteFile(t, filepath.Join(external, "SKILL.md"), "# foreign")
	if err := os.Symlink(filepath.Join(external, "SKILL.md"), filepath.Join(project, "poteto-mode", "SKILL.md")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	app := &App{Config: config.Config{Skills: config.SkillsConfig{Paths: []string{project}}}}
	_ = captureStdout(t, func() {
		if err := runSkillsListJSON(app); err == nil {
			t.Error("must refuse a symlink-backed selected SKILL.md")
		}
	})
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = originalStdout
	})

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read pipe: %v", err)
	}
	return string(out)
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
