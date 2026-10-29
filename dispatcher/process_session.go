package dispatcher

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/parse"
)

// processSession implements Session backed by a direct child process.
// It runs the stamp processor in-process (like spritesSession) rather than
// as a shell pipeline stage.
type processSession struct {
	sessionBase
	process          HarnessProcess
	controller       *claudeController // nil for non-steerable sessions
	warnings         []string
	provider         string
	stderrPath       string
	stderrDone       <-chan struct{}
	startup          *codexStartup
	pi               *piRPC
	killMu           sync.Mutex
	forceKilled      bool
	killed           chan struct{}
	joined           chan struct{}
	requireGroupExit bool
	completionExit   sync.Once
}

type processSessionConfig struct {
	id               string
	process          HarnessProcess
	eventWriter      *event.EventWriter
	canonicalPath    string
	stampedPath      string
	prompt           string
	warnings         []string
	controller       *claudeController // nil for non-steerable sessions
	sink             SessionEventSink
	provider         string
	stderrPath       string
	stderrDone       <-chan struct{}
	startup          *codexStartup
	pi               *piRPC
	requireGroupExit bool
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
		process:          cfg.process,
		controller:       cfg.controller,
		warnings:         append([]string(nil), cfg.warnings...),
		provider:         cfg.provider,
		stderrPath:       cfg.stderrPath,
		stderrDone:       cfg.stderrDone,
		startup:          cfg.startup,
		pi:               cfg.pi,
		joined:           make(chan struct{}),
		killed:           make(chan struct{}),
		requireGroupExit: cfg.requireGroupExit,
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
	for _, warning := range s.warnings {
		s.publish(SessionEvent{
			Type:      "warning",
			Message:   warning,
			Timestamp: nowUTC(),
		})
	}

	if s.pi != nil {
		s.pi.start(ctx, s.process)
	}
	s.wg.Add(2)

	go func() {
		defer s.wg.Done()
		defer s.closeStreamDone()
		defer s.process.Stdout().Close()
		if s.pi != nil {
			s.processPiStream()
			return
		}
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

}

func (s *processSession) processHook(ce parse.CanonicalEvent) {
	s.startup.observeInit(ce.Type)
	s.observeCanonicalEvent(ce)
	if s.controller != nil {
		s.controller.NotifyEvent(string(ce.Type))
	}
	if s.pi == nil && s.requireGroupExit && s.controller == nil && (ce.Type == parse.EventComplete || ce.Type == parse.EventResult) {
		s.completionExit.Do(func() {
			go func() {
				timer := time.NewTimer(2 * time.Second)
				defer timer.Stop()
				select {
				case <-s.process.Done():
					return
				case <-timer.C:
				}
				// One-shot completion is not process exit. Stop only our
				// original child; the existing waiter retains quota until exit.
				if err := s.process.Terminate(); err != nil {
					s.publish(SessionEvent{Type: "warning", Message: fmt.Sprintf("completed worker stop requires owner readback: %v", err), Timestamp: nowUTC()})
				}
			}()
		})
	}
}

func (s *processSession) waitForExit(ctx context.Context) {
	cancellation := ctx.Done()
wait:
	for {
		select {
		case <-s.process.Done():
			break wait
		case err := <-s.process.WaitProblems():
			s.publish(SessionEvent{Type: "warning", Message: fmt.Sprintf("original worker exit requires owner readback: %v", err), Timestamp: nowUTC()})
		case <-cancellation:
			if err := s.process.ForceKill(); err != nil {
				s.publish(SessionEvent{Type: "warning", Message: fmt.Sprintf("original worker stop requires owner readback: %v", err), Timestamp: nowUTC()})
			}
			cancellation = nil
		}
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
	go func() { s.process.IOWaitGroup().Wait(); close(ioDone) }()
	select {
	case <-ioDone:
	case <-ctx.Done():
		s.closeProcessPipes()
		<-ioDone
	case <-s.killed:
		s.closeProcessPipes()
		<-ioDone
	}
	s.startup.observeThread(s.stampedPath)
	s.killMu.Lock()
	killed := s.forceKilled
	s.killMu.Unlock()
	exitCode, _ := s.process.ExitCode()
	if s.pi != nil {
		outcome := s.pi.outcome(exitCode, ctx.Err() != nil, killed)
		s.doneOnce.Do(func() {
			s.mu.Lock()
			s.status, s.outcome = outcome.Status.String(), outcome
			s.mu.Unlock()
			close(s.done)
		})
		return
	}
	if killed {
		<-s.streamDone
		s.trackerMu.Lock()
		completed := s.sawComplete || s.sawResult
		s.trackerMu.Unlock()
		if !completed {
			s.markDone("killed")
			return
		}
	}
	s.resolveAndMarkDone(exitCode, ctx.Err() != nil)
}

func (s *processSession) closeProcessPipes() {
	_ = s.process.Stdout().Close()
	_ = s.process.Stderr().Close()
	_ = s.process.Stdin().Close()
}

func (s *processSession) Terminate() error {
	select {
	case <-s.joined:
		return nil
	default:
	}
	return s.process.Terminate()
}

func (s *processSession) ForceKill() error {
	select {
	case <-s.joined:
		return nil
	default:
	}
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
