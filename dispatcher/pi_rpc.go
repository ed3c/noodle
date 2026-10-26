package dispatcher

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/parse"
	"github.com/poteto/noodle/stamp"
)

const piControlTimeout = 10 * time.Second

func (d *ProcessDispatcher) validatePi(req DispatchRequest) error {
	if d.runtimeKind != "process" || d.runtimeDefault != "" || (req.Runtime != "" && NormalizeRuntime(req.Runtime) != "process") {
		return fmt.Errorf("Pi RPC unavailable for selected runtime")
	}
	if req.ResumeSessionID != "" {
		return fmt.Errorf("Pi resume is not supported")
	}
	if req.MaxTurns != 0 || req.BudgetCap != 0 {
		return fmt.Errorf("Pi max turns and budget cap are not supported")
	}
	if len(d.providerConfigs.Pi.Args) != 0 {
		return fmt.Errorf("Pi extra arguments are not supported")
	}
	if path := strings.TrimSpace(d.providerConfigs.Pi.Path); path != "" {
		if _, err := d.execution.ResolvePath(path); err != nil {
			return fmt.Errorf("resolve Pi executable directory: %w", err)
		}
	}
	provider, model, ok := strings.Cut(req.Model, "/")
	if !ok || provider == "" || model == "" || strings.ContainsAny(req.Model, " \t\r\n:*?[]\\") || strings.HasPrefix(provider, "-") || strings.HasPrefix(model, "-") {
		return fmt.Errorf("Pi model %q is not an exact provider/model-id", req.Model)
	}
	switch req.ReasoningLevel {
	case "", "off", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("Pi thinking level %q is not supported", req.ReasoningLevel)
	}
	return nil
}

type piState struct {
	Model struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	} `json:"model"`
	SessionID   string `json:"sessionId"`
	SessionFile string `json:"sessionFile"`
	Streaming   *bool  `json:"isStreaming"`
	Compacting  *bool  `json:"isCompacting"`
	Pending     *int   `json:"pendingMessageCount"`
}

type piReceipt struct {
	ExpectedModel string  `json:"expected_model"`
	Native        piState `json:"native"`
	Verified      bool    `json:"verified"`
	PromptSent    bool    `json:"prompt_sent"`
	Disposition   string  `json:"disposition"`
	Settled       bool    `json:"settled"`
	Result        bool    `json:"result"`
	StdinClosed   bool    `json:"stdin_closed"`
	Error         string  `json:"error"`
}

// One RPC controller owns the two commands. Stdout is always drained by the
// session, including during writes and after rejection. The receipt is evidence,
// never a substitute for the original process/group exit observation.
type piRPC struct {
	failed     chan struct{}
	failOnce   sync.Once
	mu         sync.Mutex
	receipt    piReceipt
	dir        string
	process    *ProcessHandle
	stateReady chan struct{}
	ackReady   chan struct{}
	prompt     string
	timeout    time.Duration
}

func newPiRPC(dir, model, prompt string) (*piRPC, error) {
	p := &piRPC{failed: make(chan struct{}), dir: dir, prompt: prompt, timeout: piControlTimeout, stateReady: make(chan struct{}), ackReady: make(chan struct{})}
	p.receipt.ExpectedModel = model
	if err := os.MkdirAll(filepath.Join(dir, "pi"), 0700); err != nil {
		return nil, fmt.Errorf("create Pi session directory: %w", err)
	}
	if err := p.persist(); err != nil {
		return nil, err
	}
	return p, nil
}

// Caller holds mu once the process starts.
func (p *piRPC) persist() error {
	data, err := json.Marshal(p.receipt)
	if err != nil {
		return err
	}
	return filex.WriteFileAtomic(filepath.Join(p.dir, "pi-rpc.json"), data)
}

func (p *piRPC) fail(err error) {
	p.mu.Lock()
	if p.receipt.Error == "" {
		p.receipt.Error = err.Error()
		if persistErr := p.persist(); persistErr != nil {
			p.receipt.Error += "; persist Pi receipt: " + persistErr.Error()
		}
	}
	p.mu.Unlock()
	p.failOnce.Do(func() { close(p.failed) })
	_ = p.process.Stdin().Close()
	if err := p.process.ForceKill(); err != nil {
		p.mu.Lock()
		p.receipt.Error += "; stop original Pi process/group: " + err.Error()
		if persistErr := p.persist(); persistErr != nil {
			p.receipt.Error += "; persist stop diagnostic: " + persistErr.Error()
		}
		p.mu.Unlock()
	}
}

