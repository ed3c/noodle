//go:build darwin && cgo

package procx

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestObserveNativeLaunchReadsSelectedGenerationWithoutEnvironment(t *testing.T) {
	t.Setenv("NOODLE_LAUNCH_READBACK_PRIVATE_FIXTURE", "not-an-argument")
	child, original := nativeFixture(t, "normal")
	launch, err := ObserveNativeLaunch(original)
	if err != nil {
		t.Fatal(err)
	}
	image, err := filepath.EvalSymlinks(launch.Executable)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.EvalSymlinks(child.Path)
	if err != nil || image != expected || !reflect.DeepEqual(launch.Arguments, child.Args) {
		t.Fatal("selected kernel image or exact argument boundary differs", err)
	}
	stale := original
	stale.Version++
	if _, err = ObserveNativeLaunch(stale); err == nil {
		t.Fatal("launch readback accepted a different selected generation")
	}
	if err = os.WriteFile(filepath.Join(child.Dir, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}
