package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/parse"
)

const CodexNativeRuntime = "codex-app-server"

// NativeCodexRecord belongs to the existing Noodle attempt, not an OS PID.
type NativeCodexRecord struct {
	ServerRequests    map[string]*NativeServerRequest `json:"server_requests,omitempty"`
	NativeHome        string                          `json:"native_home"`
	Foreground        nativeProcessIdentity           `json:"foreground"`
	ObservedChildren  []nativeProcessIdentity         `json:"observed_children"`
	UnsubscribeIntent bool                            `json:"unsubscribe_intent"`
	UnsubscribeACK    json.RawMessage                 `json:"unsubscribe_ack,omitempty"`
	UnloadReadback    json.RawMessage                 `json:"unload_readback,omitempty"`
	ServerExit        *int                            `json:"server_exit,omitempty"`
	Schema            int                             `json:"schema"`
	SessionID         string                          `json:"session_id"`
	Socket            string                          `json:"socket"`
	SocketID          string                          `json:"socket_id"`
	ThreadID          string                          `json:"thread_id,omitempty"`
	Source            json.RawMessage                 `json:"source,omitempty"`
	TurnID            string                          `json:"turn_id,omitempty"`
	Worktree          string                          `json:"worktree"`
	TaskRole          string                          `json:"task_role"`
	TaskKey           string                          `json:"task_key"`
	OrderID           string                          `json:"order_id"`
	Model             string                          `json:"model"`
	State             string                          `json:"state"`
	Terminal          string                          `json:"terminal,omitempty"`
	SwapArchived      bool                            `json:"swap_archived"`
	ArchiveIntent     bool                            `json:"archive_intent"`
	ArchiveACK        json.RawMessage                 `json:"archive_ack,omitempty"`
	UnarchiveACK      json.RawMessage                 `json:"unarchive_ack,omitempty"`
	ChildRestores     map[string]json.RawMessage      `json:"child_restores,omitempty"`
	StopACK           json.RawMessage                 `json:"stop_ack,omitempty"`
	StopRefusal       json.RawMessage                 `json:"stop_refusal,omitempty"`
	StopRequested     bool                            `json:"stop_requested"`
	Children          map[string]*NativeCodexChild    `json:"children,omitempty"`
	Problem           string                          `json:"problem,omitempty"`
}

type CodexNativeDispatcher struct{ prepare *ProcessDispatcher }

func NewCodexNativeDispatcher(cfg ProcessDispatcherConfig) *CodexNativeDispatcher {
	cfg.RuntimeKind = CodexNativeRuntime
	return &CodexNativeDispatcher{prepare: NewProcessDispatcher(cfg)}
}

