package runtime

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

func progressTestCommand(t *testing.T) OpenShellCommand {
	t.Helper()
	base := openShellTestCommand()
	base.Env["NOODLE_SESSION_DIR"] = "/sandbox/noodle-sessions/session-one"
	wrapped, err := wrapOpenShellProgress(base, "/sandbox/payload/noodle-progress", "session-one", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return wrapped
}

func TestOpenShellProgressCommandBindsOriginalLaunch(t *testing.T) {
	original := progressTestCommand(t)
	base, binding, err := unwrapOpenShellProgress(original)
	if err != nil {
		t.Fatal(err)
	}
	if base.Env["NOODLE_PROGRESS_TOKEN"] != "" || binding.Directory != progressDirectory("session-one") {
		t.Fatal("wrapper identity differs")
	}
	again, err := wrapOpenShellProgress(base, binding.Binary, binding.SessionID, binding.Token)
	if err != nil || !reflect.DeepEqual(original, again) {
		t.Fatalf("round trip: %v", err)
	}
	stop, err := progressStopCommand(original, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil || stop.Argv[0] != binding.Binary || stop.Argv[1] != "stop" || !maps.Equal(stop.Env, map[string]string{"NOODLE_PROGRESS_TOKEN": binding.Token}) {
		t.Fatalf("stop authority differs: %v", err)
	}
	for _, index := range []int{3, 5, 7, 9, 11, 13} {
		t.Run(original.Argv[index], func(t *testing.T) {
			changed := original
			changed.Argv = append([]string{}, original.Argv...)
			changed.Argv[index] += "-changed"
			if _, _, err := unwrapOpenShellProgress(changed); err == nil {
				t.Fatal("modified original launch accepted")
			}
		})
	}
}
