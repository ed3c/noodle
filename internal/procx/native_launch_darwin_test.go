//go:build darwin && cgo

package procx

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
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

func TestObserveNativeImageDoesNotRequireQualifiedArgv(t *testing.T) {
	child, original := nativeFixture(t, "exec-empty-argv")
	if err := os.WriteFile(filepath.Join(child.Dir, "exec"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var current NativeIdentity
	for {
		var err error
		current, err = ObserveNativeIdentity(original.PID)
		if err == nil && !original.SameGeneration(current) && !current.Zombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("selected fixture did not exec", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	image, err := ObserveNativeImage(current)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := filepath.EvalSymlinks(image)
	expected, expectedErr := filepath.EvalSymlinks("/bin/sleep")
	if err != nil || expectedErr != nil || actual != expected {
		t.Fatal("kernel-only image differs", image, err, expectedErr)
	}
	if _, err = ObserveNativeLaunch(current); err == nil {
		t.Fatal("full launch accepted unqualified argv0")
	}
	if _, err = ObserveNativeImage(original); err == nil {
		t.Fatal("image-only reader accepted the pre-exec identity")
	}
}