func (d *CodexNativeDispatcher) Dispatch(ctx context.Context, req DispatchRequest) (Session, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.Provider != "codex" || NormalizeRuntime(req.Runtime) != CodexNativeRuntime || req.RequireGroupExit {
		return nil, fmt.Errorf("native Codex runtime requires its selected Codex request, not process-group custody")
	}
	config, err := nativeCodexConfig(d.prepare.providerConfigs.Codex.Args)
	if err != nil {
		return nil, err
	}
	worktree, err := d.prepare.resolveWorktreeForDispatch(req)
	if err != nil {
		return nil, err
	}
	req.WorktreePath = worktree
	if req.ReasoningLevel != "" {
		config["model_reasoning_effort"] = req.ReasoningLevel
	}
	environment := buildDispatchEnvFrom(req, d.prepare.execution.Environ())
	home, err := nativeCodexHome(environment)
	if err != nil {
		return nil, err
	}
	id, err := generateSessionID(req.Name)
	if err != nil {
		return nil, err
	}
	workerEnv := map[string]string{"NOODLE_SESSION_ID": id}
	for _, entry := range buildDispatchEnvFrom(req, nil) {
		key, value, _ := strings.Cut(entry, "=")
		workerEnv[key] = value
	}
	config["shell_environment_policy.set"] = workerEnv
	dir, promptPath, rawPath, canonicalPath, _ := sessionPaths(d.prepare.runtimeDir, id)
	writer, _, _, prompt, err := d.prepare.prepareSessionDir(id, dir, promptPath, req)
	if err != nil {
		return nil, err
	}
	if err := writeDispatchMetadata(d.prepare.runtimeDir, id, req, nowUTC()); err != nil {
		return nil, err
	}
	s := &nativeCodexSession{sessionBase: newSessionBase(sessionBaseConfig{
		id: id, eventWriter: writer, stampedPath: rawPath, canonicalPath: canonicalPath,
		prompt: req.Prompt, sink: d.prepare.sink,
	}), dir: dir, terminal: make(chan struct{}, 1), release: make(chan struct{}, 1),
		record: NativeCodexRecord{Schema: 2, SessionID: id, NativeHome: home, Worktree: worktree,
			TaskRole: req.TaskRole, TaskKey: req.TaskKey, OrderID: req.EnvVars["NOODLE_ORDER_ID"], Model: req.Model, State: "prepared"}}
	if err := s.persist(); err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(filepath.Join(dir, "stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	client, process, drained, err := startCodexStdio(ctx, d.prepare.planProcessLaunch(harnessCodex, req, "").binary, worktree, environment, stderr, s.observe)
	s.client, s.process = client, process
	if process != nil {
		evidenceDone := make(chan error, 1)
		s.stderrDone = evidenceDone
		go func() {
			copyErr := <-drained
			evidenceDone <- errors.Join(copyErr, stderr.Close())
			close(evidenceDone)
		}()
	}
	if process == nil {
		stderr.Close()
		return nil, err
	}
	if err != nil {
		s.unknown(err)
		return s, nil
	}
	snapshot, snapshotErr := nativeProcessSnapshot(ctx)
	if snapshotErr != nil {
		s.unknown(snapshotErr)
		return s, nil
	}
	original, ok := snapshot[process.PID()]
	if !ok {
		s.unknown(fmt.Errorf("foreground process disappeared during binding"))
		return s, nil
	}
	s.nativeMu.Lock()
	s.record.Foreground = original
	err = s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		s.unknown(err)
		return s, nil
	}
	params := map[string]any{"cwd": worktree, "model": req.Model, "config": config}
	method := "thread/start"
	if req.ResumeSessionID != "" {
		previous, err := readNativePredecessor(d.prepare.runtimeDir, req, home)
		if err != nil {
			s.unknown(err)
			return s, nil
		}
		raw, err := os.ReadFile(filepath.Join(d.prepare.runtimeDir, "sessions", req.ResumeSessionID, "raw.ndjson"))
		if err != nil {
			s.unknown(err)
			return s, nil
		}
		req.codexThreadID = previous.ThreadID
		req.codexThreadSourceSHA = fmt.Sprintf("%x", sha256.Sum256(raw))
		if err := writeDispatchMetadata(d.prepare.runtimeDir, id, req, nowUTC()); err != nil {
			s.unknown(err)
			return s, nil
		}
		s.nativeMu.Lock()
		s.record.ThreadID, s.record.TurnID, s.record.Terminal = previous.ThreadID, previous.TurnID, previous.Terminal
		s.nativeMu.Unlock()
		if err := s.originalTerminal(ctx); err != nil {
			s.unknown(err)
			return s, nil
		}
		s.nativeMu.Lock()
		s.record.TurnID, s.record.Terminal = "", ""
		err = s.persist()
		s.nativeMu.Unlock()
		if err != nil {
			s.unknown(err)
			return s, nil
		}
		params["threadId"] = previous.ThreadID
		method = "thread/resume"
	}
	s.nativeMu.Lock()
	s.record.TurnID, s.record.Terminal, s.record.State = "", "", "thread_intent"
	err = s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		s.unknown(err)
		return s, nil
	}
	var started struct {
		Thread struct {
			ID     string
			Source json.RawMessage
		}
	}
	if err := client.call(ctx, method, params, &started); err != nil {
		s.unknown(err)
		return s, nil // Retain admission: a missing response is not a failed dispatch.
	}
	s.nativeMu.Lock()
	originalThread := s.record.ThreadID
	s.nativeMu.Unlock()
	if !codexThreadUUID.MatchString(started.Thread.ID) || len(started.Thread.Source) == 0 || string(started.Thread.Source) == "null" || (originalThread != "" && originalThread != started.Thread.ID) {
		s.unknown(fmt.Errorf("original native thread identity conflicts"))
		return s, nil
	}
	s.nativeMu.Lock()
	s.record.ThreadID, s.record.State = started.Thread.ID, "turn_intent"
	s.record.Source = started.Thread.Source
	err = s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		s.unknown(err)
		return s, nil
	}
	var turn struct{ Turn struct{ ID string } }
	if err := client.call(ctx, "turn/start", map[string]any{
		"threadId": started.Thread.ID, "input": []map[string]string{{"type": "text", "text": prompt}},
	}, &turn); err != nil {
		s.unknown(err)
		return s, nil
	}
	s.nativeMu.Lock()
	if s.record.TurnID != "" && s.record.TurnID != turn.Turn.ID {
		err = fmt.Errorf("native turn response conflicts with original event")
	} else if turn.Turn.ID == "" {
		err = fmt.Errorf("native turn identity missing")
	} else {
		s.record.TurnID = turn.Turn.ID
		if s.record.Terminal == "" {
			s.record.State = "running"
		}
		err = s.persist()
	}
	s.nativeMu.Unlock()
	if err != nil {
		s.unknown(err)
		return s, nil
	}
	s.startCompletion()
	return s, nil
}

