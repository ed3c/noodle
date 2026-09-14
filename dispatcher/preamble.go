package dispatcher

import (
	"fmt"
	"strings"
)

func buildSessionPreamble(req DispatchRequest) string {
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
