package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

type codexStdio struct {
	input    *os.File
	output   io.ReadCloser
	decoder  *json.Decoder
	once     sync.Once
	closeErr error
}

func (s *codexStdio) SetWriteDeadline(t time.Time) error { return s.input.SetWriteDeadline(t) }
func (s *codexStdio) WriteJSON(value any) error          { return json.NewEncoder(s.input).Encode(value) }
func (s *codexStdio) ReadMessage() (int, []byte, error) {
	var raw json.RawMessage
	err := s.decoder.Decode(&raw)
	if err != nil {
		_ = s.output.Close()
	}
	return 0, raw, err
}

// EOF requests native shutdown. It is not proof of process or MCP exit.
func (s *codexStdio) Close() error {
	s.once.Do(func() { s.closeErr = s.input.Close() })
	return s.closeErr
}

// The caller retains the returned process even when initialization is unknown.
// No cancellation context kills the server or its children.
func startCodexStdio(ctx context.Context, binary, worktree string, env []string, stderr io.Writer, onEvent func(json.RawMessage) error) (*codexRPC, *ProcessHandle, <-chan error, error) {
	if stderr == nil {
		return nil, nil, nil, fmt.Errorf("native stderr evidence destination unavailable")
	}
	cmd := exec.Command(binary, "app-server")
	cmd.Dir, cmd.Env = worktree, env
	process, err := StartProcess(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	stderrDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stderr, process.Stderr())
		closeErr := process.Stderr().Close()
		if err == nil {
			err = closeErr
		}
		stderrDone <- err
		close(stderrDone)
	}()
	input, ok := process.Stdin().(*os.File)
	if !ok {
		return nil, process, stderrDone, fmt.Errorf("native stdin does not support bounded writes")
	}
	transport := &codexStdio{input: input, output: process.Stdout(), decoder: json.NewDecoder(process.Stdout())}
	client := &codexRPC{conn: transport, pending: make(map[string]chan codexRPCReply), done: make(chan struct{}), onEvent: onEvent}
	if err := client.initialize(ctx); err != nil {
		return client, process, stderrDone, err
	}
	return client, process, stderrDone, nil
}
