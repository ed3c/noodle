package dispatcher

import (
	"fmt"
	"strings"
)

func buildSessionPreamble(req DispatchRequest) string {
	if req.Runtime == "openshell" {
		return fmt.Sprintf(`# Noodle Runtime Context

This session runs in a Linux OpenShell VM. Work in /sandbox/repo, an exported
snapshot of the original isolated worker checkout. Git metadata and private
environment files are intentionally absent. Do not initialize Git or commit here.
Make the requested file changes and run the selected Linux checks. Noodle retains
native session evidence, stops the original VM, validates the returned files,
and creates a candidate commit for the existing review process.

- selected_skill: %s

Project workflow policy comes from the selected skill or explicit system prompt.`, strings.TrimSpace(req.Skill))
	}

	checkoutMode := "isolated-worktree"
	if req.AllowPrimaryCheckout {
		checkoutMode = "primary-checkout"
	}
	return fmt.Sprintf(`# Noodle Runtime Context

This session is managed by Noodle. The following are runtime facts, not project workflow policy:

- working_directory: %s
- checkout_mode: %s
- selected_skill: %s

Project workflow policy comes from the selected skill or explicit system prompt.`, strings.TrimSpace(req.WorktreePath), checkoutMode, strings.TrimSpace(req.Skill))
}
