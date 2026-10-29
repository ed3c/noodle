package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/loop"
)

type hostBootstrapTask struct {
	Original       hostReceipt            `json:"original"`
	Request        hostRequestBinding     `json:"request"`
	RequestFile    string                 `json:"request_file"`
	Round          *loop.HostContinuation `json:"round,omitempty"`
	PauseConfirmed bool                   `json:"pause_confirmed,omitempty"`
	Detached       *hostReceipt           `json:"detached,omitempty"`
	Inputs         *hostContextInputs     `json:"inputs,omitempty"`
	Blocked        string                 `json:"blocked,omitempty"`
	CustodySaved   bool                   `json:"custody_saved"`
	StorageRoot    string                 `json:"storage_root,omitempty"`
}
type hostBootstrap struct {
	Schema          int                 `json:"schema"`
	Phase           string              `json:"phase"`
	Target          string              `json:"target"`
	TargetSHA       string              `json:"target_sha256"`
	Source          hostGeneration      `json:"source"`
	ProcessIdentity string              `json:"process_identity"`
	Tasks           []hostBootstrapTask `json:"tasks"`
}

func saveHostBootstrap(root string, b hostBootstrap) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(root, "bootstrap.json"), raw)
}
func readHostBootstrap(root string) (hostBootstrap, error) {
	var b hostBootstrap
	raw, err := os.ReadFile(filepath.Join(root, "bootstrap.json"))
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.Schema != 1 || !validPoolID(b.Source.ID) || b.Source.Directory != filepath.Join(root, "g", b.Source.ID) || b.Phase == "" || !filepath.IsAbs(b.Target) || b.TargetSHA == "" || b.ProcessIdentity == "" {
		return b, errors.New("original Host maintenance binding is invalid")
	}
	return b, nil
}

type legacyHostResources struct {
	Protocol  string `json:"protocol"`
	Instance  string `json:"host_instance_id"`
	PID       int    `json:"host_pid"`
	Contexts  int    `json:"contexts"`
	Rolling   int    `json:"rolling_host_version"`
	Resources struct {
		Workers     int `json:"active_workers"`
		Residents   int `json:"active_residents"`
		Allocations int `json:"active_allocations"`
		Unresolved  int `json:"unresolved_residents"`
	} `json:"resources"`
	Residents struct {
		Error any `json:"error"`
	} `json:"residents"`
}

