package dispatcher

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestProcessHandleSpawnAndDone(t *testing.T) {
	cmd := exec.Command("echo", "hello")
	h, err := StartProcess(cmd)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	t.Cleanup(func() { _ = h.Stdout().Close(); _ = h.Stderr().Close() })

	buf := make([]byte, 64)
	n, readErr := h.Stdout().Read(buf)
	if readErr != nil && readErr != io.EOF {
		t.Fatalf("stdout read: %v", readErr)
	}
	got := string(buf[:n])
	if got != "hello\n" {
		t.Fatalf("stdout = %q, want %q", got, "hello\n")
	}

	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}

	code, exited := h.ExitCode()
	if !exited {
		t.Fatal("expected exited = true")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestProcessHandleForceKill(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	h, err := StartProcess(cmd)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	t.Cleanup(func() { _ = h.Stdout().Close(); _ = h.Stderr().Close() })

	if err := h.ForceKill(); err != nil {
		t.Fatalf("ForceKill: %v", err)
	}

	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit after kill")
	}

	_, exited := h.ExitCode()
	if !exited {
		t.Fatal("expected exited = true after kill")
	}
}

func TestProcessHandlePID(t *testing.T) {
	cmd := exec.Command("echo", "hello")
	h, err := StartProcess(cmd)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	t.Cleanup(func() { _ = h.Stdout().Close(); _ = h.Stderr().Close() })
	pid := h.PID()
	if pid <= 0 {
		t.Fatalf("PID = %d, want > 0", pid)
	}
	<-h.Done()
}

func TestProcessHandleNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test requires sh")
	}
	cmd := exec.Command("sh", "-c", "exit 42")
	h, err := StartProcess(cmd)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	t.Cleanup(func() { _ = h.Stdout().Close(); _ = h.Stderr().Close() })
	<-h.Done()

	code, exited := h.ExitCode()
	if !exited {
		t.Fatal("expected exited = true")
	}
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

func TestWriteProcessMetadata(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)

	if err := WriteProcessMetadata(dir, "session-1", 12345, ts); err != nil {
		t.Fatalf("WriteProcessMetadata: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "process.json"))
	if err != nil {
		t.Fatalf("read process.json: %v", err)
	}
	want := `{"pid":12345,"session_id":"session-1","started_at":"2026-02-27T12:00:00Z"}`
	if string(data) != want {
		t.Fatalf("process.json =\n  %s\nwant\n  %s", data, want)
	}
}

// Awaiting Done establishes fast exit before any stream consumer; no timing sleep.
func TestProcessHandleOutputSurvivesExit(t *testing.T) {
	cmd := exec.Command("sh", "-c", "printf 'stdout-after-exit'; printf 'stderr-after-exit' >&2; exit 42")
	h, err := StartProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Stdout().Close(); _ = h.Stderr().Close() })
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		h.ForceKill()
		t.Fatal("unread small output blocked process exit")
	}
	for _, stream := range []struct {
		name string
		r    io.Reader
		want string
	}{{"stdout", h.Stdout(), "stdout-after-exit"}, {"stderr", h.Stderr(), "stderr-after-exit"}} {
		got, err := io.ReadAll(stream.r)
		if err != nil || string(got) != stream.want {
			t.Errorf("%s after Done = %q, err=%v; want %q", stream.name, got, err, stream.want)
		}
	}
	if code, exited := h.ExitCode(); !exited || code != 42 {
		t.Fatalf("exit=%d exited=%v", code, exited)
	}
}

func TestProcessHandleLargeOutputAndUnreadCancellation(t *testing.T) {
	for _, drain := range []bool{true, false} {
		t.Run(fmtBool(drain), func(t *testing.T) {
			cmd := exec.Command("sh", "-c", "head -c 1048576 /dev/zero; head -c 1048576 /dev/zero >&2")
			h, err := StartProcess(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Stdout().Close()
			defer h.Stderr().Close()
			defer h.ForceKill()
			if drain {
				type result struct {
					data []byte
					err  error
				}
				results := make(chan result, 2)
				for _, r := range []io.Reader{h.Stdout(), h.Stderr()} {
					go func(r io.Reader) { b, e := io.ReadAll(r); results <- result{b, e} }(r)
				}
				for i := 0; i < 2; i++ {
					select {
					case r := <-results:
						if r.err != nil || !bytes.Equal(r.data, make([]byte, 1048576)) {
							t.Fatalf("large stream len=%d err=%v", len(r.data), r.err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("stream drain blocked")
					}
				}
			} else {
				if err := h.ForceKill(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-h.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("process exit blocked")
			}
		})
	}
}
func fmtBool(v bool) string {
	if v {
		return "drained"
	}
	return "unread-killed"
}

func TestProcessHandleStartFailureClosesOwnedPipes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor readback uses procfs")
	}
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := StartProcess(exec.Command("/missing-noodle-80-executable")); err == nil {
			t.Fatal("missing executable admitted")
		}
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("start failures leaked descriptors: %d -> %d", len(before), len(after))
	}
}
