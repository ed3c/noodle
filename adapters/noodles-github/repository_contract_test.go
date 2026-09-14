package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/skill"
)

func TestNoodlesGitHubRepositoryContract(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	prepareAgentHome(t)
	policy, err := loadStrictJSON[Policy](filepath.Join(root, "policy", "github.json"))
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := loadStrictJSON[Capabilities](filepath.Join(root, "policy", "repo-capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCapabilities(capabilities); err != nil {
		t.Fatal(err)
	}
	if err := validatePolicy(policy); err != nil {
		t.Fatal(err)
	}

	cfg, diagnostics, err := config.Load(filepath.Join(root, ".noodle.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.Fatals()) != 0 {
		t.Fatalf("fatal config diagnostics = %#v", diagnostics.Fatals())
	}
	if cfg.Routing.Defaults.Provider != "codex" || cfg.Routing.Defaults.Model != "gpt-5.6-sol" {
		t.Fatalf("default target route = %#v, want codex/gpt-5.6-sol", cfg.Routing.Defaults)
	}
	if !reflect.DeepEqual(cfg.Agents.Codex.Args, []string{"--ignore-user-config"}) {
		t.Fatalf("codex args = %#v, want --ignore-user-config", cfg.Agents.Codex.Args)
	}
	if !cfg.Agents.Codex.RequireTypedOutcome {
		t.Fatal("agents.codex.require_typed_outcome = false, want true")
	}
	backlog := cfg.Adapters["backlog"]
	wantScripts := map[string]string{
		"sync": "go run ./adapters/noodles-github sync",
		"add":  "go run ./adapters/noodles-github refuse",
		"done": "go run ./adapters/noodles-github done",
		"edit": "go run ./adapters/noodles-github refuse",
	}
	if backlog.Skill != "noodles-issue-execute" || len(backlog.Scripts) != len(wantScripts) {
		t.Fatalf("backlog route = %#v", backlog)
	}
	for action, want := range wantScripts {
		if backlog.Scripts[action] != want {
			t.Fatalf("backlog %s route = %q, want %q", action, backlog.Scripts[action], want)
		}
	}
	for _, skillName := range []string{"schedule", "execute", "noodles-issue-execute"} {
		path := filepath.Join(root, ".agents", "skills", skillName, "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("target-owned %s route is missing: %v", skillName, err)
		}
	}
}

func TestNoodlesGitHubOperationalSkillsEncodeSingleStagePolicy(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	required := map[string][]string{
		"schedule": {"exactly one stage", "execution_skill", "entire row", ".noodle/orders.json", "owned_ids", "`id` is not in `owned_ids`"},
		"noodles-issue-execute": {
			"source-authorized", "declared write boundary", "handoff", "awaiting_land", "stage_yield",
			"stage_message", "noodle_session_id", "noodle_order_id", "noodle_stage_index",
			`"outcome":"completed"`, `"outcome":"blocked"`, `"outcome":"failed"`,
		},
	}
	for _, name := range []string{"schedule", "noodles-issue-execute"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, ".agents", "skills", name, "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			frontmatter, body, err := skill.ParseFrontmatter(data)
			if err != nil {
				t.Fatal(err)
			}
			if frontmatter.Name != name || !frontmatter.IsTaskType() {
				t.Fatalf("frontmatter = %#v", frontmatter)
			}
			operational := strings.ToLower(string(body))
			for _, forbidden := range []string{"brain/todos.md", "quality", "reflect", "noodle worktree merge"} {
				if strings.Contains(operational, forbidden) {
					t.Fatalf("%s operational policy contains forbidden route %q", name, forbidden)
				}
			}
			for _, signal := range required[name] {
				if !strings.Contains(operational, signal) {
					t.Fatalf("%s operational policy is missing %q", name, signal)
				}
			}
		})
	}
}

func prepareAgentHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, name := range []string{".claude", ".codex"} {
		if err := os.Mkdir(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
}

func TestNoodlesDispatchWorkflowHasNoExecutionAuthority(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "noodles-dispatch.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, required := range []string{
		"repository_dispatch:", "types: [noodles-execution]", "contents: read", "issues: write",
		"go run ./adapters/noodles-github receive", "ref: ${{ github.sha }}",
		"persist-credentials: false", "cancel-in-progress: false", "timeout-minutes: 5",
		"actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683",
		"actions/setup-go@d35c59abb061a4a6fb18e82ac0862c26744d6ab5",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("workflow lacks %q", required)
		}
	}
	for _, forbidden := range []string{"pull-requests: write", "contents: write", "git worktree", "git branch", "codex", "claude", "/dispatches"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("workflow carries forbidden execution authority %q", forbidden)
		}
	}
}
