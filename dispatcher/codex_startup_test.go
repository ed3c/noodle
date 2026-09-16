package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/parse"
)

func readCodexStartup(t *testing.T, path string) codexStartupReceipt {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt codexStartupReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

type startupInput struct {
	n                          int
	writeErr, closeErr         error
	closed                     bool
	writeEntered, releaseWrite chan struct{}
	closeEntered, releaseClose chan struct{}
}

func (w *startupInput) Write(p []byte) (int, error) {
	if w.writeEntered != nil {
		close(w.writeEntered)
		<-w.releaseWrite
	}
	return w.n, w.writeErr
}
func (w *startupInput) Close() error {
	if w.closeEntered != nil {
		close(w.closeEntered)
		<-w.releaseClose
	}
	w.closed = true
	return w.closeErr
}

func TestCodexStartupPendingCloseIsNotCompletion(t *testing.T) {
	s, err := newCodexStartup(t.TempDir(), "pending-close", 4)
	if err != nil {
		t.Fatal(err)
	}
	w := &startupInput{n: 4, closeEntered: make(chan struct{}), releaseClose: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); s.writeInput(w, "test") }()
	<-w.closeEntered
	r := readCodexStartup(t, s.path)
	close(w.releaseClose)
	<-done
	if r.Stdin.WrittenBytes == nil || *r.Stdin.WrittenBytes != 4 || r.Stdin.WriteCompletedAt == nil || r.Stdin.CloseCompletedAt != nil {
		t.Fatalf("pending close confused with pending write or success: %+v", r.Stdin)
	}
}

