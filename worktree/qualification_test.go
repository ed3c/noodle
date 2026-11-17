package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
)

func TestSmallQualificationRepositoryRejectsEffectsBeforeControlCreation(t *testing.T) {
	for _, kind := range []string{"small", "untracked-large", "selected-base-large", "dependencies", "hook", "filter", "selected-base-relative-hook"} {
		t.Run(kind, func(t *testing.T) {
			repository := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = repository
				cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
				raw, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatal(args, err, string(raw))
				}
				return strings.TrimSpace(string(raw))
			}
			git("init", "-b", "main")
			if err := os.WriteFile(filepath.Join(repository, "source"), []byte("source"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "source")
			git("commit", "-m", "Keep original small source")
			head := git("rev-parse", "HEAD")
			if kind == "untracked-large" || kind == "selected-base-large" {
				f, err := os.Create(filepath.Join(repository, "large"))
				if err != nil {
					t.Fatal(err)
				}
				if err = f.Truncate(65 << 20); err != nil {
					t.Fatal(err)
				}
				f.Close()
				if kind == "selected-base-large" {
					git("add", "large")
					git("commit", "-m", "Preserve a large selected tree")
					head = git("rev-parse", "HEAD")
					git("switch", "--detach", "HEAD~1")
				}
			}
			if kind == "dependencies" {
				if err := os.Mkdir(filepath.Join(repository, "node_modules"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "hook" {
				if err := os.WriteFile(filepath.Join(repository, ".git/hooks/post-checkout"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "selected-base-relative-hook" {
				if err := os.Mkdir(filepath.Join(repository, ".githooks"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repository, ".githooks", "post-checkout"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
				git("add", ".githooks/post-checkout")
				git("commit", "-m", "Preserve original executable hook in selected base")
				head = git("rev-parse", "HEAD")
				git("rm", ".githooks/post-checkout")
				git("commit", "-m", "Remove hook only from current checkout")
				git("config", "core.hooksPath", ".githooks")
				if _, err := os.Stat(filepath.Join(repository, ".githooks")); !os.IsNotExist(err) {
					t.Fatal("counterexample must have no current checkout hook", err)
				}
			}
			if kind == "filter" {
				git("config", "filter.fixture.smudge", "unqualified-command")
				if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("source filter=fixture\n"), 0600); err != nil {
					t.Fatal(err)
				}
				git("add", ".gitattributes")
				git("commit", "-m", "Keep original active checkout filter")
				head = git("rev-parse", "HEAD")
			}
			host := t.TempDir()
			control := TaskControl{Repository: repository, Head: head, ControlRoot: filepath.Join(host, "controls", "fixture", "checkout")}
			err := VerifySmallQualificationRepository(context.Background(), control)
			if kind == "small" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, admission.ErrDeferred) {
				t.Fatal("unqualified repository was not a named HOLD", err)
			}
			if kind == "selected-base-relative-hook" && (err == nil || !strings.Contains(err.Error(), "relative core.hooksPath")) {
				t.Fatal("selected base hook was not rejected by its original relative-path guard", err)
			}
			if _, err := os.Stat(filepath.Join(host, "controls")); !os.IsNotExist(err) {
				t.Fatal("qualification inspection created checkout/custody", err)
			}
		})
	}
}
