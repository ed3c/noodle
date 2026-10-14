package mise

import (
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/execution"
)

func TestBuilderInternalRunnerUsesContextEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	t.Setenv("BACKLOG_ID", "host")
	root := t.TempDir()
	e, err := execution.New(root, []string{"PATH=" + os.Getenv("PATH"), "BACKLOG_ID=context"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Adapters["backlog"] = config.AdapterConfig{Scripts: map[string]string{"sync": `printf '{"id":"%s","title":"fixture","status":"open"}\n' "$BACKLOG_ID"`}}
	cfg, err = config.Bind(cfg, e)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBuilderWithExecution(root, cfg, e)
	brief, _, _, err := b.Build(context.Background(), ActiveSummary{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(brief.Backlog) != 1 || brief.Backlog[0].ID != "context" {
		t.Fatalf("internal runner leaked env: %+v", brief.Backlog)
	}
}
