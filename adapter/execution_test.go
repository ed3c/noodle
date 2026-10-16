package adapter

import (
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/execution"
)

func TestRunnerUsesBoundEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	t.Setenv("ADAPTER_SECRET", "host")
	root := t.TempDir()
	e, err := execution.New(root, []string{"PATH=" + os.Getenv("PATH"), "ADAPTER_SECRET=context"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Adapters["backlog"] = config.AdapterConfig{Scripts: map[string]string{"sync": "printf '%s' \"$ADAPTER_SECRET\""}}
	r := NewRunnerWithExecution(root, cfg, e)
	out, err := r.Run(context.Background(), "backlog", "sync", RunOptions{})
	if err != nil || out != "context" {
		t.Fatalf("adapter environment: %q %v", out, err)
	}
}
