package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/skill"
)

func TestRequiredSkillRefusesBeforeProcessLaunch(t *testing.T) {
	selected := t.TempDir()
	method := filepath.Join(selected, "poteto-mode")
	if err := os.MkdirAll(method, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte("# Verified entry")
	if err := os.WriteFile(filepath.Join(method, "SKILL.md"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(source)
	pin := hex.EncodeToString(digest[:])
	treePin, err := skill.TreeSHA256(method)
	if err != nil { t.Fatal(err) }
	root := filepath.Join(t.TempDir(), ".noodle")
	d := NewProcessDispatcher(ProcessDispatcherConfig{
		ProjectDir: t.TempDir(), RuntimeDir: root, RuntimeKind: "process",
		RuntimeDefault: "cat >/dev/null",
		SkillResolver: skill.Resolver{SearchPaths: []string{selected}},
	})
	base := DispatchRequest{
		Name: "bounded-issue", Prompt: "synthetic bounded issue",
		Provider: "codex", Model: "test-model",
		WorktreePath: t.TempDir(), AllowPrimaryCheckout: true,
		Skill: "poteto-mode", RequiredSkillSHA256: pin,
		RequiredSkillTreeSHA256: treePin,
	}
	cases := []struct {
		name string
		mutate func(*DispatchRequest)
	}{
		{"wrong_pin", func(req *DispatchRequest) { req.RequiredSkillSHA256 = strings.Repeat("0", 64) }},
		{"wrong_tree_pin", func(req *DispatchRequest) { req.RequiredSkillTreeSHA256 = strings.Repeat("0", 64) }},
		{"missing_tree_pin", func(req *DispatchRequest) { req.RequiredSkillTreeSHA256 = "" }},
		{"tree_without_raw_pin", func(req *DispatchRequest) { req.RequiredSkillSHA256 = "" }},
		{"unversioned_pin", func(req *DispatchRequest) { req.RequiredSkillSHA256 = "main" }},
		{"wrong_skill", func(req *DispatchRequest) { req.Skill = "builder-bug-factory" }},
		{"missing_skill_name", func(req *DispatchRequest) { req.Skill = "" }},
		{"override", func(req *DispatchRequest) { req.SystemPrompt = "# Candidate override" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			tc.mutate(&request)
			_, err := d.Dispatch(context.Background(), request)
			if err == nil {
				t.Fatalf("%s: required method gate did not refuse", tc.name)
			}
			// The request passed Noodle's OS worktree admission only because
			// AllowPrimaryCheckout was explicitly set in this negative fixture.
			// The worker must NEVER be launched or receive process metadata.
			entries, readErr := os.ReadDir(filepath.Join(root, "sessions"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if _, statErr := os.Stat(filepath.Join(root, "sessions", entry.Name(), "process.json")); statErr == nil {
					t.Fatalf("%s: rejected method launched an OS process", tc.name)
				}
			}
		})
	}
	loaded, err := resolveSkillBundle(d.skillResolver, base)
	if err != nil || loaded.ResolvedPath != method || loaded.EntrySHA256 != pin {
		t.Fatalf("strict selected method unexpectedly rejected: %+v %v", loaded, err)
	}
}

func TestRequiredSkillRejectsShadowedProvidersAndTruncatedRefs(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	name := "poteto-mode"
	for _, source := range []string{first, second} {
		dir := filepath.Join(source, name)
		if err := os.MkdirAll(dir, 0o755); err != nil { t.Fatal(err) }
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("identical entry"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := sha256.Sum256([]byte("identical entry"))
	firstTree, err := skill.TreeSHA256(filepath.Join(first, name))
	if err != nil { t.Fatal(err) }
	req := DispatchRequest{Skill: name, Provider: "codex",
		RequiredSkillSHA256: hex.EncodeToString(h[:]),
		RequiredSkillTreeSHA256: firstTree}
	// Same content digest must not legitimize an ambiguous first-match provider.
	_, err = resolveSkillBundle(skill.Resolver{SearchPaths: []string{first, second}}, req)
	if err == nil || !strings.Contains(err.Error(), "shadowed") {
		t.Fatalf("same-named providers were accepted: %v", err)
	}
	req.Skill = " poteto-mode "
	_, err = resolveSkillBundle(skill.Resolver{SearchPaths: []string{first, second}}, req)
	if err == nil || !strings.Contains(err.Error(), "shadowed") {
		t.Fatalf("padded same-named providers were accepted: %v", err)
	}
	selected, err := resolveSkillBundle(skill.Resolver{SearchPaths: []string{first}}, req)
	if err != nil || selected.ResolvedPath != filepath.Join(first, name) {
		t.Fatalf("padded unambiguous Skill unexpectedly rejected: %+v %v", selected, err)
	}
	// Existing no-pin tasks retain the original first-match winner.
	req.RequiredSkillSHA256 = ""
	req.RequiredSkillTreeSHA256 = ""
	legacy, err := resolveSkillBundle(skill.Resolver{SearchPaths: []string{first, second}}, req)
	if err != nil || legacy.ResolvedPath != filepath.Join(first, name) {
		t.Fatalf("legacy first-match behavior changed: %+v %v", legacy, err)
	}
	req.Skill = name
	req.RequiredSkillSHA256 = hex.EncodeToString(h[:])
	req.RequiredSkillTreeSHA256 = firstTree
	dir := filepath.Join(first, name, "references")
	if err := os.MkdirAll(dir, 0o755); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(dir, "oversized.md"), []byte(strings.Repeat("x", codexSkillRefsLimitBytes+32)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Isolate incomplete-reference refusal from the distinct tree-digest gate.
	req.RequiredSkillTreeSHA256, err = skill.TreeSHA256(filepath.Join(first, name))
	if err != nil { t.Fatal(err) }
	_, err = resolveSkillBundle(skill.Resolver{SearchPaths: []string{first}}, req)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("truncated method was accepted in strict mode: %v", err)
	}
}
