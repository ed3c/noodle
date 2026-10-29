package dispatcher

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/skill"
	wt "github.com/poteto/noodle/worktree"
)

// ProcessDispatcherConfig configures a process dispatcher.
type ProcessDispatcherConfig struct {
	Execution       *execution.Context
	ProjectDir      string
	RuntimeDir      string
	NoodleBin       string
	SkillResolver   skill.Resolver
	ProviderConfigs ProviderConfigs
	RuntimeDefault  string // command template from config, empty = built-in
	RuntimeKind     string // runtime kind this dispatcher instance services
	Sink            SessionEventSink
}

// ProcessDispatcher dispatches provider sessions as direct child processes
// with bidirectional pipes. Replaces the tmux-based dispatcher.
type ProcessDispatcher struct {
	execution       *execution.Context
	projectDir      string
	runtimeDir      string
	noodleBin       string
	skillResolver   skill.Resolver
	providerConfigs ProviderConfigs
	runtimeDefault  string
	runtimeKind     string
	sink            SessionEventSink
}

func NewProcessDispatcher(config ProcessDispatcherConfig) *ProcessDispatcher {
	if config.Execution == nil {
		config.Execution = execution.Capture(config.ProjectDir)
	}
	return &ProcessDispatcher{
		execution:       config.Execution,
		projectDir:      strings.TrimSpace(config.ProjectDir),
		runtimeDir:      strings.TrimSpace(config.RuntimeDir),
		noodleBin:       strings.TrimSpace(config.NoodleBin),
		skillResolver:   config.SkillResolver,
		providerConfigs: config.ProviderConfigs,
		runtimeDefault:  strings.TrimSpace(config.RuntimeDefault),
		runtimeKind:     NormalizeRuntime(config.RuntimeKind),
		sink:            config.Sink,
	}
}

func (d *ProcessDispatcher) Dispatch(ctx context.Context, req DispatchRequest) (Session, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	harness, err := selectHarness(req.Provider)
	if err != nil {
		return nil, err
	}
	if harness == harnessPi {
		if err := d.validatePi(req); err != nil {
			return nil, err
		}
	}
	if req.RequireGroupExit && runtime.GOOS == "windows" {
		return nil, fmt.Errorf("worker process-group exit observation unavailable on Windows")
	}
	reqRuntime := strings.TrimSpace(req.Runtime)
	if reqRuntime == "" {
		reqRuntime = d.runtimeKind
	} else {
		reqRuntime = NormalizeRuntime(reqRuntime)
	}
	if reqRuntime != d.runtimeKind {
		return nil, fmt.Errorf("runtime %q not configured", reqRuntime)
	}
	req.Runtime = reqRuntime

	worktree, err := d.resolveWorktreeForDispatch(req)
	if err != nil {
		return nil, err
	}
	req.WorktreePath = worktree

	if d.runtimeDir == "" {
		return nil, fmt.Errorf("runtime directory not set")
	}
	req, err = d.resolveCodexResume(req)
	if err != nil {
		return nil, err
	}

	sessionID, err := generateSessionID(req.Name)
	if err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}

	sessionDir, promptPath, stampedPath, canonicalPath, stderrPath := sessionPaths(d.runtimeDir, sessionID)
	eventWriter, skillBundle, systemPrompt, composedPrompt, err := d.prepareSessionDir(sessionID, sessionDir, promptPath, req)
	if err != nil {
		return nil, err
	}

	launch := d.planProcessLaunch(harness, req, systemPrompt)
	var pi *piRPC
	if harness == harnessPi {
		pi, err = newPiRPC(sessionDir, req.Model, composedPrompt)
		if err != nil {
			return nil, err
		}
		launch.args = append(launch.args, "--session-dir", filepath.Join(sessionDir, "pi"))
	}
	var startup *codexStartup
	if launch.input == inputPromptEOF {
		startup, err = newCodexStartup(sessionDir, sessionID, len(composedPrompt))
		if err != nil {
			return nil, err
		}
	}
	stderrDone := make(chan struct{})
	controller, process, err := d.startSessionProcess(ctx, req, launch, composedPrompt, sessionID, sessionDir, stderrPath, stderrDone, startup, pi)
	if err != nil {
		return nil, err
	}

	session := newProcessSession(processSessionConfig{
		id:               sessionID,
		process:          process,
		eventWriter:      eventWriter,
		canonicalPath:    canonicalPath,
		stampedPath:      stampedPath,
		prompt:           req.Prompt,
		warnings:         skillBundle.Warnings,
		controller:       controller,
		sink:             d.sink,
		provider:         req.Provider,
		stderrPath:       stderrPath,
		stderrDone:       stderrDone,
		startup:          startup,
		pi:               pi,
		requireGroupExit: req.RequireGroupExit,
	})
	session.start(ctx)
	return session, nil
}

