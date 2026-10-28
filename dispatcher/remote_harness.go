package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/instructionbundle"
	"github.com/poteto/noodle/skill"
)

// HarnessProcess supplies native streams and execution termination. Its Done
// concerns execution only; the runtime separately retains VM/resource custody.
type HarnessProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Done() <-chan struct{}
	ExitCode() (int, bool)
	Terminate() error
	ForceKill() error
	IOWaitGroup() *sync.WaitGroup
	WaitProblems() <-chan error
}

func (h *ProcessHandle) IOWaitGroup() *sync.WaitGroup { return &h.ioWG }
func (h *ProcessHandle) WaitProblems() <-chan error   { return h.waitProblem }

// RemoteHarness prepares local evidence before the remote runtime can launch.
// The selected runtime owns the guest binary and isolation policy.
type RemoteHarness struct {
	ID                                             string
	Directory                                      string
	argv                                           []string
	GuestSessionDir                                string
	Instructions                                   *instructionbundle.Prepared
	req                                            DispatchRequest
	runtimeDir, prompt, stamped, canonical, stderr string
	writer                                         *event.EventWriter
	warnings                                       []string
	pi                                             *piRPC
	startup                                        *codexStartup
	mu                                             sync.Mutex
	started                                        bool
}

func PrepareRemoteHarness(runtimeDir string, resolver skill.Resolver, req DispatchRequest, binary, guestSessionRoot, instructionVerifier string) (*RemoteHarness, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.Runtime != "openshell" || (req.Provider != "codex" && req.Provider != "pi") {
		return nil, fmt.Errorf("remote harness/runtime combination unsupported")
	}
	if req.ResumeSessionID != "" {
		return nil, fmt.Errorf("OpenShell native session resume is not supported; inspect the stopped attempt before an explicit new attempt")
	}
	if req.MaxTurns != 0 || req.BudgetCap != 0 {
		return nil, fmt.Errorf("remote harness turn/budget limit unsupported")
	}
	if !path.IsAbs(binary) || !path.IsAbs(guestSessionRoot) {
		return nil, fmt.Errorf("remote harness binary/session directory not absolute")
	}
	if req.InstructionBundle != nil && (!path.IsAbs(instructionVerifier) || path.Clean(instructionVerifier) != instructionVerifier) {
		return nil, fmt.Errorf("remote instruction verifier missing or invalid")
	}
	id, err := generateSessionID(req.Name)
	if err != nil {
		return nil, err
	}
	dir, promptPath, stamped, canonical, stderr := sessionPaths(runtimeDir, id)
	d := NewProcessDispatcher(ProcessDispatcherConfig{RuntimeDir: runtimeDir, SkillResolver: resolver, RuntimeKind: "process"})
	if req.Provider == "pi" {
		local := req
		local.Runtime = "process"
		if err = d.validatePi(local); err != nil {
			return nil, err
		}
	}
	writer, bundle, _, prompt, err := d.prepareSessionDir(id, dir, promptPath, req)
	if err != nil {
		return nil, err
	}
	p := &RemoteHarness{ID: id, Directory: dir, GuestSessionDir: path.Join(guestSessionRoot, id), req: req, runtimeDir: runtimeDir, prompt: prompt, stamped: stamped, canonical: canonical, stderr: stderr, writer: writer, warnings: bundle.Warnings}
	if req.InstructionBundle != nil {
		p.Instructions, err = instructionbundle.Prepare(context.Background(), *req.InstructionBundle, req.EnvVars["NOODLE_ORDER_ID"], dir)
		if err != nil {
			return nil, err
		}
		prompt += "\n\n" + p.Instructions.PromptSection()
		p.prompt = prompt
		if err = os.WriteFile(inputPath(dir), []byte(prompt), 0600); err != nil {
			return nil, err
		}
		mapping := struct {
			OriginalManifest InstructionBundleRef        `json:"original_manifest"`
			Prepared         *instructionbundle.Prepared `json:"prepared"`
		}{*req.InstructionBundle, p.Instructions}
		data, e := json.MarshalIndent(mapping, "", "  ")
		if e != nil {
			return nil, e
		}
		if err = os.WriteFile(filepath.Join(dir, "instruction-mapping.json"), data, 0600); err != nil {
			return nil, err
		}
	}
	if req.Provider == "codex" {
		p.argv = append([]string{binary}, codexBaseArgs(req)...)
		p.argv = append(p.argv, "--dangerously-bypass-approvals-and-sandbox")
		if req.ReasoningLevel != "" {
			switch req.ReasoningLevel {
			case "low", "medium", "high", "xhigh", "minimal", "none":
			default:
				return nil, fmt.Errorf("Codex reasoning level unsupported")
			}
			p.argv = append(p.argv, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.ReasoningLevel))
		}
		p.argv = append(p.argv, "-")
		p.startup, err = newCodexStartup(dir, id, len(prompt))
	} else {
		provider, model, _ := strings.Cut(req.Model, "/")
		p.argv = []string{binary, "--mode", "rpc", "--provider", provider, "--model", model, "--no-extensions", "--no-mcp", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", path.Join(p.GuestSessionDir, "pi")}
		if req.ReasoningLevel != "" {
			p.argv = append(p.argv, "--thinking", req.ReasoningLevel)
		}
		p.pi, err = newPiRPC(dir, req.Model, prompt)
		if err == nil {
			p.pi.nativeSessionDir = path.Join(p.GuestSessionDir, "pi")
		}
	}
	if err != nil {
		return nil, err
	}
	if p.Instructions != nil {
		p.argv = append([]string{instructionVerifier, "--directory", p.Instructions.GuestDirectory, "--sha256", p.Instructions.ManifestSHA256, "--order-id", req.EnvVars["NOODLE_ORDER_ID"], "--"}, p.argv...)
	}
	if err = writeDispatchMetadata(runtimeDir, id, req, nowUTC()); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *RemoteHarness) Command() []string { return slices.Clone(p.argv) }
func (p *RemoteHarness) Start(ctx context.Context, process HarnessProcess, sink SessionEventSink) (Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || process == nil {
		return nil, fmt.Errorf("remote harness already started or streams missing")
	}
	p.started = true
	stderrDone := make(chan struct{})
	process.IOWaitGroup().Add(1)
	go func() {
		defer process.IOWaitGroup().Done()
		defer close(stderrDone)
		if err := drainToFile(p.startup.reader(process.Stderr(), true), p.stderr); err != nil && p.pi != nil {
			p.pi.fail(err)
		}
	}()
	if p.pi != nil {
		p.pi.process = process
	} else {
		process.IOWaitGroup().Add(1)
		go func() { defer process.IOWaitGroup().Done(); p.startup.writeInput(process.Stdin(), p.prompt) }()
	}
	s := newProcessSession(processSessionConfig{id: p.ID, process: process, eventWriter: p.writer, canonicalPath: p.canonical, stampedPath: p.stamped, prompt: p.req.Prompt, warnings: p.warnings, sink: sink, provider: p.req.Provider, stderrPath: p.stderr, stderrDone: stderrDone, startup: p.startup, pi: p.pi})
	s.start(ctx)
	return s, nil
}

var _ HarnessProcess = (*ProcessHandle)(nil)