func TestCodexStartupCancellationWithBlockedInput(t *testing.T) {
	root := t.TempDir()
	d := NewProcessDispatcher(ProcessDispatcherConfig{ProjectDir: root, RuntimeDir: filepath.Join(root, ".noodle"), RuntimeKind: "process",
		RuntimeDefault: `printf '%s\n' '{"type":"thread.started","thread_id":"fixture"}'; exec sleep 30`})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := d.Dispatch(ctx, DispatchRequest{Name: "blocked-input", Prompt: strings.Repeat("x", 1<<20), Provider: "codex", Model: "fixture", WorktreePath: root, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Events() {
		cancel()
	}
	if session.Outcome().Status != StatusCancelled {
		t.Fatalf("lost cancellation: %+v", session.Outcome())
	}
	s := session.(*processSession)
	select {
	case <-s.process.Done():
	default:
		t.Fatal("child survived cancellation")
	}
	// Receipt completion is asynchronous and must not become a lifecycle wait.
	// Bound only this test's readback; a stalled write remains explicitly pending.
	deadline := time.Now().Add(time.Second)
	for {
		r := readCodexStartup(t, s.startup.path)
		if r.Stdin.CloseCompletedAt != nil {
			if r.Stdin.WrittenBytes == nil || *r.Stdin.WrittenBytes >= r.Stdin.ExpectedBytes || r.Stdin.WriteError == "" {
				t.Fatalf("blocked input reported success: %+v", r.Stdin)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled stdin writer did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCodexStartupInputResults(t *testing.T) {
	for _, tc := range []struct {
		name               string
		n                  int
		writeErr, closeErr error
	}{
		{"complete", 6, nil, nil},
		{"short write", 2, nil, nil},
		{"write failed", 2, errors.New("broken pipe"), nil},
		{"close failed", 6, nil, errors.New("close failed")},
		{"both failed", 0, errors.New("write failed"), errors.New("close failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newCodexStartup(t.TempDir(), "input-session", 6)
			if err != nil {
				t.Fatal(err)
			}
			w := &startupInput{n: tc.n, writeErr: tc.writeErr, closeErr: tc.closeErr}
			s.writeInput(w, "秘密") // six bytes; do not count runes or persist the prompt
			r := readCodexStartup(t, s.path)
			if r.SessionID != "input-session" || r.Stdin.ExpectedBytes != 6 || r.Stdin.WrittenBytes == nil || *r.Stdin.WrittenBytes != tc.n || !w.closed {
				t.Fatalf("lost write/close result: %+v, closed=%v", r, w.closed)
			}
			wantWrite, wantClose := "", ""
			if tc.writeErr != nil {
				wantWrite = tc.writeErr.Error()
			}
			if tc.closeErr != nil {
				wantClose = tc.closeErr.Error()
			}
			if r.Stdin.WriteError != wantWrite || r.Stdin.CloseError != wantClose || r.Stdin.WriteStartedAt == nil || r.Stdin.WriteCompletedAt == nil || r.Stdin.CloseCompletedAt == nil {
				t.Fatalf("lost independent completion/error: %+v", r.Stdin)
			}
			if r.Stdin.WriteCompletedAt.Before(*r.Stdin.WriteStartedAt) || r.Stdin.CloseCompletedAt.Before(*r.Stdin.WriteCompletedAt) || r.FirstInitAt != nil {
				t.Fatalf("incorrect observation order: %+v", r)
			}
			data, _ := os.ReadFile(s.path)
			if bytes.Contains(data, []byte("秘密")) {
				t.Fatal("prompt leaked into observation")
			}
		})
	}
}

func TestCodexStartupPendingWriteIsNotCompletion(t *testing.T) {
	s, err := newCodexStartup(t.TempDir(), "pending", 4)
	if err != nil {
		t.Fatal(err)
	}
	w := &startupInput{n: 0, writeErr: io.ErrClosedPipe, writeEntered: make(chan struct{}), releaseWrite: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); s.writeInput(w, "test") }()
	<-w.writeEntered
	r := readCodexStartup(t, s.path)
	close(w.releaseWrite)
	<-done
	if r.Stdin.WriteStartedAt == nil || r.Stdin.WriteCompletedAt != nil || r.Stdin.WrittenBytes != nil || r.Stdin.CloseCompletedAt != nil {
		t.Fatalf("pending write reported complete: %+v", r.Stdin)
	}
	if !w.closed {
		t.Fatal("failed write did not close stdin")
	}
}

func TestCodexStartupRealDispatchObservesEOFAndInit(t *testing.T) {
	root := t.TempDir()
	// The real child waits for EOF before emitting init. Stderr alone is not init.
	d := NewProcessDispatcher(ProcessDispatcherConfig{ProjectDir: root, RuntimeDir: filepath.Join(root, ".noodle"), RuntimeKind: "process",
		RuntimeDefault: `cat > received.txt; printf '%s\n' 'startup marker' >&2; printf '%s\n' '{"type":"thread.started","thread_id":"fixture"}'`})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := d.Dispatch(ctx, DispatchRequest{Name: "startup", Prompt: "unique prompt", Provider: "codex", Model: "fixture", WorktreePath: root, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Events() {
	}
	s := session.(*processSession)
	<-s.stderrDone
	r := readCodexStartup(t, s.startup.path)
	input, err := os.ReadFile(filepath.Join(root, "received.txt"))
	if err != nil {
		t.Fatal(err)
	}
	composed, err := os.ReadFile(filepath.Join(root, ".noodle", "sessions", session.ID(), "input.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input, composed) || !bytes.Contains(input, []byte("unique prompt")) || r.Stdin.WrittenBytes == nil || *r.Stdin.WrittenBytes != len(composed) || r.Stdin.ExpectedBytes != len(composed) || r.Stdin.WriteError != "" || r.Stdin.CloseError != "" || r.Stdin.CloseCompletedAt == nil {
		t.Fatalf("stdin did not reach child through EOF: %+v", r)
	}
	if r.FirstStdout == nil || r.FirstStderr == nil || r.FirstInitAt == nil || r.PersistenceError != "" {
		t.Fatalf("missing real consumer observation: %+v", r)
	}
	stderr, _ := os.ReadFile(s.stderrPath)
	if string(stderr) != "startup marker\n" {
		t.Fatalf("stderr changed: %q", stderr)
	}
	if session.Outcome().HasDeliverable {
		t.Fatal("initialization became delivery")
	}
}

func TestCodexStartupPreInitOutputAndFirstOnly(t *testing.T) {
	s, err := newCodexStartup(t.TempDir(), "output", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, stderr := range []bool{true, false} {
		reader := s.reader(io.NopCloser(strings.NewReader("Reading prompt from stdin...\n")), stderr)
		got, err := io.ReadAll(reader)
		if err != nil || string(got) != "Reading prompt from stdin...\n" {
			t.Fatalf("bytes changed: %q %v", got, err)
		}
		reader.Close()
	}
	s.observeInit(parse.EventType("message"))
	r := readCodexStartup(t, s.path)
	if r.FirstInitAt != nil || r.FirstStderr == nil || r.FirstStdout == nil {
		t.Fatalf("raw output became init: %+v", r)
	}
	s.observeInit(parse.EventInit)
	first, _ := os.ReadFile(s.path)
	s.observeInit(parse.EventInit)
	again, _ := os.ReadFile(s.path)
	if !bytes.Equal(first, again) || readCodexStartup(t, s.path).FirstInitAt == nil {
		t.Fatal("first init lost or overwritten")
	}
}

func TestCodexStartupPersistenceFailureIsVisible(t *testing.T) {
	dir := t.TempDir()
	s, err := newCodexStartup(dir, "disk-failure", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Make the rename target a directory; then restore it to prove a later
	// successful write cannot erase the missing-observation history.
	if err := os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.path, 0700); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	defer slog.SetDefault(previous)
	s.observeInit(parse.EventInit)
	if !strings.Contains(log.String(), "observation incomplete") {
		t.Fatal("lost persistence error")
	}
	if err := os.Remove(s.path); err != nil {
		t.Fatal(err)
	}
	s.writeInput(&startupInput{}, "")
	if readCodexStartup(t, s.path).PersistenceError == "" {
		t.Fatal("persistence failure erased")
	}
}
