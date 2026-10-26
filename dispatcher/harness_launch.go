package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/stringx"
)

// Only the native harnesses implemented by these dispatchers are admitted.
// Carrier-specific argv and input protocols are selected after this boundary.
type harnessKind string

const (
	harnessClaude harnessKind = "claude"
	harnessCodex  harnessKind = "codex"
)

func selectHarness(provider string) (harnessKind, error) {
	switch stringx.Normalize(provider) {
	case string(harnessClaude):
		return harnessClaude, nil
	case string(harnessCodex):
		return harnessCodex, nil
	default:
		return "", fmt.Errorf("harness %q not supported", provider)
	}
}

type processInput uint8

const (
	inputClaudeStreamJSON processInput = iota + 1
	inputPromptEOF
)

// processLaunch keeps command arguments and their stdin protocol together.
// Sprites uses its own pipe-mode argv and never consumes this plan.
type processLaunch struct {
	binary string
	args   []string
	input  processInput
}

func (d *ProcessDispatcher) planProcessLaunch(harness harnessKind, req DispatchRequest, systemPrompt string) processLaunch {
	var plan processLaunch
	var config ProviderConfig
	switch harness {
	case harnessCodex:
		config = d.providerConfigs.Codex
		plan = processLaunch{binary: "codex", args: codexBaseArgs(req), input: inputPromptEOF}
		if req.codexThreadID != "" {
			// Carrier flags belong to exec, not exec resume.
			plan.args = append(append([]string{"exec"}, config.Args...), plan.args[1:]...)
			plan.args = append(plan.args, req.codexThreadID, "-")
		} else {
			plan.args = append(plan.args, config.Args...)
		}
	case harnessClaude:
		config = d.providerConfigs.Claude
		plan = processLaunch{binary: "claude", input: inputClaudeStreamJSON}
		plan.args = replaceFlag(claudeBaseArgs(req, systemPrompt), "-p", "--input-format", "stream-json")
		plan.args = append(plan.args, config.Args...)
	}
	if path := strings.TrimSpace(config.Path); path != "" {
		resolved, _ := d.execution.ResolvePath(path)
		candidate := filepath.Join(resolved, plan.binary)
		if _, err := os.Stat(candidate); err == nil {
			plan.binary = candidate
		}
	}
	return plan
}
