package loop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func TestLocalTaskRetentionCommitsBranchWithoutMerge(t *testing.T) {
	l, cook, cmd := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	before, err := recoveryGit(l.projectDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Action = "retain-local-task"
	if err = l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if after, err := recoveryGit(l.projectDir, "rev-parse", "HEAD"); err != nil || after != before {
		t.Fatal("retained candidate changed integration HEAD", after, err)
	}
	if _, err := os.Stat(filepath.Join(l.projectDir, "candidate.txt")); !os.IsNotExist(err) {
		t.Fatal("retained candidate was merged", err)
	}
	order := l.canonical.Orders[cook.orderID]
	if order.Status != state.OrderCompleted || order.Stages[0].Status != state.StageCompleted || len(order.Stages[0].Extra[localTaskMergeKey]) != 0 {
		t.Fatal("retention claimed a merge or left the task active", order)
	}
	var retained struct {
		Schema      int              `json:"schema"`
		Disposition string           `json:"disposition"`
		Candidate   localTaskFileRef `json:"candidate"`
		Head        string           `json:"head"`
		Tree        string           `json:"tree"`
		Branch      string           `json:"branch"`
		ControlID   string           `json:"control_id"`
	}
	if err = json.Unmarshal(order.Stages[0].Extra["local_task_retention"], &retained); err != nil {
		t.Fatal(err)
	}
	if retained.Schema != 1 || retained.Disposition != "retain_branch" || retained.Candidate.Path != cmd.Value || retained.Candidate.SHA256 != cmd.Target || retained.ControlID != cmd.ID || retained.Head == "" || retained.Tree == "" || retained.Branch != "refs/heads/"+cook.worktreeName {
		t.Fatal("retention lost original candidate, control or branch", retained)
	}
	if head, err := recoveryGit(cook.worktreePath, "rev-parse", "HEAD"); err != nil || head != retained.Head {
		t.Fatal("retained branch commit missing", head, err)
	}
	if dirty, err := recoveryGit(cook.worktreePath, "status", "--porcelain"); err != nil || dirty != "" {
		t.Fatal("retained candidate was not normally committed", dirty, err)
	}
	for _, effect := range l.effectLedger.All() {
		if effect.Effect.Type == reducer.EffectMerge {
			t.Fatal("retention fabricated a merge effect")
		}
	}
	if err = l.dispatchControlCommand(cmd); err != nil {
		t.Fatal("same retention could not read its completed result", err)
	}
	if err = l.checkCompletedLocalTask(order, order.Stages[0]); err == nil {
		t.Fatal("cleanup accepted retained disposition without original ACK")
	}
	_, ackPath, _ := l.controlPaths()
	if err = appendAcks(ackPath, []ControlAck{{ID: cmd.ID, Action: cmd.Action, Status: "ok"}}); err != nil {
		t.Fatal(err)
	}
	if err = l.checkCompletedLocalTask(order, order.Stages[0]); err != nil {
		t.Fatal("cleanup rejected exact retained disposition", err)
	}
	cmd.ID = "foreign-retention"
	if err = l.dispatchControlCommand(cmd); err == nil {
		t.Fatal("another control adopted the completed disposition")
	}
}

func TestLocalTaskRetentionRejectsCandidateDriftBeforeCommit(t *testing.T) {
	l, cook, cmd := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	cmd.Action = "retain-local-task"
	requestChangesWrite(t, filepath.Join(cook.worktreePath, "candidate.txt"), []byte("unreviewed bytes\n"))
	if err := l.dispatchControlCommand(cmd); err == nil {
		t.Fatal("retention accepted changed candidate")
	}
	if l.canonical.Orders[cook.orderID].Status != state.OrderActive || len(l.canonical.Orders[cook.orderID].Stages[0].Extra["local_task_retention"]) != 0 {
		t.Fatal("refused retention changed canonical disposition")
	}
}
