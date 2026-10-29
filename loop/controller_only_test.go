package loop

import (
	"github.com/poteto/noodle/config"
	"testing"
)

func TestControllerOnlyRefusesModeAndEveryRuntimeBeforeDispatch(t *testing.T) {
	l := New(t.TempDir(), "noodle", config.DefaultConfig(), Dependencies{DispatchBlocked: "worker profile not selected"})
	for _, mode := range []string{"supervised", "auto"} {
		if err := l.controlMode(mode); err == nil {
			t.Fatalf("controller enabled %s", mode)
		}
	}
	for _, runtime := range []string{"process", "codex-app-server", "sprites", "cursor"} {
		if err := l.requireProcessOrder(runtime, "original"); err == nil {
			t.Fatalf("controller admitted %s", runtime)
		}
	}
	// A normal source-bound context still follows its existing dispatch contract.
	l.deps.DispatchBlocked = ""
	if err := l.requireProcessOrder("process", "original"); err != nil {
		t.Fatal(err)
	}
}
