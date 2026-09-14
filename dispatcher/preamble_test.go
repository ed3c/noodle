package dispatcher

import (
	"strings"
	"testing"
)

func TestBuildSessionPreambleForIsolatedCook(t *testing.T) {
	preamble := buildSessionPreamble(DispatchRequest{
		WorktreePath: "/tmp/project-worktree",
		Skill:        "execute",
	})
	for _, expected := range []string{
		"working_directory: /tmp/project-worktree",
		"checkout_mode: isolated-worktree",
		"selected_skill: execute",
	} {
		if !strings.Contains(preamble, expected) {
			t.Fatalf("isolated preamble missing %q: %q", expected, preamble)
		}
	}
	for _, forbidden := range []string{"todos.md", "brain/", "conventional commit", "run verification"} {
		if strings.Contains(strings.ToLower(preamble), forbidden) {
			t.Fatalf("isolated preamble contains project policy %q: %q", forbidden, preamble)
		}
	}
}

func TestBuildSessionPreambleForPrimaryCheckoutContainsOnlyRuntimeFacts(t *testing.T) {
	preamble := buildSessionPreamble(DispatchRequest{
		AllowPrimaryCheckout: true,
		WorktreePath:         "/tmp/project",
		Skill:                "schedule",
	})
	for _, expected := range []string{
		"working_directory: /tmp/project",
		"checkout_mode: primary-checkout",
		"selected_skill: schedule",
	} {
		if !strings.Contains(preamble, expected) {
			t.Fatalf("primary preamble missing %q: %q", expected, preamble)
		}
	}
	for _, forbidden := range []string{"todos.md", "brain/", "isolated checkout", "assigned worktree", "conventional commit", "run verification"} {
		if strings.Contains(strings.ToLower(preamble), forbidden) {
			t.Fatalf("primary preamble contains project policy %q: %q", forbidden, preamble)
		}
	}
}
