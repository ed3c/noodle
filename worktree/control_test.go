package worktree

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareTaskControlPreservesSourceAndReusesOriginal(t *testing.T) {
	root := t.TempDir()
	git := func(directory string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = directory
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	git(root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(root, "add", "source.txt")
	git(root, "commit", "-m", "Keep the original fixture source")
	before := git(root, "status", "--porcelain=v1", "-b")
	task := filepath.Join(t.TempDir(), "task.json")
	raw, _ := json.Marshal(map[string]any{"repository": root, "base_ref": "main", "goal": "prepare an isolated admission"})
	if err := os.WriteFile(task, raw, 0600); err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	first, err := PrepareTaskControl(context.Background(), host, task)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PrepareTaskControl(context.Background(), host, task)
	if err != nil || first != second || second.Status != "prepared" {
		t.Fatalf("original control differs: %+v %+v %v", first, second, err)
	}
	if got := git(root, "status", "--porcelain=v1", "-b"); got != before {
		t.Fatalf("source tree changed: %s", got)
	}
	if first.ControlRoot == root {
		t.Fatal("source became the control checkout")
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("control creation changed source ignore configuration")
	}
	if err := os.Rename(first.ControlRoot, first.ControlRoot+"-lost"); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareTaskControl(context.Background(), host, task); err == nil {
		t.Fatal("lost original creation was replayed")
	}
}

func TestPrepareTaskControlTakesBaseOnlyFromBoundTaskSource(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("commit", "--allow-empty", "-m", "Original source")
	firstHead := git("rev-parse", "HEAD")
	git("commit", "--allow-empty", "-m", "Explicit revised source")
	secondHead := git("rev-parse", "HEAD")
	host, task := t.TempDir(), filepath.Join(t.TempDir(), "task.json")
	var controls []TaskControl
	for _, head := range []string{firstHead, secondHead} {
		raw, _ := json.Marshal(map[string]string{"repository": root, "base_ref": head})
		if err := os.WriteFile(task, raw, 0600); err != nil {
			t.Fatal(err)
		}
		control, err := PrepareTaskControl(context.Background(), host, task)
		if err != nil || control.Head != head {
			t.Fatalf("bound base ignored: %+v %v", control, err)
		}
		controls = append(controls, control)
	}
	if controls[0].ControlRoot == controls[1].ControlRoot {
		t.Fatal("different bound task sources reused one control")
	}
}