func (p *piRPC) start(ctx context.Context, process *ProcessHandle) {
	process.ioWG.Add(1)
	go func() {
		defer process.ioWG.Done()
		// This writer may encounter kernel backpressure. The watchdog closes stdin
		// and kills the original group, so neither pipe can trap cancellation.
		finished := make(chan error, 1)
		process.ioWG.Add(1)
		go func() {
			defer process.ioWG.Done()
			finished <- json.NewEncoder(process.Stdin()).Encode(map[string]string{"id": "noodle-state", "type": "get_state"})
		}()
		if !p.wait(ctx, finished, nil) || !p.wait(ctx, nil, p.stateReady) {
			return
		}
		p.mu.Lock()
		if p.receipt.Error != "" {
			p.mu.Unlock()
			return
		}
		if _, exited := process.ExitCode(); exited {
			p.mu.Unlock()
			p.fail(fmt.Errorf("Pi exited before prompt"))
			return
		}
		p.receipt.PromptSent = true
		err := p.persist()
		p.mu.Unlock()
		if err != nil {
			p.fail(fmt.Errorf("persist Pi prompt evidence: %w", err))
			return
		}
		process.ioWG.Add(1)
		go func() {
			defer process.ioWG.Done()
			finished <- json.NewEncoder(process.Stdin()).Encode(map[string]string{"id": "noodle-prompt", "type": "prompt", "message": p.prompt})
		}()
		if !p.wait(ctx, finished, nil) {
			return
		}
		p.wait(ctx, nil, p.ackReady)
	}()
}

func (p *piRPC) wait(ctx context.Context, written <-chan error, ready <-chan struct{}) bool {
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case err := <-written:
		if err != nil {
			p.fail(fmt.Errorf("write Pi RPC command: %w", err))
			return false
		}
	case <-ready:
	case <-ctx.Done():
		p.fail(ctx.Err())
		return false
	case <-p.failed:
		return false
	case <-timer.C:
		p.fail(fmt.Errorf("Pi RPC control timed out"))
		return false
	}
	return true
}