func readLegacyResources(ctx context.Context, dir string) (legacyHostResources, error) {
	var r legacyHostResources
	raw, err := nativeHostRPC(ctx, dir, hostRequest{Operation: "resources"})
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	var required map[string]json.RawMessage
	if err = json.Unmarshal(raw, &required); err != nil {
		return r, err
	}
	if r.Protocol != hostProtocol || !validPoolID(r.Instance) || r.PID <= 0 || r.Rolling != 0 || required["resources"] == nil {
		return r, errors.New("original legacy Host resource identity is unavailable")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(required["resources"], &fields); err != nil {
		return r, err
	}
	for _, name := range []string{"active_workers", "active_residents", "active_allocations", "unresolved_residents"} {
		if fields[name] == nil {
			return r, fmt.Errorf("legacy Host omits %s", name)
		}
	}
	return r, nil
}

// This compatibility path is bound to the demonstrated old reducer/control
// implementation. An unknown build must expose a native migration contract.
func verifyLegacyBootstrapBinary(path, digest string) error {
	raw, err := os.ReadFile(path)
	if err != nil || hostDigest(raw) != digest {
		return errors.New("original legacy executable changed")
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return err
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["vcs.revision"] != "2b636db20166ada9e304ac3462db52dd07c69899" || settings["vcs.modified"] != "false" {
		return errors.New("legacy executable has no verified first-migration contract")
	}
	return nil
}

func beginHostBootstrap(ctx context.Context, root, target, digest string) (hostBootstrap, error) {
	b, err := readHostBootstrap(root)
	if err == nil {
		if b.Target != target || b.TargetSHA != digest {
			return b, errors.New("initial maintenance already selected another target; retain its original request")
		}
		return b, nil
	}
	if !os.IsNotExist(err) {
		return b, err
	}
	r, err := readLegacyResources(ctx, root)
	if err != nil {
		return b, err
	}
	raw, err := os.ReadFile(filepath.Join(root, "receipts.json"))
	if err != nil {
		return b, err
	}
	var receipts []hostReceipt
	if err = json.Unmarshal(raw, &receipts); err != nil {
		return b, err
	}
	var source *hostReceipt
	for i := len(receipts) - 1; i >= 0; i-- {
		if receipts[i].HostInstanceID == r.Instance {
			source = &receipts[i]
			break
		}
	}
	if source == nil || source.HostPID != r.PID {
		return b, errors.New("legacy Host has no original executable receipt")
	}
	if err = verifyLegacyBootstrapBinary(source.BinaryPath, source.BinarySHA256); err != nil {
		return b, err
	}
	identity, err := legacyProcessIdentity(ctx, r.PID, source.BinaryPath)
	if err != nil {
		return b, err
	}
	b = hostBootstrap{Schema: 1, Phase: "prepared", Target: target, TargetSHA: digest, ProcessIdentity: identity, Source: hostGeneration{ID: r.Instance, Directory: filepath.Join(root, "g", r.Instance), Binary: source.BinaryPath, SHA: source.BinarySHA256, State: "active", Process: procx.ProcessRecord{PID: r.PID, SessionID: r.Instance, StartedAt: time.Now()}}}
	if err = os.MkdirAll(b.Source.Directory, 0700); err != nil {
		return b, err
	}
	return b, saveHostBootstrap(root, b)
}

func bindLegacyBootstrapTasks(ctx context.Context, root string, b *hostBootstrap, paths []string) error {
	r, err := readLegacyResources(ctx, b.Source.Directory)
	if err != nil {
		return err
	}
	if r.Instance != b.Source.ID || r.PID != b.Source.Process.PID {
		return errors.New("legacy Host changed during maintenance")
	}
	raw, err := os.ReadFile(filepath.Join(root, "receipts.json"))
	if err != nil {
		return err
	}
	var receipts []hostReceipt
	if err = json.Unmarshal(raw, &receipts); err != nil {
		return err
	}
	active := []hostReceipt{}
	for _, receipt := range receipts {
		if receipt.HostInstanceID == b.Source.ID && receipt.ContextState != "detached" {
			active = append(active, receipt)
		}
	}
	if len(active) != r.Contexts {
		return errors.New("legacy contexts differ from the original receipts; readback required")
	}
	byID := map[string]struct {
		Binding hostRequestBinding
		Path    string
	}{}
	for _, receipt := range active {
		if receipt.ConfigPath != "" {
			paths = append(paths, filepath.Join(filepath.Dir(receipt.ConfigPath), "host-request.json"))
		}
	}
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var binding hostRequestBinding
		if err = json.Unmarshal(raw, &binding); err != nil {
			return err
		}
		if prior, exists := byID[binding.RequestID]; exists && prior.Binding != binding {
			return errors.New("conflicting original task bindings")
		}
		byID[binding.RequestID] = struct {
			Binding hostRequestBinding
			Path    string
		}{binding, path}
	}
	for _, receipt := range active {
		row, ok := byID[receipt.RequestID]
		req := hostRequest{ProjectDir: row.Binding.ProjectDir, ConfigPath: row.Binding.ConfigPath, ConfigSHA256: row.Binding.ConfigSHA256, Mode: row.Binding.Mode, RequestID: row.Binding.RequestID, CleanupCompleted: row.Binding.CleanupCompleted}
		if !ok || row.Binding.Schema != 1 || row.Binding.Status != "accepted" || row.Binding.Operation != "attach" || row.Binding.HostDir != root || row.Binding.BinarySHA256 != receipt.BinarySHA256 || row.Binding.EnvironmentSHA256 == "" || !hostReceiptMatches(req, receipt) {
			return fmt.Errorf("original accepted request is unavailable for %s", receipt.RequestID)
		}
		b.Tasks = append(b.Tasks, hostBootstrapTask{Original: receipt, Request: row.Binding, RequestFile: row.Path})
	}
	sort.Slice(b.Tasks, func(i, j int) bool { return b.Tasks[i].Original.RequestID < b.Tasks[j].Original.RequestID })
	b.Phase = "draining"
	return saveHostBootstrap(root, *b)
}

func bootstrapTaskRequest(t hostBootstrapTask) hostRequest {
	r := t.Original
	return hostRequest{ProjectDir: r.ProjectDir, ContextID: r.ContextID, InstanceID: r.HostInstanceID, ConfigPath: r.ConfigPath, ConfigSHA256: r.ConfigSHA256, Mode: r.Mode, RequestID: r.RequestID, CleanupCompleted: r.CleanupCompleted, BinarySHA256: r.BinarySHA256, AdmissionSHA: r.BinarySHA256}
}

func bootstrapCheckpoint(ctx context.Context, root string, b *hostBootstrap) (bool, error) {
	for i := range b.Tasks {
		t := &b.Tasks[i]
		if t.Detached != nil {
			continue
		}
		if t.Round != nil && (!t.PauseConfirmed || t.Round.DispatchState != loop.StatePaused) {
			return false, errors.New("retained migration has no confirmed pause boundary; readback only, no control replay")
		}
		req := bootstrapTaskRequest(*t)
		req.Operation = "status"
		raw, readErr := nativeHostRPC(ctx, b.Source.Directory, req)
		if readErr != nil {
			return false, readErr
		}
		var observed hostResponse
		if err := json.Unmarshal(raw, &observed); err != nil {
			return false, err
		}
		if observed.Status != "ok" {
			return false, fmt.Errorf("original context readback: %s %s", observed.Status, observed.Error)
		}
		if observed.ContextState == "detached" {
			if observed.TerminalCause != loop.TerminalAutoCompleted && observed.TerminalCause != loop.TerminalExplicitCleanup && observed.TerminalCause != loop.TerminalMigrationDetach {
				return false, errors.New("original context ended without a known cause")
			}
			if observed.TerminalCause == loop.TerminalMigrationDetach && t.Round == nil {
				return false, errors.New("detached context has no original migration continuation")
			}
			t.Detached = &observed.hostReceipt
			if err := saveHostBootstrap(root, *b); err != nil {
				return false, err
			}
			continue
		}
		dir := filepath.Join(root, "bootstrap", t.Original.RequestID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return false, err
		}
		if t.Round == nil {
			if !t.PauseConfirmed {
				held, pauseErr := legacyPauseCheckpoint(ctx, filepath.Join(t.Original.ProjectDir, ".noodle"), filepath.Join(dir, "pause.json"))
				if pauseErr != nil {
					return false, pauseErr
				}
				if !held {
					return false, nil
				}
				t.PauseConfirmed = true
				if err := saveHostBootstrap(root, *b); err != nil {
					return false, err
				}
			}
			round, err := legacyModeCheckpoint(ctx, filepath.Join(t.Original.ProjectDir, ".noodle"), filepath.Join(dir, "hold.json"))
			if err != nil {
				return false, err
			}
			if round == nil {
				return false, nil
			}
			if round.DispatchState != loop.StatePaused {
				return false, errors.New("original mode ACK does not preserve the confirmed pause; readback required")
			}
			t.Round = round
			if round.Mode == "" {
				t.Blocked = "original effective mode not observed; retain manual maintenance hold"
				t.Round.Mode = "manual"
			}
			if err = saveHostBootstrap(root, *b); err != nil {
				return false, err
			}
		}
	}
	r, err := readLegacyResources(ctx, b.Source.Directory)
	if err != nil {
		return false, err
	}
	if r.Instance != b.Source.ID || r.PID != b.Source.Process.PID {
		return false, errors.New("original legacy Host changed")
	}
	if r.Resources.Workers != 0 || r.Resources.Allocations != 0 || r.Resources.Residents != 0 || r.Resources.Unresolved != 0 || r.Residents.Error != nil {
		return false, nil
	}
	for i := range b.Tasks {
		t := &b.Tasks[i]
		if t.CustodySaved {
			continue
		}
		dir := filepath.Join(root, "bootstrap", t.Original.RequestID)
		runtimeDir := filepath.Join(t.Original.ProjectDir, ".noodle")
		if t.Detached == nil {
			// Re-checkpoint after real worker completion, before Release flushes projections.
			if _, statErr := os.Stat(filepath.Join(dir, "final.json")); os.IsNotExist(statErr) {
				if _, err = legacyModeCheckpoint(ctx, runtimeDir, filepath.Join(dir, "hold.json")); err != nil {
					return false, err
				}
			} else if statErr != nil {
				return false, statErr
			}
			if round, checkpointErr := legacyModeCheckpoint(ctx, runtimeDir, filepath.Join(dir, "final.json")); checkpointErr != nil {
				return false, checkpointErr
			} else if round == nil {
				return false, nil
			} else {
				if round.DispatchState != loop.StatePaused {
					return false, errors.New("original final checkpoint is not paused; no detach was sent")
				}
				t.Round.DispatchState = round.DispatchState
				if err = saveHostBootstrap(root, *b); err != nil {
					return false, err
				}
			}
			req := bootstrapTaskRequest(*t)
			req.Operation = "detach"
			response, callErr := legacyDetachOnce(ctx, b.Source.Directory, filepath.Join(dir, "detach.json"), req)
			if callErr != nil {
				return false, callErr
			}
			if response.Status == "busy" {
				return false, errors.New("original detach is busy; retain its intent and read back without resending")
			}
			if response.Status != "ok" || response.ContextState != "detached" || response.TerminalCause != loop.TerminalMigrationDetach {
				return false, fmt.Errorf("original migration detach is unresolved: %s %s", response.Status, response.Error)
			}
			t.Detached = &response.hostReceipt
			if err = saveHostBootstrap(root, *b); err != nil {
				return false, err
			}
		}
		// Preserve all existing records, including dirty workers, outside App cleanup.
		// Missing repository custody parks only this task; the original bytes remain.
		req := bootstrapTaskRequest(*t)
		data, readErr := readHostRequestConfig(req)
		if readErr != nil || hostDigest(data) != req.ConfigSHA256 {
			t.Blocked += "; original config source unavailable or changed"
		} else {
			req.Environment = os.Environ()
			storage, storageErr := prepareHostStorageAt(ctx, req, data, dir)
			if storageErr != nil {
				return false, storageErr
			}
			if storage == "" {
				t.Blocked += "; original Git worktree custody unavailable"
			} else {
				t.StorageRoot = storage
				sort.Strings(req.Environment)
				if hostDigest([]byte(strings.Join(req.Environment, "\x00"))) != t.Request.EnvironmentSHA256 {
					t.Blocked += "; original context environment source not reconstructed"
				} else if inputs, inputErr := captureHostContextInputs(req); inputErr != nil {
					t.Blocked += "; " + inputErr.Error()
				} else {
					t.Inputs = &inputs
				}
			}
		}
		if t.StorageRoot == "" {
			for _, name := range []string{".noodle", ".worktrees"} {
				source := filepath.Join(t.Original.ProjectDir, name)
				if _, statErr := os.Stat(source); os.IsNotExist(statErr) {
					t.Blocked += "; original " + name + " bytes unavailable"
					continue
				} else if statErr != nil {
					return false, statErr
				}
				if err = moveHostStorageDirectory(source, filepath.Join(dir, name)); err != nil {
					return false, err
				}
			}
		} else if err = syncHostStorageTree(runtimeDir); err != nil {
			return false, err
		}
		t.CustodySaved = true
		t.Blocked = strings.Trim(t.Blocked, "; ")
		if err = saveHostBootstrap(root, *b); err != nil {
			return false, err
		}
	}
	b.Phase = "detached"
	return true, saveHostBootstrap(root, *b)
}

func bootstrapLegacyHost(ctx context.Context, root, target, digest string, requests []string, out *json.Encoder) error {
	lock, err := lockfile.TryLock(filepath.Join(root, "maintenance.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	b, err := beginHostBootstrap(ctx, root, target, digest)
	if err != nil {
		return err
	}
	if b.Phase == "active" {
		p, readErr := readHostPool(root)
		if readErr != nil {
			return readErr
		}
		g, ok := p.Generations[p.Active]
		if !ok {
			return errors.New("selected Host is unavailable")
		}
		if _, readErr = nativeHostRPC(ctx, g.Directory, hostRequest{Operation: "resources"}); readErr != nil {
			return readErr
		}
		if err = repairPoolSocket(root, g.ID); err != nil {
			return err
		}
		return out.Encode(b)
	}
	gate, err := openHostMaintenanceGate(root, b.Source.Directory)
	if err != nil {
		return err
	}
	defer gate.Close()
	if b.Phase == "prepared" {
		// The journal precedes rename. A retry reconciles the private socket,
		// rather than moving another process's public entry.
		if _, statErr := os.Stat(filepath.Join(b.Source.Directory, "host.sock")); os.IsNotExist(statErr) {
			if identity, readErr := legacyProcessIdentity(ctx, b.Source.Process.PID, b.Source.Binary); readErr != nil || identity != b.ProcessIdentity {
				return errors.New("original legacy process identity changed")
			}
			if err = os.Rename(filepath.Join(root, "host.sock"), filepath.Join(b.Source.Directory, "host.sock")); err != nil {
				return err
			}
			if err = filex.SyncDir(b.Source.Directory); err != nil {
				return err
			}
		} else if statErr != nil {
			return statErr
		}
		b.Phase = "fenced"
		if err = saveHostBootstrap(root, b); err != nil {
			return err
		}
	}
	if err = publishMaintenanceSocket(root); err != nil {
		return err
	}
	if b.Phase != "fenced" && b.Phase != "draining" {
		gate.freeze()
	}
	if b.Phase == "fenced" {
		if err = bindLegacyBootstrapTasks(ctx, root, &b, requests); err != nil {
			return err
		}
	}
	if err = out.Encode(map[string]any{"phase": b.Phase, "source": b.Source.ID, "task_count": len(b.Tasks), "next": "wait_for_original_workers"}); err != nil {
		return err
	}
	for b.Phase == "draining" {
		done, stepErr := bootstrapCheckpoint(ctx, root, &b)
		if stepErr != nil {
			return stepErr
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if b.Phase == "detached" {
		gate.freeze()
		if err = verifyEmptyLegacyHost(ctx, b); err != nil {
			return err
		}
		receipts, readErr := os.ReadFile(filepath.Join(root, "receipts.json"))
		if readErr != nil {
			return readErr
		}
		if err = saveHostStorageBytes(filepath.Join(b.Source.Directory, "receipts.json"), receipts); err != nil {
			return err
		}
		b.Phase = "stopping"
		if err = saveHostBootstrap(root, b); err != nil {
			return err
		}
		// One signal through the old Host's existing normal shutdown handler.
		// A retry in stopping only observes; it never repeats this effect.
		if err = signalEmptyLegacyHost(b.Source.Process.PID); err != nil {
			return err
		}
	}
	for b.Phase == "stopping" {
		absent, readErr := b.Source.Process.GroupAbsent()
		if readErr != nil {
			// The process can disappear between kill(0) and the start-identity
			// read. Only a second positive group-absence observation resolves it.
			var absenceErr error
			absent, absenceErr = procx.ProcessGroupAbsent(b.Source.Process.PID)
			if absenceErr != nil || !absent {
				return errors.Join(readErr, absenceErr)
			}
		}
		if absent {
			b.Phase = "stopped"
			if err = saveHostBootstrap(root, b); err != nil {
				return err
			}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	// Exclude old pinned launchers until the rolling owner holds the shared fence.
	fence, err := lockfile.TryLockShared(filepath.Join(root, "host.lock"))
	if err != nil {
		return err
	}
	defer fence.Close()
	if b.Phase == "stopped" {
		b.Source.State = "exited"
		expected, buildErr := bootstrapImportedPool(b)
		if buildErr != nil {
			return buildErr
		}
		if err = updateHostPool(root, func(p *hostPool) error {
			if len(p.Generations) == 0 && len(p.Owners) == 0 && p.Active == "" && len(p.Queue) == 0 {
				*p = expected
				return nil
			}
			actualRaw, marshalErr := json.Marshal(p)
			if marshalErr != nil {
				return marshalErr
			}
			expectedRaw, marshalErr := json.Marshal(expected)
			if marshalErr != nil {
				return marshalErr
			}
			if string(actualRaw) != string(expectedRaw) {
				return errors.New("original imported task or generation binding changed")
			}
			return nil
		}); err != nil {
			return err
		}

		b.Phase = "imported"
		if err = saveHostBootstrap(root, b); err != nil {
			return err
		}
	}
	if b.Phase == "imported" {
		p, readErr := readHostPool(root)
		if readErr != nil {
			return readErr
		}
		if p.Active == "" {
			// launchHostGeneration persists intent before spawn. Do not launch again
			// when a previous readiness or process observation is unknown.
			if len(p.Generations) != 1 {
				return errors.New("new Host launch already recorded; inspect its original generation")
			}
			g, startErr := launchHostGeneration(ctx, root, target, digest)
			if startErr != nil {
				return startErr
			}
			if err = updateHostPool(root, func(p *hostPool) error { p.Active = g.ID; g.State = "active"; p.Generations[g.ID] = g; return nil }); err != nil {
				return err
			}
		}
		b.Phase = "rebuilding"
		if err = saveHostBootstrap(root, b); err != nil {
			return err
		}
	}
	if b.Phase == "rebuilding" {
		p, readErr := readHostPool(root)
		if readErr != nil {
			return readErr
		}
		g, ok := p.Generations[p.Active]
		if !ok || g.SHA != digest {
			return errors.New("selected new Host is unresolved")
		}
		for _, t := range b.Tasks {
			if t.Blocked != "" || t.Detached.TerminalCause != loop.TerminalMigrationDetach {
				continue
			}
			req := bootstrapTaskRequest(t)
			req.ContextID = ""
			req.InstanceID = ""
			req.Operation = "round_rebuild"
			req.RoundSource = b.Source.ID
			req.BinarySHA256 = digest
			req.Environment = os.Environ()
			raw, callErr := nativeHostRPC(ctx, g.Directory, req)
			if callErr != nil {
				return callErr
			}
			var response hostResponse
			if err = json.Unmarshal(raw, &response); err != nil {
				return err
			}
			if response.Status != "ok" {
				return fmt.Errorf("original task rebuild remains unresolved: %s %s", response.Status, response.Error)
			}
		}
		b.Phase = "active"
		if err = saveHostBootstrap(root, b); err != nil {
			return err
		}
		if err = publishPoolSocket(root, g.Directory); err != nil {
			return err
		}
	}
	return out.Encode(b)
}

func bootstrapImportedPool(b hostBootstrap) (hostPool, error) {
	p := hostPool{Schema: 1, Generations: map[string]hostGeneration{b.Source.ID: b.Source}, Owners: map[string]hostTaskOwner{}}
	for _, t := range b.Tasks {
		if t.Detached == nil || !t.CustodySaved || (t.Detached.TerminalCause == loop.TerminalMigrationDetach && t.Round == nil) {
			return p, errors.New("original task release or custody is unconfirmed")
		}
		taskState := "ready"
		if t.Detached.TerminalCause != loop.TerminalMigrationDetach {
			taskState = "terminal"
		}
		p.Owners[t.Original.RequestID] = hostTaskOwner{Original: t.Original, Current: *t.Detached, State: taskState, RoundState: t.Round, ContextInputs: t.Inputs, RebuildError: t.Blocked, EnvironmentSHA: t.Request.EnvironmentSHA256}
	}
	return p, nil
}
