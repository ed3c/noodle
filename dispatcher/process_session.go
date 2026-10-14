package dispatcher

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/parse"
)

// processSession implements Session backed by a direct child process.
// It runs the stamp processor in-process (like spritesSession) rather than
// as a shell pipeline stage.
type processSession struct {
	sessionBase
	process     *ProcessHandle
	controller  *claudeController // nil for non-steerable sessions
	warnings    []string
	provider    string
	stderrPath  string
	stderrDone  <-chan struct{}
	startup     *codexStartup
	killMu      sync.Mutex
	forceKilled bool
	killed      chan struct{}
	joined      chan struct{}
}

type processSessionConfig struct {
	id            string
	process       *ProcessHandle
	eventWriter   *event.EventWriter
	canonicalPath string
	stampedPath   string
	prompt        string
	warnings      []string
	controller    *claudeController // nil for non-steerable sessions
	sink          SessionEventSink
	provider      string
	stderrPath    string
	stderrDone    <-chan struct{}
	startup       *codexStartup
}

func newProcessSession(cfg processSessionConfig) *processSession {
	return &processSession{
		sessionBase: newSessionBase(sessionBaseConfig{
			id:            cfg.id,
			eventWriter:   cfg.eventWriter,
			stampedPath:   cfg.stampedPath,
			canonicalPath: cfg.canonicalPath,
			prompt:        cfg.prompt,
			sink:          cfg.sink,
		}),
		process:    cfg.process,
		controller: cfg.controller,
		warnings:   append([]string(nil), cfg.warnings...),
		provider:   cfg.provider,
		stderrPath: cfg.stderrPath,
		stderrDone: cfg.stderrDone,
		startup:    cfg.startup,
		joined:     make(chan struct{}),
		killed:     make(chan struct{}),
	}
}

// Outcome retains Codex's own pre-event launch diagnostic. Codex owns config
// validation; Noodle must not guess a replacement permission policy or hide the
// invalid field behind "no events emitted".
func (s *processSession) Outcome() SessionOutcome {
	outcome := s.sessionBase.Outcome()
	if !strings.EqualFold(strings.TrimSpace(s.provider), "codex") || outcome.Status != StatusFailed || outcome.Reason != "no events emitted" {
		return outcome
	}
	if s.stderrDone != nil {
		<-s.stderrDone
	}
	f, err := os.Open(s.stderrPath)
	if err != nil {
		return outcome
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8192))
	if err != nil {
		return outcome
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "error:") {
			outcome.Reason = "Codex process launch failed: " + line + "; owner: Codex exec configuration; next: codex exec --help"
			break
		}
	}
	return outcome
}

func (s *processSession) start(ctx context.Context) {
	s.writeInitialHeartbeat()
	s.wg.Add(2)

	go func() {
		defer s.wg.Done()
		defer s.closeStreamDone()
		defer s.process.Stdout().Close()
		interceptor := &canonicalLineInterceptor{onLine: func(line []byte) {
			s.consumeCanonicalLine(line, s.processHook)
		}}
		s.processStream(ctx, s.startup.reader(s.process.Stdout(), false), interceptor)
	}()

	go func() {
		defer s.wg.Done()
		s.waitForExit(ctx)
	}()

	go func() { s.closeEventsWhenDone(); close(s.joined) }()

	for _, warning := range s.warnings {
		s.publish(SessionEvent{
			Type:      "warning",
			Message:   warning,
			Timestamp: nowUTC(),
		})
	}
}

func (s *processSession) processHook(ce parse.CanonicalEvent) {
	s.startup.observeInit(ce.Type)
	s.observeCanonicalEvent(ce)
	if s.controller != nil {
		s.controller.NotifyEvent(string(ce.Type))
	}
}

func (s *processSession) waitForExit(ctx context.Context) {
	select {
	case <-s.process.Done():
	case <-ctx.Done():
		_ = s.process.ForceKill()
		<-s.process.Done()
	}

	// Keep draining after a normal exit, but cancellation or a forced kill
	// must unblock pipes retained by descendants as well.
	select {
	case <-s.streamDone:
	case <-ctx.Done():
		s.closeProcessPipes()
	case <-s.killed:
		s.closeProcessPipes()
	}
	ioDone := make(chan struct{})
	go func() { s.process.ioWG.Wait(); close(ioDone) }()
	select {
	case <-ioDone:
	case <-ctx.Done():
		s.closeProcessPipes()
		<-ioDone
	case <-s.killed:
		s.closeProcessPipes()
		<-ioDone
	}
	s.killMu.Lock()
	killed := s.forceKilled
	s.killMu.Unlock()
	exitCode, _ := s.process.ExitCode()
	if killed {
		<-s.streamDone
		s.markDone("killed")
		return
	}
	s.resolveAndMarkDone(exitCode, ctx.Err() != nil)
}

func (s *processSession) closeProcessPipes() {
	_ = s.process.Stdout().Close()
	_ = s.process.Stderr().Close()
	_ = s.process.Stdin().Close()
}

func (s *processSession) Terminate() error {
	return s.process.Terminate()
}

func (s *processSession) ForceKill() error {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	err := s.process.ForceKill()
	if err != nil {
		return err
	}
	if !s.forceKilled {
		s.forceKilled = true
		close(s.killed)
	}
	return nil
}

func (s *processSession) Done() <-chan struct{} { return s.joined }

func (s *processSession) Controller() AgentController {
	if s.controller != nil {
		return s.controller
	}
	return noopController{}
}

var _ Session = (*processSession)(nil)