func nativeCodexHome(env []string) (string, error) {
	home, base := "", ""
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "CODEX_HOME="); ok {
			home = value
		}
		if value, ok := strings.CutPrefix(entry, "HOME="); ok {
			base = value
		}
	}
	if home == "" {
		if base == "" {
			return "", errors.New("native context HOME source is missing")
		}
		home = filepath.Join(base, ".codex")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("native CODEX_HOME is not absolute")
	}
	return filepath.Clean(home), nil
}

func nativeCodexConfig(args []string) (map[string]any, error) {
	config := map[string]any{}
	for len(args) != 0 {
		if len(args) < 2 {
			return nil, fmt.Errorf("native Codex carrier argument needs its original value: %s", args[0])
		}
		switch args[0] {
		case "--sandbox":
			config["sandbox_mode"] = args[1]
		case "--ask-for-approval", "-a":
			config["approval_policy"] = args[1]
		case "-c", "--config":
			key, value, ok := strings.Cut(args[1], "=")
			if !ok || strings.TrimSpace(key) == "" {
				return nil, fmt.Errorf("native Codex configuration override invalid")
			}
			var parsed struct {
				Value any `toml:"value"`
			}
			if _, err := toml.Decode("value = "+value, &parsed); err != nil {
				parsed.Value = value // Same literal-string rule as the native CLI.
			}
			config[key] = parsed.Value
		default:
			return nil, fmt.Errorf("native Codex carrier argument unsupported: %s", args[0])
		}
		args = args[2:]
	}
	return config, nil
}

type nativeCodexSession struct {
	sessionBase
	nativeMu     sync.Mutex
	client       *codexRPC
	process      *ProcessHandle
	stderrDone   <-chan error
	dir          string
	record       NativeCodexRecord
	terminal     chan struct{}
	release      chan struct{}
	completion   sync.WaitGroup
	rootUnloaded bool // Fresh recovery readback; never inferred from an archive ACK.
}

func (s *nativeCodexSession) persist() error {
	raw, err := json.Marshal(s.record)
	if err != nil {
		return err
	}
	if err := filex.WriteFileAtomicDurable(filepath.Join(s.dir, "native.json"), raw); err != nil {
		return err
	}
	status := "running"
	var alive any
	if s.record.State == "running" || s.record.State == "terminal" || s.record.State == "unsubscribe_intent" || s.record.State == "waiting_native_unload" || s.record.State == "draining_background" {
		alive = true
	}
	if s.record.State == "released" {
		status, alive = "exited", false
		if s.record.Terminal == "failed" {
			status = "failed"
		}
	}
	meta, err := json.Marshal(map[string]any{"session_id": s.id, "runtime": CodexNativeRuntime, "status": status,
		"alive": alive, "native_state": s.record.State, "provider": "codex", "model": s.record.Model})
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(s.dir, "meta.json"), meta)
}

func (s *nativeCodexSession) unknown(err error) {
	s.nativeMu.Lock()
	if s.record.State == "released" {
		s.nativeMu.Unlock()
		return
	}
	s.record.State, s.record.Problem = "needs_owner_readback", err.Error()
	if persistErr := s.persist(); persistErr != nil {
		s.publish(SessionEvent{Type: "warning", Message: "native state persistence failed: " + persistErr.Error()})
	}
	s.publish(SessionEvent{Type: "warning", Message: err.Error()})
	s.nativeMu.Unlock()
}

