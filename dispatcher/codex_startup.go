package dispatcher

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/parse"
)

// This receipt observes the process boundary, not Codex internals or lifecycle
// authority. A nil observation means not observed, never successful completion.
type codexStartupReceipt struct {
	SessionID string `json:"session_id"`
	Stdin     struct {
		ExpectedBytes    int        `json:"expected_bytes"`
		WriteStartedAt   *time.Time `json:"write_started_at"`
		WriteCompletedAt *time.Time `json:"write_completed_at"`
		WrittenBytes     *int       `json:"written_bytes"`
		WriteError       string     `json:"write_error"`
		CloseCompletedAt *time.Time `json:"close_completed_at"`
		CloseError       string     `json:"close_error"`
	} `json:"stdin"`
	FirstStdout      *codexFirstRead `json:"first_stdout"`
	FirstStderr      *codexFirstRead `json:"first_stderr"`
	FirstInitAt      *time.Time      `json:"first_init_at"`
	PersistenceError string          `json:"persistence_error"`
}

type codexFirstRead struct {
	At    time.Time `json:"at"`
	Bytes int       `json:"bytes"`
}

type codexStartup struct {
	mu      sync.Mutex
	path    string
	receipt codexStartupReceipt
}

func newCodexStartup(sessionDir, sessionID string, promptBytes int) (*codexStartup, error) {
	s := &codexStartup{path: filepath.Join(sessionDir, "codex-startup.json")}
	s.receipt.SessionID = sessionID
	s.receipt.Stdin.ExpectedBytes = promptBytes
	if err := s.persist(); err != nil {
		return nil, fmt.Errorf("write Codex startup observation: %w", err)
	}
	return s, nil
}

func (s *codexStartup) persist() error {
	data, err := json.Marshal(s.receipt)
	if err != nil {
		return err
	}
	return filex.WriteFileAtomic(s.path, data)
}

func (s *codexStartup) update(change func(*codexStartupReceipt) bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !change(&s.receipt) {
		return
	}
	if err := s.persist(); err != nil {
		// Preserve a failed observation if a later write succeeds, and report it
		// out of band even when the receipt cannot be written at all.
		s.receipt.PersistenceError = err.Error()
		slog.Error("Codex startup observation incomplete", "session_id", s.receipt.SessionID, "error", err)
	}
}

func (s *codexStartup) writeInput(stdin io.WriteCloser, prompt string) {
	s.update(func(r *codexStartupReceipt) bool {
		now := nowUTC()
		r.Stdin.WriteStartedAt = &now
		return true
	})
	n, writeErr := io.WriteString(stdin, prompt)
	s.update(func(r *codexStartupReceipt) bool {
		now := nowUTC()
		r.Stdin.WriteCompletedAt = &now
		r.Stdin.WrittenBytes = &n
		if writeErr != nil {
			r.Stdin.WriteError = writeErr.Error()
		}
		return true
	})
	// Close even after a failed/short write. Success here proves only that the
	// parent's close returned successfully, not that Codex consumed the prompt.
	closeErr := stdin.Close()
	s.update(func(r *codexStartupReceipt) bool {
		now := nowUTC()
		r.Stdin.CloseCompletedAt = &now
		if closeErr != nil {
			r.Stdin.CloseError = closeErr.Error()
		}
		return true
	})
}

func (s *codexStartup) observeInit(typ parse.EventType) {
	if typ != parse.EventInit {
		return
	}
	s.update(func(r *codexStartupReceipt) bool {
		if r.FirstInitAt != nil {
			return false
		}
		now := nowUTC()
		r.FirstInitAt = &now
		return true
	})
}

func (s *codexStartup) reader(reader io.ReadCloser, stderr bool) io.ReadCloser {
	if s == nil {
		return reader
	}
	return &codexStartupReader{ReadCloser: reader, startup: s, stderr: stderr}
}

type codexStartupReader struct {
	io.ReadCloser
	startup *codexStartup
	stderr  bool
	once    sync.Once
}

func (r *codexStartupReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.once.Do(func() {
			r.startup.update(func(s *codexStartupReceipt) bool {
				observation := &codexFirstRead{At: nowUTC(), Bytes: n}
				if r.stderr {
					s.FirstStderr = observation
				} else {
					s.FirstStdout = observation
				}
				return true
			})
		})
	}
	return n, err
}
