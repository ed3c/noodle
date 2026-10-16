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
	"github.com/poteto/noodle/internal/stringx"
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

	sessionID, err := generateSessionID(req.Name)
	if err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}

	sessionDir, promptPath, stampedPath, canonicalPath, stderrPath := sessionPaths(d.runtimeDir, sessionID)
	eventWriter, skillBundle, systemPrompt, composedPrompt, err := d.prepareSessionDir(sessionID, sessionDir, promptPath, req)
	if err != nil {
		return nil, err
	}

	var startup *codexStartup
	if stringx.Normalize(req.Provider) == "codex" {
		startup, err = newCodexStartup(sessionDir, sessionID, len(composedPrompt))
		if err != nil {
			return nil, err
		}
	}
	stderrDone := make(chan struct{})
	controller, process, err := d.startSessionProcess(ctx, req, systemPrompt, composedPrompt, sessionID, sessionDir, stderrPath, stderrDone, startup)
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
	ctx context.Context, req DispatchRequest,
	systemPrompt, composedPrompt, sessionID, sessionDir, stderrPath string,
	stderrDone chan struct{},
	startup *codexStartup,
) (*claudeController, *ProcessHandle, error) {
	cmd, err := d.buildCmd(req, systemPrompt)
	if err != nil {
		return nil, nil, fmt.Errorf("build command: %w", err)
	}
	cmd.Env = append(buildDispatchEnvFrom(req, cmd.Env), "NOODLE_SESSION_ID="+sessionID)

	process, err := StartProcess(cmd)
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

	controller, err := d.configureStdin(ctx, process, req.Provider, composedPrompt, startup)
	if err != nil {
		_ = process.ForceKill()
		_ = process.Stdout().Close()
		_ = process.Stderr().Close()
		return nil, nil, err
	}

	process.ioWG.Add(1)
	go func() {
		defer process.ioWG.Done()
		defer close(stderrDone)
		drainToFile(startup.reader(process.Stderr(), true), stderrPath)
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
// as the first stream-json user message. For other providers (codex), writes
// the prompt to stdin and closes it.
func (d *ProcessDispatcher) configureStdin(
	ctx context.Context, process *ProcessHandle, provider, composedPrompt string,
	startup *codexStartup,
) (*claudeController, error) {
	if stringx.Normalize(provider) == "codex" {
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
func (d *ProcessDispatcher) buildCmd(req DispatchRequest, systemPrompt string) (*exec.Cmd, error) {
	runtimeCmd := strings.TrimSpace(d.runtimeDefault)
	if runtimeCmd != "" {
		return d.buildCustomRuntimeCmd(req, runtimeCmd)
	}

	provider := stringx.Normalize(req.Provider)
	binary := d.resolveAgentBinary(provider)
	extraArgs := d.resolveExtraArgs(provider)

	switch provider {
	case "codex":
		args := codexBaseArgs(req)
		args = append(args, extraArgs...)
		return d.execution.Command(context.Background(), req.WorktreePath, binary, args...), nil

	default: // claude
		args := claudeBaseArgs(req, systemPrompt)
		// Replace -p (pipe mode) with --input-format stream-json for
		// bidirectional streaming. The prompt is sent as a user message
		// through the claudeController rather than piped to stdin.
		args = replaceFlag(args, "-p", "--input-format", "stream-json")
		args = append(args, extraArgs...)
		return d.execution.Command(context.Background(), req.WorktreePath, binary, args...), nil
	}
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

func (d *ProcessDispatcher) resolveAgentBinary(provider string) string {
	switch stringx.Normalize(provider) {
	case "codex":
		if path := strings.TrimSpace(d.providerConfigs.Codex.Path); path != "" {
			resolved, _ := d.execution.ResolvePath(path)
			candidate := filepath.Join(resolved, "codex")
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		return "codex"
	default:
		if path := strings.TrimSpace(d.providerConfigs.Claude.Path); path != "" {
			resolved, _ := d.execution.ResolvePath(path)
			candidate := filepath.Join(resolved, "claude")
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		return "claude"
	}
}

func (d *ProcessDispatcher) resolveExtraArgs(provider string) []string {
	switch stringx.Normalize(provider) {
	case "codex":
		return d.providerConfigs.Codex.Args
	default:
		return d.providerConfigs.Claude.Args
	}
}

// drainToFile reads from r and writes to the named file.
func drainToFile(r io.ReadCloser, path string) {
	if r == nil || strings.TrimSpace(path) == "" {
		return
	}
	defer r.Close()
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = io.Copy(f, r)
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