func (s *nativeCodexSession) startCompletion() {
	s.completion.Add(1)
	go func() { defer s.completion.Done(); s.complete() }()
}

func (s *nativeCodexSession) observe(raw json.RawMessage) error {
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	var event struct {
		Method string
		Params struct {
			ThreadID string
			Thread   json.RawMessage
			Turn     struct{ ID, Status string }
			Item     struct{ Type, Text, Kind, AgentThreadID string }
			Status   json.RawMessage
		}
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	var request nativeRequestEnvelope
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	if (len(request.ID) != 0 && string(request.ID) != "null") || request.Method == "serverRequest/resolved" {
		return s.observeServerRequest(request, raw)
	}
	if event.Method == "thread/started" && s.record.ThreadID != "" {
		var started struct {
			ID, Cwd string
			Source  json.RawMessage
		}
		if err := json.Unmarshal(event.Params.Thread, &started); err != nil {
			return err
		}
		if started.ID != s.record.ThreadID {
			var origin struct {
				SubAgent struct {
					Spawn struct {
						Parent string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subAgent"`
			}
			if json.Unmarshal(started.Source, &origin) == nil && (origin.SubAgent.Spawn.Parent == s.record.ThreadID || s.record.Children[origin.SubAgent.Spawn.Parent] != nil) {
				if !codexThreadUUID.MatchString(started.ID) || started.Cwd != s.record.Worktree {
					return fmt.Errorf("native child start binding conflicts")
				}
				if s.record.Children == nil {
					s.record.Children = make(map[string]*NativeCodexChild)
				}
				if s.record.Children[started.ID] == nil {
					s.record.Children[started.ID] = &NativeCodexChild{Source: append(json.RawMessage(nil), event.Params.Thread...)}
				}
				event.Params.ThreadID = started.ID
			}
		}
	}
	if event.Params.ThreadID == "" || (event.Params.ThreadID != s.record.ThreadID && s.record.Children[event.Params.ThreadID] == nil) {
		return nil // Shared backend notifications do not bind unrelated tasks.
	}
	file, err := os.OpenFile(filepath.Join(s.dir, "raw.ndjson"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(append([]byte(nil), raw...), '\n'))
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if event.Params.Item.Type == "subAgentActivity" && (event.Params.Item.Kind == "started" || event.Params.Item.Kind == "completed") {
		childID := event.Params.Item.AgentThreadID
		if !codexThreadUUID.MatchString(childID) || childID == s.record.ThreadID {
			return fmt.Errorf("native child activity identity conflicts")
		}
		if s.record.Children == nil {
			s.record.Children = make(map[string]*NativeCodexChild)
		}
		if s.record.Children[childID] == nil {
			s.record.Children[childID] = &NativeCodexChild{Source: append(json.RawMessage(nil), raw...)}
		}
		if err := s.persist(); err != nil {
			return err
		}
	}
	if s.record.ThreadID == "" || event.Params.ThreadID != s.record.ThreadID {
		if child := s.record.Children[event.Params.ThreadID]; child != nil {
			if event.Method == "turn/completed" {
				if event.Params.Turn.ID == "" || (child.TurnID != "" && event.Params.Turn.ID != child.TurnID) || !nativeTerminal(event.Params.Turn.Status) {
					return fmt.Errorf("native child terminal conflicts with original turn")
				}
				child.Terminal = event.Params.Turn.Status
				child.TurnID = event.Params.Turn.ID
			}
			if err := s.persist(); err != nil {
				return err
			}
			select {
			case s.release <- struct{}{}:
			default:
			}
		}
		return nil
	}
	canonical := parse.CanonicalEvent{Provider: "codex", Timestamp: nowUTC()}
	switch event.Method {
	case "turn/started":
		if s.record.TurnID != "" && s.record.TurnID != event.Params.Turn.ID {
			return fmt.Errorf("later native turn cannot replace original attempt")
		}
		s.record.TurnID = event.Params.Turn.ID
		canonical.Type, canonical.Message = parse.EventInit, "native Codex turn started"
	case "item/completed":
		if event.Params.Item.Type != "agentMessage" {
			return nil
		}
		canonical.Type, canonical.Message = parse.EventAction, "text:"+event.Params.Item.Text
	case "turn/completed":
		if s.record.TurnID != event.Params.Turn.ID || !nativeTerminal(event.Params.Turn.Status) {
			return fmt.Errorf("native terminal does not match original turn")
		}
		s.record.Terminal, s.record.State = event.Params.Turn.Status, "terminal"
		if event.Params.Turn.Status == "interrupted" {
			s.record.StopRequested = true
		}
		select {
		case s.terminal <- struct{}{}:
		default:
		}
		canonical.Type, canonical.Message = parse.EventResult, "native turn "+event.Params.Turn.Status
	case "thread/closed", "thread/status/changed":
		var status struct{ Type string }
		if event.Method == "thread/status/changed" {
			if err := json.Unmarshal(event.Params.Status, &status); err != nil {
				return fmt.Errorf("native thread status invalid: %w", err)
			}
		}
		if event.Method == "thread/closed" || status.Type == "notLoaded" {
			select {
			case s.release <- struct{}{}:
			default:
			}
		}
		return nil
	default:
		return nil
	}
	if err := s.persist(); err != nil {
		return err
	}
	line, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	file, err = os.OpenFile(s.canonicalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr = file.Write(append(line, '\n'))
	closeErr = file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	s.observeCanonicalEvent(canonical)
	s.consumeCanonicalLine(line, nil)
	return nil
}

func nativeTerminal(status string) bool {
	return status == "completed" || status == "interrupted" || status == "failed"
}

func (s *nativeCodexSession) originalTerminal(ctx context.Context) error {
	s.nativeMu.Lock()
	id, turn, terminal := s.record.ThreadID, s.record.TurnID, s.record.Terminal
	s.nativeMu.Unlock()
	var result struct {
		Thread struct {
			ID     string
			Status struct{ Type string }
		}
	}
	if err := s.client.call(ctx, "thread/read", map[string]string{"threadId": id}, &result); err != nil {
		return err
	}
	if result.Thread.ID != id || result.Thread.Status.Type != "notLoaded" {
		return fmt.Errorf("original native thread still loaded; no resume or release inferred")
	}
	return s.sameTerminal(ctx, id, turn, terminal)
}

var errNativeTerminalPending = errors.New("native terminal readback pending")

func (s *nativeCodexSession) sameTerminal(ctx context.Context, id, turn, terminal string) error {
	var turns struct{ Data []struct{ ID, Status string } }
	if err := s.client.call(ctx, "thread/turns/list", map[string]any{
		"threadId": id, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
	}, &turns); err != nil {
		return err
	}
	if len(turns.Data) != 1 || turns.Data[0].ID != turn || !nativeTerminal(terminal) {
		return fmt.Errorf("original terminal identity changed; expected=%s observed=%+v", turn, turns.Data)
	}
	if turns.Data[0].Status == "inProgress" {
		return fmt.Errorf("%w: thread=%s turn=%s", errNativeTerminalPending, id, turn)
	}
	if turns.Data[0].Status != terminal {
		return fmt.Errorf("original terminal outcome differs: expected=%s observed=%s", terminal, turns.Data[0].Status)
	}
	return nil
}

func (s *nativeCodexSession) complete() {
	select {
	case <-s.terminal:
	case <-s.client.done:
		s.unknown(fmt.Errorf("native connection ended before original terminal"))
		return
	}
	if err := s.releaseForeground(); err != nil {
		s.unknown(err)
	}
}

func (s *nativeCodexSession) Controller() AgentController { return NoopController() }
func (s *nativeCodexSession) Terminate() error {
	s.nativeMu.Lock()
	thread, turn := s.record.ThreadID, s.record.TurnID
	if s.client == nil || thread == "" || turn == "" {
		s.nativeMu.Unlock()
		return fmt.Errorf("original native turn unavailable; effect remains unknown")
	}
	if nativeTerminal(s.record.Terminal) {
		s.record.StopRequested = true
		err := s.persist()
		s.nativeMu.Unlock()
		select {
		case s.release <- struct{}{}:
		default:
		}
		return err
	}
	if s.record.StopRequested {
		acknowledged := len(s.record.StopACK) != 0
		s.nativeMu.Unlock()
		if acknowledged {
			return nil
		}
		return fmt.Errorf("original stop effect unknown; read back before another request")
	}
	s.record.State = "stop_intent"
	s.record.StopRequested = true
	err := s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexRPCOperationTimeout)
	defer cancel()
	var result json.RawMessage
	if err := s.client.call(ctx, "turn/interrupt", map[string]string{"threadId": thread, "turnId": turn}, &result); err != nil {
		var refused *codexRPCRefusal
		if errors.As(err, &refused) {
			if readErr := s.readTerminalAfterStopRefusal(ctx, thread, turn, refused.Raw); readErr == nil {
				return nil
			} else {
				err = errors.Join(err, readErr)
			}
		}
		s.unknown(err)
		return err
	}
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	s.record.StopACK = result
	return s.persist()
}
func (s *nativeCodexSession) ForceKill() error {
	return fmt.Errorf("native thread has no process force-stop authorization")
}

func readNativePredecessor(runtimeDir string, req DispatchRequest, home string) (NativeCodexRecord, error) {
	old, err := ReadReleasedNativeSession(context.Background(), runtimeDir, req.ResumeSessionID)
	if err != nil {
		return old, err
	}
	if old.NativeHome != home || old.Worktree != req.WorktreePath || old.TaskRole != req.TaskRole || old.TaskKey != req.TaskKey || old.OrderID != req.EnvVars["NOODLE_ORDER_ID"] {
		return old, fmt.Errorf("native predecessor custody, role, worktree or release differs")
	}
	return old, nil
}

// An explicit rejection can race a terminal notification. Read the original
// turn; never retry the interrupt or turn a lost response into an ACK.
func (s *nativeCodexSession) readTerminalAfterStopRefusal(ctx context.Context, thread, turn string, raw json.RawMessage) error {
	s.nativeMu.Lock()
	s.record.StopRefusal = raw
	err := s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	var turns struct{ Data []struct{ ID, Status string } }
	if err := s.client.call(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &turns); err != nil {
		return err
	}
	if len(turns.Data) != 1 || turns.Data[0].ID != turn || !nativeTerminal(turns.Data[0].Status) {
		return fmt.Errorf("stop refusal has no matching terminal readback")
	}
	s.nativeMu.Lock()
	if s.record.ThreadID != thread || s.record.TurnID != turn {
		s.nativeMu.Unlock()
		return fmt.Errorf("stop refusal original identity changed")
	}
	s.record.Terminal = turns.Data[0].Status
	s.record.State = "terminal"
	err = s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	select {
	case s.terminal <- struct{}{}:
	default:
	}
	return nil
}

type NativeServerRequest struct {
	Request  json.RawMessage `json:"request"`
	ThreadID string          `json:"thread_id"`
	Resolved json.RawMessage `json:"resolved,omitempty"`
}

// Called with nativeMu held. Receipt preserves the request, not permission.
func (s *nativeCodexSession) observeServerRequest(event nativeRequestEnvelope, raw json.RawMessage) error {
	if s.record.ServerRequests == nil {
		s.record.ServerRequests = make(map[string]*NativeServerRequest)
	}
	if event.Method == "serverRequest/resolved" {
		original := s.record.ServerRequests[string(event.Params.RequestID)]
		if original == nil || original.ThreadID == "" || original.ThreadID != event.Params.ThreadID {
			return fmt.Errorf("native request resolution identity unavailable")
		}
		original.Resolved = append(json.RawMessage(nil), raw...)
		pending := false
		for _, request := range s.record.ServerRequests {
			if len(request.Resolved) == 0 {
				pending = true
			}
		}
		if !pending && s.record.Problem == "native request retained; Host response capability unavailable" {
			s.record.Problem = ""
		}
		if !pending && s.record.State == "native_request_pending" {
			s.record.State = "running"
			if nativeTerminal(s.record.Terminal) {
				s.record.State = "terminal"
			}
		}

	} else {
		key := string(event.ID)
		if old := s.record.ServerRequests[key]; old != nil && string(old.Request) != string(raw) {
			return fmt.Errorf("native server request identity reused")
		}
		s.record.ServerRequests[key] = &NativeServerRequest{Request: append(json.RawMessage(nil), raw...), ThreadID: event.Params.ThreadID}
		s.record.State = "native_request_pending"
		s.record.Problem = "native request retained; Host response capability unavailable"
	}
	return s.persist()
}
