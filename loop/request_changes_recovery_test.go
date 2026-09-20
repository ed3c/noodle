package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func TestRequestChangesRestartEditRequeue(t *testing.T) {
	l, _, cook := newTypedOutcomeTestLoop(t)
	runGitInRepo(t, l.projectDir, "init", "-b", "main")
	runGitInRepo(t, l.projectDir, "config", "user.email", "test@noodle.dev")
	runGitInRepo(t, l.projectDir, "config", "user.name", "Noodle Test")
	runGitInRepo(t, l.projectDir, "commit", "--allow-empty", "-m", "base")
	runGitInRepo(t, l.projectDir, "worktree", "add", "-b", cook.worktreeName, cook.worktreePath)
	runGitInRepo(t, cook.worktreePath, "commit", "--allow-empty", "-m", "candidate")
	appendTypedOutcome(t, l, cook, event.StageOutcomeBlocked, true, cook.orderID, cook.stageIndex)
	dir := filepath.Join(l.runtimeDir, "sessions", cook.session.ID())
	for name, data := range map[string]string{
		"spawn.json": `{"session_id":"session-1","worktree_path":"` + cook.worktreePath + `","retry_count":0}`,
		"prompt.txt": "original prompt", "process.json": `{"session_id":"session-1","pid":99999999}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.parkPendingReview(cook, "typed blocked"); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequestChanges(cook.orderID, "correction"); err != nil {
		t.Fatal(err)
	}
	restarted := New(l.projectDir, "noodle", l.config, l.deps)
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.controlEditItem(ControlCommand{OrderID: cook.orderID, Prompt: "fresh correction"}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := restarted.cooks.activeCooksByOrder[cook.orderID]
	if next == nil || next.session.ID() == cook.session.ID() || next.attempt != 1 || next.worktreePath != cook.worktreePath {
		t.Fatalf("wrong resumed attempt: %#v", next)
	}
	if !strings.Contains(next.stage.Prompt, "fresh correction") {
		t.Fatal("stale prompt")
	}
}

// Use real Git custody and disk-backed canonical/legacy evidence. The runtime
// spy observes dispatch without starting a provider process.
func newRequestChangesRecovery(t *testing.T) (*Loop, *cookHandle, requestChangesPacket) {
	t.Helper()
	l, _, cook := newTypedOutcomeTestLoop(t)
	runGitInRepo(t, l.projectDir, "init", "-b", "main")
	runGitInRepo(t, l.projectDir, "config", "user.email", "test@noodle.dev")
	runGitInRepo(t, l.projectDir, "config", "user.name", "Noodle Test")
	runGitInRepo(t, l.projectDir, "commit", "--allow-empty", "-m", "base")
	runGitInRepo(t, l.projectDir, "worktree", "add", "-b", cook.worktreeName, cook.worktreePath)
	runGitInRepo(t, cook.worktreePath, "commit", "--allow-empty", "-m", "candidate")
	appendTypedOutcome(t, l, cook, event.StageOutcomeBlocked, true, cook.orderID, cook.stageIndex)
	dir := filepath.Join(l.runtimeDir, "sessions", cook.session.ID())
	recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"session_id": cook.session.ID(), "worktree_path": cook.worktreePath, "retry_count": 0})
	recoveryWriteJSON(t, filepath.Join(dir, "process.json"), map[string]any{"session_id": cook.session.ID(), "pid": 99999999})
	requestChangesWrite(t, filepath.Join(dir, "prompt.txt"), []byte("original terminal prompt"))
	if err := l.parkPendingReview(cook, "typed blocked"); err != nil {
		t.Fatal(err)
	}
	if err := l.recordInitialAdmissions([]string{cook.orderID}); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequestChanges(cook.orderID, "correction"); err != nil {
		t.Fatal(err)
	}
	p := requestChangesPacket{Order: l.canonical.Orders[cook.orderID], Review: l.canonical.PendingReviews[cook.orderID]}
	var err error
	p.Binding, err = recoveryBinding(p.Order.Stages[cook.stageIndex])
	if err != nil {
		t.Fatal(err)
	}
	p.LoopEventsSHA256 = recoveryDigest(recoveryRead(t, filepath.Join(l.runtimeDir, "loop-events.ndjson")))
	for _, r := range l.effectLedger.All() {
		if r.EffectID == initialAdmissionEffectID(cook.orderID) {
			p.Admission = r
		}
	}
	return l, cook, p
}

func recoveryRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func requestChangesWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func recoveryWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	requestChangesWrite(t, path, data)
}

// This reproduces the old startup archive: all failed non-schedule orders are
// removed through the normal state writer, which removes canonical reviews too.
func archiveRequestChangesLikeOldStartup(t *testing.T, l *Loop) *Loop {
	t.Helper()
	if err := l.mutateOrdersState(func(orders *OrdersFile) (bool, error) {
		kept := orders.Orders[:0]
		for _, o := range orders.Orders {
			if o.Status != OrderStatusFailed || isScheduleOrder(o) {
				kept = append(kept, o)
			}
		}
		orders.Orders = kept
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	restarted := New(l.projectDir, "noodle", l.config, l.deps)
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return restarted
}

func recoveryCommand(t *testing.T, l *Loop, p requestChangesPacket) ControlCommand {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery.json")
	recoveryWriteJSON(t, path, p)
	return ControlCommand{ID: "recovery-once", Action: "recover-request-changes", OrderID: p.Order.OrderID, Value: path, Target: recoveryDigest(recoveryRead(t, path))}
}

func recoveryControl(t *testing.T, l *Loop, cmd ControlCommand) ControlAck {
	t.Helper()
	controlPath, ackPath, _ := l.controlPaths()
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	requestChangesWrite(t, controlPath, append(data, '\n'))
	if err := l.processControlCommands(); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(recoveryRead(t, ackPath)), []byte{'\n'})
	var ack ControlAck
	if err := json.Unmarshal(lines[len(lines)-1], &ack); err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestRequestChangesOldArchiveRestartRequeue(t *testing.T) {
	l, cook, p := newRequestChangesRecovery(t)
	before := recoverySessionBytes(t, l, cook.session.ID())
	l = archiveRequestChangesLikeOldStartup(t, l)
	cmd := recoveryCommand(t, l, p)
	if ack := recoveryControl(t, l, cmd); ack.Status != "ok" {
		t.Fatalf("recover: %+v", ack)
	}
	if l.canonical.Orders[cook.orderID].Status != state.OrderFailed || len(l.cooks.pendingReview) != 1 || len(l.deps.Runtimes["process"].(*mockRuntime).calls) != 0 {
		t.Fatal("migration dispatched or did not restore failed review")
	}
	restored := l.canonical.Orders[cook.orderID]
	if !recoveryJSONEqual(restored, p.Order) {
		t.Fatalf("order definition changed: %#v", restored)
	}
	// A fresh command cannot consume the same packet a second time.
	cmd.ID = "recovery-twice"
	if ack := recoveryControl(t, l, cmd); ack.Status != "error" {
		t.Fatal("second migration accepted")
	}
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.controlEditItem(ControlCommand{OrderID: cook.orderID, Prompt: "fresh operator correction"}); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if err := l.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := l.cooks.activeCooksByOrder[cook.orderID]
	if next == nil || next.session.ID() == cook.session.ID() || next.attempt != 1 || next.orderID != cook.orderID || next.stageIndex != cook.stageIndex || next.worktreePath != cook.worktreePath {
		t.Fatalf("wrong resumed attempt: %#v", next)
	}
	rt := l.deps.Runtimes["process"].(*mockRuntime)
	found := false
	for _, call := range rt.calls {
		if call.WorktreePath == cook.worktreePath {
			found = strings.Contains(call.Prompt, "fresh operator correction") && call.RetryCount == 1
		}
	}
	if !found {
		t.Fatal("dispatcher did not receive edited prompt and new attempt")
	}
	if !reflect.DeepEqual(before, recoverySessionBytes(t, l, cook.session.ID())) {
		t.Fatal("prior terminal session bytes changed")
	}
	if got := gitOutputInRepo(t, cook.worktreePath, "rev-parse", "HEAD"); got != p.Binding.Head {
		t.Fatal("candidate HEAD changed")
	}
	if len(l.canonical.Orders[cook.orderID].Stages[0].Attempts) != 2 {
		t.Fatal("attempt history lost")
	}
}

func recoverySessionBytes(t *testing.T, l *Loop, id string) map[string]string {
	t.Helper()
	result := map[string]string{}
	dir := filepath.Join(l.runtimeDir, "sessions", id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			result[e.Name()] = string(recoveryRead(t, filepath.Join(dir, e.Name())))
		}
	}
	return result
}

// Capture state bytes immediately before a refused command, including planted
// evidence changes. Refusal may append its ack, but cannot alter any owner data.
func TestRequestChangesPacketRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *Loop, *cookHandle, *requestChangesPacket, *ControlCommand)
	}{
		{"missing digest", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			cmd.Target = ""
		}},
		{"changed digest", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			requestChangesWrite(t, cmd.Value, []byte("{}"))
		}},
		{"relative packet", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			cmd.Value = "packet.json"
		}},
		{"wrong order", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			p.Order.OrderID = "other"
		}},
		{"wrong stage", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			p.Review.StageIndex = 2
		}},
		{"wrong session", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			p.Binding.SessionID = "other"
		}},
		{"wrong attempt", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			p.Binding.Attempt = 1
		}},
		{"wrong attempt ID", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			p.Binding.AttemptID = "other"
		}},
		{"wrong full definition", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			p.Order.Stages[0].Skill = "other"
		}},
		{"wrong admission", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			p.Admission.Attempts++
		}},
		{"undone admission", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			p.Admission.Status = reducer.EffectLedgerPending
		}},
		{"missing worktree", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			if err := os.RemoveAll(c.worktreePath); err != nil {
				t.Fatal(err)
			}
		}},
		{"dirty worktree", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			requestChangesWrite(t, filepath.Join(c.worktreePath, "dirty"), []byte("dirty"))
		}},
		{"moved branch", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			runGitInRepo(t, c.worktreePath, "branch", "-m", "moved")
		}},
		{"moved HEAD", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			runGitInRepo(t, c.worktreePath, "commit", "--allow-empty", "-m", "moved")
		}},
		{"conflicting order", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			l.canonical.Orders[c.orderID] = p.Order
		}},
		{"conflicting review", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			l.canonical.PendingReviews[c.orderID] = p.Review
		}},
		{"conflicting legacy order", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			l.orders.Orders = append(l.orders.Orders, Order{ID: c.orderID})
		}},
		{"live PID", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "process.json")
			recoveryWriteJSON(t, path, map[string]any{"session_id": c.session.ID(), "pid": os.Getpid()})
			p.Binding.Files["process.json"] = recoveryDigest(recoveryRead(t, path))
		}},
		{"missing blocked outcome", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "events.ndjson")
			requestChangesWrite(t, path, nil)
			p.Binding.Files["events.ndjson"] = recoveryDigest(nil)
		}},
		{"mismatched typed outcome", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "events.ndjson")
			data := bytes.ReplaceAll(recoveryRead(t, path), []byte(c.orderID), []byte("wrong-order"))
			requestChangesWrite(t, path, data)
			p.Binding.Files["events.ndjson"] = recoveryDigest(data)
		}},
		{"mismatched process identity", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "process.json")
			recoveryWriteJSON(t, path, map[string]any{"session_id": "wrong", "pid": 99999999})
			p.Binding.Files["process.json"] = recoveryDigest(recoveryRead(t, path))
		}},
		{"mismatched spawn identity", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "spawn.json")
			recoveryWriteJSON(t, path, map[string]any{"session_id": c.session.ID(), "worktree_path": c.worktreePath, "retry_count": 2})
			p.Binding.Files["spawn.json"] = recoveryDigest(recoveryRead(t, path))
		}},
		{"missing request changes failure", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			path := filepath.Join(l.runtimeDir, "loop-events.ndjson")
			requestChangesWrite(t, path, nil)
			p.LoopEventsSHA256 = recoveryDigest(nil)
		}},
		{"mismatched request changes failure", func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
			path := filepath.Join(l.runtimeDir, "loop-events.ndjson")
			data := bytes.ReplaceAll(recoveryRead(t, path), []byte("request_changes"), []byte("review_rejected"))
			requestChangesWrite(t, path, data)
			p.LoopEventsSHA256 = recoveryDigest(data)
		}},
	}
	for _, name := range append(slices.Clone(recoverySessionFiles), "loop-events.ndjson") {
		for _, missing := range []bool{false, true} {
			cases = append(cases, struct {
				name   string
				change func(*testing.T, *Loop, *cookHandle, *requestChangesPacket, *ControlCommand)
			}{fmt.Sprintf("evidence %s missing=%v", name, missing), func(t *testing.T, l *Loop, c *cookHandle, p *requestChangesPacket, cmd *ControlCommand) {
				path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), name)
				if name == "loop-events.ndjson" {
					path = filepath.Join(l.runtimeDir, name)
				}
				if missing {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else {
					requestChangesWrite(t, path, append(recoveryRead(t, path), ' '))
				}
			}})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, c, p := newRequestChangesRecovery(t)
			l = archiveRequestChangesLikeOldStartup(t, l)
			cmd := recoveryCommand(t, l, p)
			priorPacket, _ := json.Marshal(p)
			tc.change(t, l, c, &p, &cmd)
			changedPacket, _ := json.Marshal(p)
			if !bytes.Equal(priorPacket, changedPacket) {
				requestChangesWrite(t, cmd.Value, changedPacket)
				cmd.Target = recoveryDigest(changedPacket)
			}
			// Missing loop events is itself a refusal input, so the byte snapshot allows absence.
			before := recoveryRefusalBytes(t, l)
			if ack := recoveryControl(t, l, cmd); ack.Status != "error" {
				t.Fatalf("unsafe recovery accepted: %+v", ack)
			}
			if !reflect.DeepEqual(before, recoveryRefusalBytes(t, l)) {
				t.Fatal("refusal mutated owner or session data")
			}
			if len(l.deps.Runtimes["process"].(*mockRuntime).calls) != 0 {
				t.Fatal("refusal dispatched")
			}
		})
	}
}

func recoveryRefusalBytes(t *testing.T, l *Loop) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(l.runtimeDir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		if strings.HasPrefix(name, "control") {
			return nil
		}
		result[path] = string(recoveryRead(t, path))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(l.canonical)
	if err != nil {
		t.Fatal(err)
	}
	result["memory"] = string(data)
	data, err = json.Marshal(l.orders)
	if err != nil {
		t.Fatal(err)
	}
	result["orders memory"] = string(data)
	return result
}

func TestReconcileFailedRequestChangesLegalTerminalArchive(t *testing.T) {
	for _, terminal := range []string{"explicit rejection", "nonrecoverable failure", "missing typed outcome", "mismatched typed outcome"} {
		t.Run(terminal, func(t *testing.T) {
			l, c, p := newRequestChangesRecovery(t)
			switch terminal {
			case "explicit rejection":
				if err := l.controlReject(c.orderID); err != nil {
					t.Fatal(err)
				}
			case "nonrecoverable failure":
				mistake := newCookMistakeEnvelope(cookRejectReasonForTask(c.stage.TaskKey), c.orderID, c.stageIndex)
				l.recordStageFailure(c, "terminal failure", OrderFailureClassOrderTerminal, &mistake)
			case "missing typed outcome":
				requestChangesWrite(t, filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "events.ndjson"), nil)
			case "mismatched typed outcome":
				path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "events.ndjson")
				requestChangesWrite(t, path, bytes.ReplaceAll(recoveryRead(t, path), []byte(c.orderID), []byte("another-order")))
			}
			l = New(l.projectDir, "noodle", l.config, l.deps)
			if err := l.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, ok := l.canonical.Orders[c.orderID]; ok {
				t.Fatal("terminal order retained")
			}
			orders, err := readOrders(l.deps.OrdersFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range orders.Orders {
				if o.ID == c.orderID {
					t.Fatal("terminal legacy order retained")
				}
			}
			if err := l.controlEditItem(ControlCommand{OrderID: c.orderID, Prompt: "unsafe"}); err == nil {
				t.Fatal("archived order editable")
			}
			if err := l.controlRequeue(c.orderID); err == nil {
				t.Fatal("archived order requeued")
			}
			// Even a newly hashed packet cannot recover a legal terminal failure.
			p.LoopEventsSHA256 = recoveryDigest(recoveryRead(t, filepath.Join(l.runtimeDir, "loop-events.ndjson")))
			if ack := recoveryControl(t, l, recoveryCommand(t, l, p)); ack.Status != "error" {
				t.Fatal("terminal failure recovered")
			}
		})
	}
}

func TestRequestChangesRestartContinuationRefusals(t *testing.T) {
	for _, condition := range []string{"missing outcome", "wrong outcome", "live process", "missing worktree", "dirty worktree", "moved HEAD", "moved branch", "missing binding", "missing review"} {
		for _, action := range []string{"edit-item", "requeue"} {
			t.Run(condition+"/"+action, func(t *testing.T) {
				l, c, _ := newRequestChangesRecovery(t)
				l = New(l.projectDir, "noodle", l.config, l.deps)
				if err := l.reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
				switch condition {
				case "missing outcome":
					requestChangesWrite(t, filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "events.ndjson"), nil)
				case "wrong outcome":
					path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "events.ndjson")
					requestChangesWrite(t, path, bytes.ReplaceAll(recoveryRead(t, path), []byte(c.orderID), []byte("wrong")))
				case "live process":
					path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "process.json")
					recoveryWriteJSON(t, path, map[string]any{"session_id": c.session.ID(), "pid": os.Getpid()})
					// Bind the live process bytes to prove the liveness check itself refuses.
					node := l.canonical.Orders[c.orderID]
					b, err := recoveryBinding(node.Stages[0])
					if err != nil {
						t.Fatal(err)
					}
					b.Files["process.json"] = recoveryDigest(recoveryRead(t, path))
					raw, err := json.Marshal(b)
					if err != nil {
						t.Fatal(err)
					}
					node.Stages[0].Extra[requestChangesKey] = raw
					l.canonical.Orders[c.orderID] = node
				case "missing worktree":
					if err := os.RemoveAll(c.worktreePath); err != nil {
						t.Fatal(err)
					}
				case "dirty worktree":
					requestChangesWrite(t, filepath.Join(c.worktreePath, "dirty"), []byte("dirty"))
				case "moved HEAD":
					runGitInRepo(t, c.worktreePath, "commit", "--allow-empty", "-m", "moved")
				case "moved branch":
					runGitInRepo(t, c.worktreePath, "branch", "-m", "moved")
				case "missing binding":
					delete(l.canonical.Orders[c.orderID].Stages[0].Extra, requestChangesKey)
				case "missing review":
					delete(l.canonical.PendingReviews, c.orderID)
					delete(l.cooks.pendingReview, c.orderID)
				}
				before := recoveryRefusalBytes(t, l)
				ack := l.applyControlCommand(ControlCommand{Action: action, OrderID: c.orderID, Prompt: "unsafe mutation"})
				if ack.Status != "error" {
					t.Fatalf("unsafe %s accepted", action)
				}
				if !reflect.DeepEqual(before, recoveryRefusalBytes(t, l)) {
					t.Fatal("refusal changed state or terminal session")
				}
				if len(l.deps.Runtimes["process"].(*mockRuntime).calls) != 0 {
					t.Fatal("refusal dispatched")
				}
			})
		}
	}
}

func TestRequestChangesRecoveryDeadRootLiveGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process group control")
	}
	cmd := exec.Command("sh", "-c", "sleep 120 >/dev/null 2>&1 & echo $!")
	// Access the platform field only on Unix while keeping this bounded test file
	// compilable on Windows, whose SysProcAttr has no Setpgid field.
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	reflect.ValueOf(cmd.SysProcAttr).Elem().FieldByName("Setpgid").SetBool(true)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p, err := os.FindProcess(child)
		if err == nil {
			_ = p.Kill()
		}
	})
	pid := cmd.Process.Pid
	if procx.IsPIDAlive(pid) {
		t.Fatal("root process remains alive")
	}
	absent, err := procx.ProcessGroupAbsent(pid)
	if err != nil || absent {
		t.Fatalf("orphan group control invalid: %v %v", absent, err)
	}
	l, c, p := newRequestChangesRecovery(t)
	l = archiveRequestChangesLikeOldStartup(t, l)
	path := filepath.Join(l.runtimeDir, "sessions", c.session.ID(), "process.json")
	recoveryWriteJSON(t, path, map[string]any{"session_id": c.session.ID(), "pid": pid})
	p.Binding.Files["process.json"] = recoveryDigest(recoveryRead(t, path))
	before := recoveryRefusalBytes(t, l)
	ack := recoveryControl(t, l, recoveryCommand(t, l, p))
	if ack.Status != "error" || !strings.Contains(ack.Message, "process/group is live") {
		t.Fatalf("orphan group not refused: %+v", ack)
	}
	if !reflect.DeepEqual(before, recoveryRefusalBytes(t, l)) {
		t.Fatal("live group refusal mutated state")
	}
}

func TestRequestChangesRecoveryAuthorityEndsAtRequeue(t *testing.T) {
	l, c, _ := newRequestChangesRecovery(t)
	before := recoveryRefusalBytes(t, l)
	if err := l.controlRequestChanges(c.orderID, "second request"); err == nil {
		t.Fatal("second request-changes overwrote the terminal failure binding")
	}
	if !reflect.DeepEqual(before, recoveryRefusalBytes(t, l)) {
		t.Fatal("second request-changes mutated terminal state")
	}
	if err := l.controlRequeue(c.orderID); err != nil {
		t.Fatal(err)
	}
	// A later ordinary failure must retain the existing generic requeue behavior.
	if err := l.mutateOrdersState(func(orders *OrdersFile) (bool, error) {
		orders.Orders[0].Status = OrderStatusFailed
		orders.Orders[0].Stages[0].Status = StageStatusFailed
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.controlEditItem(ControlCommand{OrderID: c.orderID, Prompt: "unsafe generic edit"}); err == nil {
		t.Fatal("generic failed order became editable")
	}
	if err := l.controlRequeue(c.orderID); err != nil {
		t.Fatalf("stale recovery binding blocked generic requeue: %v", err)
	}
}
