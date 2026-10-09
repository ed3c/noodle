package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/skill"
)

func TestRequiredSkillRejectsFIFOReferenceBeforeProcessLaunch(t *testing.T) {
	if selected := os.Getenv("NOODLE_FIFO_SKILL_ROOT"); selected != "" {
		root := os.Getenv("NOODLE_FIFO_RUNTIME_ROOT")
		d := NewProcessDispatcher(ProcessDispatcherConfig{
			ProjectDir: t.TempDir(), RuntimeDir: root, RuntimeKind: "process",
			RuntimeDefault: "cat >/dev/null",
			SkillResolver:  skill.Resolver{SearchPaths: []string{selected}},
		})
		t.Log("dispatching required Skill with FIFO reference")
		_, err := d.Dispatch(context.Background(), DispatchRequest{
			Name: "fifo-negative", Prompt: "synthetic bounded issue",
			Provider: "codex", Model: "test-model",
			WorktreePath: t.TempDir(), AllowPrimaryCheckout: true,
			Skill: "poteto-mode", RequiredSkillSHA256: os.Getenv("NOODLE_FIFO_ENTRY_PIN"),
			RequiredSkillTreeSHA256: os.Getenv("NOODLE_FIFO_TREE_PIN"),
		})
		if err == nil || !strings.Contains(err.Error(), "unsupported file type") {
			t.Fatalf("FIFO reference did not fail closed: %v", err)
		}
		entries, err := os.ReadDir(filepath.Join(root, "sessions"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if _, err := os.Stat(filepath.Join(root, "sessions", entry.Name(), "process.json")); err == nil {
				t.Fatal("rejected FIFO reference launched an OS process")
			}
		}
		return
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("FIFO fixture requires Linux or Darwin")
	}
	selected := t.TempDir()
	method := filepath.Join(selected, "poteto-mode")
	if err := os.MkdirAll(filepath.Join(method, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte("# Verified entry")
	if err := os.WriteFile(filepath.Join(method, "SKILL.md"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(source)
	treePin, err := skill.TreeSHA256(method)
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(method, "references", "blocked.md")
	if output, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("create FIFO: %v: %s", err, output)
	}
	info, err := os.Lstat(fifo)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("fixture is not a FIFO: %v %v", info, err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRequiredSkillRejectsFIFOReferenceBeforeProcessLaunch$", "-test.v")
	cmd.Env = append(os.Environ(),
		"NOODLE_FIFO_SKILL_ROOT="+selected,
		"NOODLE_FIFO_RUNTIME_ROOT="+filepath.Join(t.TempDir(), ".noodle"),
		"NOODLE_FIFO_ENTRY_PIN="+hex.EncodeToString(pin[:]),
		"NOODLE_FIFO_TREE_PIN="+treePin,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("required Skill dispatch blocked on FIFO reference: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("FIFO dispatch child failed: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