// resolveWorktreeForDispatch validates or resolves the worktree path for a
// dispatch request. Primary checkout is allowed when explicitly opted in;
// otherwise we require a linked worktree.
func (d *ProcessDispatcher) resolveWorktreeForDispatch(req DispatchRequest) (string, error) {
	if req.AllowPrimaryCheckout {
		path := strings.TrimSpace(req.WorktreePath)
		if path == "" {
			path = d.projectDir
		}
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("project directory not set")
		}
		return d.execution.ResolvePath(path)
	}
	validWorktree, err := wt.ValidateLinkedCheckoutWithExecution(req.WorktreePath, d.execution)
	if err != nil {
		return "", fmt.Errorf("worktree enforcement: %w", err)
	}
	return validWorktree, nil
}

// prepareSessionDir creates the session directory, resolves skills, composes
// prompts, and writes prompt files. Returns all artifacts needed for process
// startup.
func (d *ProcessDispatcher) prepareSessionDir(
	sessionID, sessionDir, promptPath string, req DispatchRequest,
) (*event.EventWriter, loadedSkill, string, string, error) {
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return nil, loadedSkill{}, "", "", fmt.Errorf("create session directory: %w", err)
	}
	eventWriter, err := event.NewEventWriter(d.runtimeDir, sessionID)
	if err != nil {
		return nil, loadedSkill{}, "", "", fmt.Errorf("create event writer: %w", err)
	}

	skillBundle, err := resolveSkillBundle(d.skillResolver, req)
	if err != nil {
		return nil, loadedSkill{}, "", "", err
	}

	preamble := buildSessionPreamble(req)
	systemPrompt, composedPrompt := composePrompts(req.Provider, req.Prompt, preamble, skillBundle.SystemPrompt)

	if _, err := writePromptFiles(sessionDir, promptPath, req.Prompt, composedPrompt); err != nil {
		return nil, loadedSkill{}, "", "", err
	}
	return eventWriter, skillBundle, systemPrompt, composedPrompt, nil
}

