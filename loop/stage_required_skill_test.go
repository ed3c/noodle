package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/poteto/noodle/config"
	loopruntime "github.com/poteto/noodle/runtime"
)

func stageExtraPins(rawSHA, treeSHA string) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		stageRequiredSkillSHA256:     json.RawMessage(`"`+rawSHA+`"`),
		stageRequiredSkillTreeSHA256: json.RawMessage(`"`+treeSHA+`"`),
		"unrelated_existing_extra":  json.RawMessage(`{"priority":4}`),
	}
}

// Exercise original orders.json -> Cycle -> cook_spawn -> Runtime.Dispatch,
// rather than only a direct dispatcher helper.
func TestStagePinsReachOriginalNoodleDispatchRequest(t *testing.T) {
	const rawPin = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const treePin = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	stage := Stage{
		TaskKey: "execute", Skill: "execute", Provider: "codex",
		Model: "test-model", Status: StageStatusPending,
		Extra: stageExtraPins(rawPin, treePin),
	}
	orders := OrdersFile{Orders: []Order{{
		ID: "bounded-factory-work-order", Title: "frozen owner-selected task",
		Status: OrderStatusActive, Stages: []Stage{stage},
	}}}
	env := newIntegrationEnv(t, orders, func(c *integrationCfg) {
		c.cfg.Mode = "auto"
	})
	seedCanonicalFromOrders(env.loop, orders)
	if err := env.loop.Cycle(context.Background()); err != nil {
		t.Fatalf("original Noodle Cycle: %v", err)
	}
	if len(env.rt.calls) != 1 {
		t.Fatalf("original runtime received %d dispatches, want one", len(env.rt.calls))
	}
	got := env.rt.calls[0]
	if got.Skill != "execute" || got.RequiredSkillSHA256 != rawPin ||
		got.RequiredSkillTreeSHA256 != treePin {
		t.Fatalf("original stage pins were dropped or changed: %+v", got)
	}
	if got.WorktreePath == "" {
		t.Fatalf("original worktree identity lost: %+v", got)
	}
	// Original order still retains the extra fields for resume/owner readback.
	frozen := env.readOrders(t).Orders[0].Stages[0]
	if string(frozen.Extra[stageRequiredSkillSHA256]) != `"`+rawPin+`"` {
		t.Fatal("original pinned Stage was lost during lifecycle persistence")
	}
}

func TestStagePinDecoderRefusesMissingBadOrHalfPair(t *testing.T) {
	const rawPin = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const treePin = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	valid := Stage{Skill: "execute", Extra: stageExtraPins(rawPin, treePin)}
	gotRaw, gotTree, err := stageRequiredSkillPins(valid)
	if err != nil || gotRaw != rawPin || gotTree != treePin {
		t.Fatalf("real Stage pin extraction changed: %q %q %v", gotRaw, gotTree, err)
	}
	legacy := Stage{Skill: "execute", Extra: map[string]json.RawMessage{
		"other_user_field": json.RawMessage("123"),
	}}
	entry, tree, err := stageRequiredSkillPins(legacy)
	if err != nil || entry != "" || tree != "" {
		t.Fatalf("nonselected historical order was accidentally pinned: %q %q %v", entry, tree, err)
	}
	cases := []struct {
		name string
		mod func(*Stage)
	}{
		{"missing_raw", func(s *Stage) { delete(s.Extra, stageRequiredSkillSHA256) }},
		{"missing_tree", func(s *Stage) { delete(s.Extra, stageRequiredSkillTreeSHA256) }},
		{"bad_raw_sha", func(s *Stage) { s.Extra[stageRequiredSkillSHA256] = json.RawMessage(`"main"`) }},
		{"bad_tree_sha", func(s *Stage) { s.Extra[stageRequiredSkillTreeSHA256] = json.RawMessage(`"HEAD"`) }},
		{"non_string", func(s *Stage) { s.Extra[stageRequiredSkillSHA256] = json.RawMessage(`null`) }},
		{"malformed_json", func(s *Stage) { s.Extra[stageRequiredSkillSHA256] = json.RawMessage("{") }},
		{"uppercase", func(s *Stage) { s.Extra[stageRequiredSkillSHA256] = json.RawMessage(`"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`) }},
		{"empty_skill", func(s *Stage) { s.Skill = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := valid
			s.Extra = make(map[string]json.RawMessage, len(valid.Extra))
			for k, v := range valid.Extra { s.Extra[k] = append([]byte{}, v...) }
			c.mod(&s)
			_, _, err := stageRequiredSkillPins(s)
			if err == nil {
				t.Fatal("untrusted or incomplete stage pin was accepted")
			}
		})
	}
}

func TestBadStagePinStopsBeforeWorktreeOrRuntime(t *testing.T) {
	orders := OrdersFile{Orders: []Order{{
		ID: "invalid-factory-stage", Status: OrderStatusActive,
		Stages: []Stage{{
			TaskKey: "execute", Skill: "execute", Provider: "codex",
			Model: "test-model", Status: StageStatusPending,
			Extra: map[string]json.RawMessage{
				stageRequiredSkillSHA256: json.RawMessage(`"main"`),
				stageRequiredSkillTreeSHA256: json.RawMessage(`"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`),
			},
		}},
	}}}
	env := newIntegrationEnv(t, orders, func(c *integrationCfg) { c.cfg.Mode = "auto" })
	seedCanonicalFromOrders(env.loop, orders)
	err := env.loop.Cycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "required Skill selection") {
		t.Fatalf("bad Stage was not refused at dispatch boundary: %v", err)
	}
	if len(env.rt.calls) != 0 || len(env.wt.created) != 0 {
		t.Fatalf("bad Stage got process/worktree effects: calls=%d worktrees=%d",
			len(env.rt.calls), len(env.wt.created))
	}
}

func TestRequiredSkillPinDoesNotFallThroughToUnverifiedRuntime(t *testing.T) {
	process := newMockRuntime()
	sprites := newMockRuntime()
	l := New(t.TempDir(), "noodle", config.DefaultConfig(), Dependencies{
		Runtimes: map[string]loopruntime.Runtime{
			"process": process,
			"sprites": sprites,
		},
		Worktree: &fakeWorktree{},
		Adapter: &fakeAdapterRunner{},
		Mise: &fakeMise{},
		Monitor: fakeMonitor{},
		Registry: testLoopRegistry(),
	})
	strict := loopruntime.DispatchRequest{
		Name: "bounded", Prompt: "synthetic", Runtime: "sprites",
		Skill: "execute",
		RequiredSkillSHA256: strings.Repeat("a", 64),
		RequiredSkillTreeSHA256: strings.Repeat("b", 64),
	}
	_, _, err := l.dispatchSession(context.Background(), strict)
	if err == nil || !strings.Contains(err.Error(), "required Skill pins need verified process runtime") {
		t.Fatalf("unverified Sprite carrier bypassed original strict method: %v", err)
	}
	if len(sprites.calls) != 0 || len(process.calls) != 0 {
		t.Fatalf("wrong-runtime Stage dispatched a process: sprites=%d process=%d",
			len(sprites.calls), len(process.calls))
	}
	strict.RequiredSkillSHA256 = ""
	strict.RequiredSkillTreeSHA256 = ""
	_, _, err = l.dispatchSession(context.Background(), strict)
	if err != nil || len(sprites.calls) != 1 {
		t.Fatalf("legacy unpinned Sprite task route regressed: %v %+v", err, sprites.calls)
	}
}
