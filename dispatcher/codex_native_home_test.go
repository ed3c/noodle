package dispatcher

import "testing"

func TestNativeCodexHomeUsesTaskEnvironment(t *testing.T) {
	for _, test := range []struct {
		env  []string
		want string
	}{
		{[]string{"HOME=/task-home"}, "/task-home/.codex"},
		{[]string{"HOME=/task-home", "CODEX_HOME=/native-task"}, "/native-task"},
	} {
		got, err := nativeCodexHome(test.env)
		if err != nil || got != test.want {
			t.Fatalf("got %q %v, want %q", got, err, test.want)
		}
	}
	if _, err := nativeCodexHome(nil); err == nil {
		t.Fatal("missing task home silently used Host home")
	}
}