func (p *piRPC) observe(line []byte, processor *stamp.Processor) ([]byte, []parse.CanonicalEvent, error) {
	var row struct {
		Type    string          `json:"type"`
		ID      string          `json:"id"`
		Command string          `json:"command"`
		Success bool            `json:"success"`
		Error   string          `json:"error"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(line, &row); err != nil {
		return nil, nil, fmt.Errorf("decode Pi RPC: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.failed:
		return nil, nil, nil
	default:
	}
	if row.Type == "response" {
		if !row.Success {
			return nil, nil, fmt.Errorf("Pi %s rejected: %s", row.Command, row.Error)
		}
		switch row.Command {
		case "get_state":
			if row.ID != "noodle-state" || p.receipt.Verified {
				return nil, nil, fmt.Errorf("Pi get_state response ID or sequence mismatch")
			}
			var state piState
			if err := json.Unmarshal(row.Data, &state); err != nil {
				return nil, nil, fmt.Errorf("decode Pi state: %w", err)
			}
			p.receipt.Native = state
			provider, model, _ := strings.Cut(p.receipt.ExpectedModel, "/")
			if state.Model.Provider != provider || state.Model.ID != model {
				return nil, nil, fmt.Errorf("Pi model identity mismatch: got %q/%q", state.Model.Provider, state.Model.ID)
			}
			if strings.TrimSpace(state.SessionID) == "" || !filepath.IsAbs(state.SessionFile) {
				return nil, nil, fmt.Errorf("Pi native session identity missing")
			}
			relative, err := filepath.Rel(filepath.Join(p.dir, "pi"), state.SessionFile)
			if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return nil, nil, fmt.Errorf("Pi native session file outside owned session directory")
			}
			if state.Streaming == nil || state.Compacting == nil || state.Pending == nil || *state.Streaming || *state.Compacting || *state.Pending != 0 {
				return nil, nil, fmt.Errorf("Pi state is busy or idle identity missing")
			}
			p.receipt.Verified = true
			if err := p.persist(); err != nil {
				return nil, nil, err
			}
			close(p.stateReady)
		case "prompt":
			if row.ID != "noodle-prompt" || !p.receipt.PromptSent || p.receipt.Disposition != "" {
				return nil, nil, fmt.Errorf("Pi prompt response ID or sequence mismatch")
			}
			var data struct {
				Disposition string `json:"disposition"`
			}
			if err := json.Unmarshal(row.Data, &data); err != nil {
				return nil, nil, err
			}
			p.receipt.Disposition = data.Disposition
			if data.Disposition != "started" {
				return nil, nil, fmt.Errorf("Pi prompt disposition %q did not start task", data.Disposition)
			}
			close(p.ackReady)
		default:
			return nil, nil, fmt.Errorf("unexpected Pi response %q", row.Command)
		}
		if err := p.closeIfSettled(); err != nil {
			return nil, nil, err
		}
		return line, nil, p.persist()
	}
	if row.Type != "session" && !p.receipt.PromptSent {
		return nil, nil, fmt.Errorf("Pi event %q before verified prompt", row.Type)
	}
	stamped, events, err := processor.ProcessLine(line)
	if err != nil {
		return nil, nil, err
	}
	for _, ev := range events {
		if ev.Type == parse.EventError && strings.HasPrefix(ev.Message, "canonical routing failed:") {
			return nil, nil, fmt.Errorf("%s", ev.Message)
		}
	}
	if row.Type == "agent_settled" {
		if p.receipt.Settled {
			return nil, nil, fmt.Errorf("duplicate Pi agent_settled")
		}
		p.receipt.Settled = true
		for _, ev := range events {
			if ev.Type == parse.EventResult {
				p.receipt.Result = true
			}
			if ev.Type == parse.EventError {
				p.receipt.Error = ev.Message
			}
		}
		if !p.receipt.Result && p.receipt.Error == "" {
			p.receipt.Error = "Pi settled without successful result"
		}
		if err := p.closeIfSettled(); err != nil {
			return nil, nil, err
		}
		if err := p.persist(); err != nil {
			return nil, nil, err
		}
	}
	return stamped, events, nil
}

func (p *piRPC) closeIfSettled() error {
	if p.receipt.Settled && p.receipt.Disposition == "started" && !p.receipt.StdinClosed {
		if err := p.process.Stdin().Close(); err != nil {
			return fmt.Errorf("close Pi RPC stdin: %w", err)
		}
		p.receipt.StdinClosed = true
	}
	return nil
}

func (s *processSession) processPiStream() {
	raw, err := os.Create(filepath.Join(s.pi.dir, "pi-stdout.jsonl"))
	if err != nil {
		s.pi.fail(err)
		return
	}
	defer raw.Close()
	stamped, err := os.Create(s.stampedPath)
	if err != nil {
		s.pi.fail(err)
		return
	}
	defer stamped.Close()
	canonical, err := os.Create(s.canonicalPath)
	if err != nil {
		s.pi.fail(err)
		return
	}
	defer canonical.Close()
	scanner := bufio.NewScanner(io.TeeReader(s.process.Stdout(), raw))
	scanner.Buffer(make([]byte, scannerInitialBuffer), scannerMaxBuffer)
	processor := stamp.NewProcessor()
	for scanner.Scan() {
		line, events, err := s.pi.observe(scanner.Bytes(), processor)
		if err != nil {
			s.pi.fail(err)
			continue
		}
		if len(line) > 0 {
			if _, err := stamped.Write(append(line, '\n')); err != nil {
				s.pi.fail(err)
				continue
			}
		}
		for _, ev := range events {
			encoded, err := json.Marshal(ev)
			if err == nil {
				_, err = canonical.Write(append(encoded, '\n'))
			}
			if err != nil {
				s.pi.fail(err)
				break
			}
			s.consumeCanonicalLine(encoded, s.processHook)
		}
	}
	if err := scanner.Err(); err != nil {
		s.pi.fail(fmt.Errorf("read Pi stdout: %w", err))
	}
	// EOF is not agent_settled, and a closed stdout is not process exit.
	s.pi.mu.Lock()
	complete := s.pi.receipt.Settled && s.pi.receipt.Disposition == "started"
	s.pi.mu.Unlock()
	if !complete {
		s.pi.fail(fmt.Errorf("Pi stdout closed before settled/started"))
	}
}

func (p *piRPC) outcome(exitCode int, cancelled, killed bool) SessionOutcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := SessionOutcome{Status: StatusFailed, ExitCode: exitCode, Reason: p.receipt.Error}
	switch {
	case cancelled:
		out.Status, out.Reason = StatusCancelled, "Pi context cancelled"
	case killed:
		out.Status, out.Reason = StatusKilled, "Pi process killed"
	case out.Reason != "":
	case exitCode != 0:
		out.Reason = fmt.Sprintf("Pi process exited with code %d", exitCode)
	case !p.receipt.Settled || !p.receipt.Result || p.receipt.Disposition != "started" || !p.receipt.StdinClosed:
		out.Reason = "Pi completion evidence missing"
	default:
		out.Status, out.Reason, out.HasDeliverable = StatusCompleted, "Pi settled and process exited", true
	}
	return out
}
