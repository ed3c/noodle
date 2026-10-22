package loop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/state"
)

func TestTaskRoleSeparatesConversationsAndResumesItsOwnPredecessor(t *testing.T) {
	l, cook, _ := localTaskReviewFixture(t)
	if err := l.controlRequestChanges(cook.orderID, "continue role"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "skill.md")
	requestChangesWrite(t, source, []byte("frozen role instructions"))
	packet := func(name string) taskRoleFile {
		path := filepath.Join(t.TempDir(), "role.json")
		recoveryWriteJSON(t, path, map[string]any{"schema": 1, "kind": "local_task_role", "order_id": cook.orderID,
			"skill": name, "context_sha256": recoveryDigest([]byte(name)), "files": []taskRoleFile{{Path: source, SHA256: recoveryDigest(recoveryRead(t, source))}}})
		return taskRoleFile{Path: path, SHA256: recoveryDigest(recoveryRead(t, path))}
	}
	first, second := packet("poteto-role"), packet("review-role")
	selectRole := func(ref taskRoleFile) dispatchCandidate {
		order := l.canonical.Orders[cook.orderID]
		raw, err := json.Marshal(map[string]any{"kind": "local_task", "goal": "inspect role", "write_paths": []string{}, "task_role": ref})
		if err != nil {
			t.Fatal(err)
		}
		order.Stages[0].Prompt = string(raw)
		l.canonical.Orders[cook.orderID] = order
		return dispatchCandidate{OrderID: cook.orderID, StageIndex: 0, Stage: Stage{TaskKey: "local-task", Skill: "local-task", Provider: "codex", Runtime: "process", Prompt: string(raw)}}
	}
	previous := l.canonical.Orders[cook.orderID].Stages[0].Prompt
	cand := selectRole(first)
	node := l.canonical.Orders[cook.orderID]
	node.Stages[0].Prompt = previous
	l.canonical.Orders[cook.orderID] = node
	if err := l.controlEditItem(ControlCommand{OrderID: cook.orderID, Prompt: cand.Stage.Prompt}); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.localTaskRevisionForDispatch(cand, 1); err != nil || !ok {
		t.Fatalf("role handoff rejected original writer exit: %v", err)
	}
	if id, err := l.resumeTaskSessionID(cand, 1); err != nil || id != "" {
		t.Fatalf("new role borrowed original conversation: %q %v", id, err)
	}
	if key, skill, err := l.taskRole(cand); err != nil || key != recoveryDigest([]byte("poteto-role")) || skill != "poteto-role" {
		t.Fatalf("role dispatch = %q %q %v", key, skill, err)
	}
	appendAttempt := func(id, role string) {
		order := l.canonical.Orders[cook.orderID]
		order.Stages[0].Attempts = append(order.Stages[0].Attempts, state.AttemptNode{SessionID: id, Status: state.AttemptCompleted})
		l.canonical.Orders[cook.orderID] = order
		if err := os.MkdirAll(filepath.Join(l.runtimeDir, "sessions", id), 0700); err != nil {
			t.Fatal(err)
		}
		recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "sessions", id, "spawn.json"), map[string]any{"session_id": id, "task_role": role})
	}
	appendAttempt("poteto-first", recoveryDigest([]byte("poteto-role")))
	cand = selectRole(second)
	if id, err := l.resumeTaskSessionID(cand, 2); err != nil || id != "" {
		t.Fatalf("review borrowed Poteto conversation: %q %v", id, err)
	}
	appendAttempt("review-first", recoveryDigest([]byte("review-role")))
	cand = selectRole(first)
	if id, err := l.resumeTaskSessionID(cand, 3); err != nil || id != "poteto-first" {
		t.Fatalf("role did not resume its own thread: %q %v", id, err)
	}
	requestChangesWrite(t, source, []byte("changed instructions"))
	if _, _, err := l.taskRole(cand); err == nil {
		t.Fatal("drifted role instructions accepted")
	}
}
