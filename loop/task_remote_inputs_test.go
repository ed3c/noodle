package loop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/dispatcher"
)

func remoteInputFixture(t *testing.T, provider string) (*Loop, dispatchCandidate, *readbackRuntime) {
	t.Helper()
	l, cook, _, rt := openShellLocalTaskFixture(t, provider)
	ref := dispatcher.InstructionBundleRef{Path: filepath.Join(l.projectDir, "frozen-manifest.json"), SHA256: strings.Repeat("a", 64)}
	var prompt map[string]any
	if err := json.Unmarshal([]byte(cook.stage.Prompt), &prompt); err != nil {
		t.Fatal(err)
	}
	prompt["instruction_context"] = map[string]any{"materialization": ref}
	prompt["model_reasoning_effort"] = "high"
	raw, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	cook.stage.Prompt = string(raw)
	order := l.canonical.Orders[cook.orderID]
	order.Stages[0].Prompt = string(raw)
	l.canonical.Orders[cook.orderID] = order
	return l, dispatchCandidate{OrderID: cook.orderID, StageIndex: 0, Stage: cook.stage}, rt
}

func TestRemoteTaskInputsReachSelectedHarness(t *testing.T) {
	for _, provider := range []string{"codex", "pi"} {
		t.Run(provider, func(t *testing.T) {
			l, cand, rt := remoteInputFixture(t, provider)
			if err := l.spawnCookAdmitted(context.Background(), cand, Order{ID: cand.OrderID}, spawnOptions{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, session := range rt.sessions {
					session.complete("cancelled")
				}
				l.watcherWG.Wait()
			})
			if len(rt.mockRuntime.calls) != 1 {
				t.Fatalf("runtime dispatch calls = %d", len(rt.mockRuntime.calls))
			}
			req := rt.mockRuntime.calls[0]
			if req.InstructionBundle == nil || req.InstructionBundle.SHA256 != strings.Repeat("a", 64) || req.ReasoningLevel != "high" || req.Provider != provider || req.EnvVars["NOODLE_ORDER_ID"] != cand.OrderID {
				t.Fatal("selected frozen inputs or effort lost at runtime dispatch")
			}
		})
	}
}

func TestRemoteTaskInputsRefuseBeforeDispatch(t *testing.T) {
	for _, kind := range []string{"missing-ref", "relative-ref", "bad-hash", "foreign-order", "changed-prompt", "changed-provider", "whitespace-effort"} {
		t.Run(kind, func(t *testing.T) {
			l, cand, rt := remoteInputFixture(t, "codex")
			var prompt map[string]any
			if err := json.Unmarshal([]byte(cand.Stage.Prompt), &prompt); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing-ref":
				delete(prompt, "instruction_context")
			case "relative-ref":
				prompt["instruction_context"] = map[string]any{"materialization": dispatcher.InstructionBundleRef{Path: "relative.json", SHA256: strings.Repeat("a", 64)}}
			case "bad-hash":
				prompt["instruction_context"] = map[string]any{"materialization": dispatcher.InstructionBundleRef{Path: "/frozen.json", SHA256: "bad"}}
			case "whitespace-effort":
				prompt["model_reasoning_effort"] = " high "
			case "foreign-order":
				l.config.Runtime.AdmittedOrders = []string{"other"}
			case "changed-provider":
				cand.Stage.Provider = "pi"
			}
			raw, err := json.Marshal(prompt)
			if err != nil {
				t.Fatal(err)
			}
			cand.Stage.Prompt = string(raw)
			if kind != "changed-prompt" {
				order := l.canonical.Orders[cand.OrderID]
				order.Stages[0].Prompt = cand.Stage.Prompt
				l.canonical.Orders[cand.OrderID] = order
			} else {
				cand.Stage.Prompt += " "
			}
			if err := l.spawnCookAdmitted(context.Background(), cand, Order{ID: cand.OrderID}, spawnOptions{}); err == nil {
				t.Fatal("invalid remote task inputs admitted")
			}
			if len(rt.mockRuntime.calls) != 0 || len(l.deps.Worktree.(*fakeWorktree).created) != 0 {
				t.Fatal("input refusal created a worktree or dispatched")
			}
		})
	}
}

func TestRemoteTaskInputsLeaveProcessEffortToItsExistingOwner(t *testing.T) {
	l, cand, _ := remoteInputFixture(t, "codex")
	cand.Stage.Runtime = "process"
	if ref, effort, err := l.remoteTaskInputs(cand); err != nil || ref != nil || effort != "" {
		t.Fatal("process route acquired a second input or reasoning owner")
	}
}
