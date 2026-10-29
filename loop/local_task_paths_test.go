package loop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalTaskDirectoryMergeAndScopeRefusals(t *testing.T) {
	for _, extra := range []string{"", "parse/new.txt", "parser/outside.txt"} {
		t.Run(extra, func(t *testing.T) {
			l, cook, cmd := localTaskReviewFixturePaths(t, "parse", "parse/candidate.txt")
			localTaskRealWorktree(t, l)
			if extra != "" {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(cook.worktreePath, extra)), 0700); err != nil {
					t.Fatal(err)
				}
				requestChangesWrite(t, filepath.Join(cook.worktreePath, extra), []byte("not reviewed"))
			}
			cmd.Action = "merge-local-task"
			err := l.dispatchControlCommand(cmd)
			if extra == "" {
				if err != nil {
					t.Fatal(err)
				}
				if string(recoveryRead(t, filepath.Join(l.projectDir, "parse/candidate.txt"))) != "reviewed candidate\n" {
					t.Fatal("directory candidate not integrated")
				}
			} else if err == nil {
				t.Fatal("unreviewed addition accepted")
			}
		})
	}
}

func TestLocalTaskDirectoryInventoryPreservesDeletionAndRejectsSymlink(t *testing.T) {
	l, cook, _ := localTaskReviewFixturePaths(t, "parse", "parse/candidate.txt")
	runGitInRepo(t, cook.worktreePath, "add", "parse")
	runGitInRepo(t, cook.worktreePath, "commit", "-m", "tracked fixture")
	if err := os.Remove(filepath.Join(cook.worktreePath, "parse/candidate.txt")); err != nil {
		t.Fatal(err)
	}
	names, err := l.localTaskScopeFiles(cook.worktreePath, []string{"parse"})
	if err != nil || !names["parse/candidate.txt"] || names["parse"] {
		t.Fatalf("inventory %v: %v", names, err)
	}
	if err := os.Remove(filepath.Join(cook.worktreePath, "parse")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(cook.worktreePath, "parse")); err != nil {
		t.Fatal(err)
	}
	order := l.canonical.Orders[cook.orderID]
	if _, err := l.localTaskWorktreeDigest(order.Stages[0], cook.worktreePath); err == nil {
		t.Fatal("symlink subtree accepted")
	}
}

func TestLocalTaskScopeUsesLiteralPathBoundaries(t *testing.T) {
	for name, want := range map[string]bool{"parse/a.go": true, "parse/sub/a.go": true, "parser/a.go": false, "parse/../outside": false, "../parse/a.go": false, "parse": true} {
		if got := localTaskPathAllowed([]string{"parse"}, name); got != want {
			t.Fatalf("%s: %v", name, got)
		}
	}
	if localTaskPathAllowed([]string{"parse*"}, "parser/a.go") {
		t.Fatal("scope interpreted as glob")
	}
}

func TestLocalTaskDirectoryMergeRejectsHookAddedFile(t *testing.T) {
	l, cook, cmd := localTaskReviewFixturePaths(t, "parse", "parse/candidate.txt")
	localTaskRealWorktree(t, l)
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "pre-commit")
	requestChangesWrite(t, hook, []byte("#!/bin/sh\nprintf 'unreviewed' > parse/hook.txt\ngit add -- parse/hook.txt\n"))
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	runGitInRepo(t, cook.worktreePath, "config", "core.hooksPath", hooks)
	cmd.Action = "merge-local-task"
	if err := l.dispatchControlCommand(cmd); err == nil {
		t.Fatal("hook-added file integrated without review")
	}
	if _, err := os.Stat(filepath.Join(l.projectDir, "parse/hook.txt")); !os.IsNotExist(err) {
		t.Fatalf("control branch changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cook.worktreePath, "parse/hook.txt")); err != nil {
		t.Fatalf("failed candidate evidence lost: %v", err)
	}
}