// startSessionProcess builds the command, starts the child process, configures
// the controller for stdin steering, drains stderr, and writes metadata.
func (d *ProcessDispatcher) startSessionProcess(
	ctx context.Context, req DispatchRequest, launch processLaunch,
	composedPrompt, sessionID, sessionDir, stderrPath string,
	stderrDone chan struct{},
	startup *codexStartup, pi *piRPC,
) (*claudeController, *ProcessHandle, error) {
	cmd, err := d.buildCmd(req, launch)
	if err != nil {
		return nil, nil, fmt.Errorf("build command: %w", err)
	}
	cmd.Env = append(buildDispatchEnvFrom(req, cmd.Env), "NOODLE_SESSION_ID="+sessionID)

	process, err := startProcess(cmd, req.RequireGroupExit)
	if err != nil {
		return nil, nil, fmt.Errorf("start process: %w", err)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = process.ForceKill()
			<-process.Done()
			_ = process.Stdin().Close()
			_ = process.Stdout().Close()
			_ = process.Stderr().Close()
			process.ioWG.Wait()
		}
	}()

	controller, err := d.configureStdin(ctx, process, launch.input, composedPrompt, startup)
	if err != nil {
		_ = process.ForceKill()
		_ = process.Stdout().Close()
		_ = process.Stderr().Close()
		return nil, nil, err
	}

	if pi != nil {
		pi.process = process
	}
	process.ioWG.Add(1)
	go func() {
		defer process.ioWG.Done()
		defer close(stderrDone)
		if err := drainToFile(startup.reader(process.Stderr(), true), stderrPath); err != nil && pi != nil {
			pi.fail(fmt.Errorf("retain Pi stderr: %w", err))
		}
	}()

	if err := WriteProcessMetadata(sessionDir, sessionID, process.PID(), nowUTC()); err != nil {
		_ = process.ForceKill()
		_ = process.Stdout().Close()
		_ = process.Stderr().Close()
		return nil, nil, fmt.Errorf("write process metadata: %w", err)
	}
	if err := writeDispatchMetadata(d.runtimeDir, sessionID, req, nowUTC()); err != nil {
		_ = process.ForceKill()
		_ = process.Stdout().Close()
		_ = process.Stderr().Close()
		return nil, nil, fmt.Errorf("write spawn metadata: %w", err)
	}
	handedOff = true
	return controller, process, nil
}

// configureStdin sets up stdin handling for the started process. For Claude,
// creates a controller that owns stdin for live steering and sends the prompt
// as the first stream-json user message. For Codex, writes
// the prompt to stdin and closes it.
func (d *ProcessDispatcher) configureStdin(
	ctx context.Context, process *ProcessHandle, input processInput, composedPrompt string,
	startup *codexStartup,
) (*claudeController, error) {
	if input == inputPiRPC {
		return nil, nil
	}
	if input == inputPromptEOF {
		process.ioWG.Add(1)
		go func() {
			defer process.ioWG.Done()
			startup.writeInput(process.Stdin(), composedPrompt)
		}()
		return nil, nil
	}
	controller := newClaudeController(process.Stdin())
	if err := controller.SendMessage(ctx, composedPrompt); err != nil {
		return nil, fmt.Errorf("send initial prompt: %w", err)
	}
	return controller, nil
}

// buildCmd constructs an exec.Cmd for the provider. The stamp processor runs
// in-process via processSession.processStream, so no shell pipeline is needed.
func (d *ProcessDispatcher) buildCmd(req DispatchRequest, launch processLaunch) (*exec.Cmd, error) {
	runtimeCmd := strings.TrimSpace(d.runtimeDefault)
	if runtimeCmd != "" {
		return d.buildCustomRuntimeCmd(req, runtimeCmd)
	}
	return d.execution.Command(context.Background(), req.WorktreePath, launch.binary, launch.args...), nil
}

// buildCustomRuntimeCmd handles user-provided runtime command templates.
func (d *ProcessDispatcher) buildCustomRuntimeCmd(req DispatchRequest, runtimeCmd string) (*exec.Cmd, error) {
	vars := map[string]string{
		"repo":   req.WorktreePath,
		"prompt": filepath.Join(d.runtimeDir, "sessions", req.Name, "input.txt"),
		"skill":  req.Skill,
		"brief":  filepath.Join(d.runtimeDir, "mise.json"),
	}
	resolved := resolveTemplateVars(runtimeCmd, vars)
	return d.execution.Command(context.Background(), req.WorktreePath, "sh", "-c", resolved), nil
}

// drainToFile reads from r and writes to the named file.
func drainToFile(r io.ReadCloser, path string) error {
	if r == nil || strings.TrimSpace(path) == "" {
		return nil
	}
	defer r.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

// replaceFlag removes old from args and appends the replacement key-value pair.
func replaceFlag(args []string, old string, newKey, newVal string) []string {
	out := make([]string, 0, len(args)+1)
	for _, a := range args {
		if a != old {
			out = append(out, a)
		}
	}
	return append(out, newKey, newVal)
}

var _ Dispatcher = (*ProcessDispatcher)(nil)
