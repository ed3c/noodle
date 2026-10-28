package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

type restrictedTask struct {
	Plan     hostMaintenanceContextPlan `json:"plan"`
	Phase    string                     `json:"phase"`
	Round    *loop.HostContinuation     `json:"round,omitempty"`
	Detached *hostReceipt               `json:"detached,omitempty"`
}

type restrictedMaintenance struct {
	RuntimeLocks map[string]uint64   `json:"runtime_locks,omitempty"`
	Schema       int                 `json:"schema"`
	Plan         hostMaintenancePlan `json:"plan"`
	Phase        string              `json:"phase"`
	Tasks        []restrictedTask    `json:"tasks"`
	Generation   *hostGeneration     `json:"generation,omitempty"`
}

func newHostRestrictedCmd() *cobra.Command {
	var path, confirmation string
	var inspect bool
	cmd := &cobra.Command{Use: "maintenance", Short: "Apply a confirmed, source-bound restricted maintenance plan; keep business tasks held", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var p hostMaintenancePlan
			if err = json.Unmarshal(raw, &p); err != nil {
				return err
			}
			if err = validateRestrictedPlan(p); err != nil {
				return err
			}
			root, err := existingHostDirectory()
			if err != nil || root != p.Root {
				return errors.New("original Host root differs from the confirmed plan")
			}
			dir := filepath.Join(root, "maintenance", p.PlanSHA)
			if inspect {
				j, err := readRestrictedMaintenance(dir)
				if err != nil {
					return err
				}
				observed := map[string]any{"journal": j, "effects": []string{}}
				owners := map[string]string{"source": restrictedSource(p).Directory}
				if j.Generation != nil {
					owners["target"] = j.Generation.Directory
				}
				for name, ownerDir := range owners {
					if ownerDir == "" {
						continue
					}
					raw, readErr := nativeHostRPC(cmd.Context(), ownerDir, hostRequest{Operation: "resources"})
					if readErr != nil {
						observed[name+"_error"] = readErr.Error()
					} else {
						observed[name] = raw
					}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(observed)
			}
			if confirmation != p.PlanSHA {
				return errors.New("restricted maintenance requires the exact confirmed plan digest")
			}
			return runRestrictedMaintenance(cmd.Context(), p, dir, json.NewEncoder(cmd.OutOrStdout()))
		}}
	cmd.Flags().StringVar(&path, "plan", "", "Original read-only maintenance plan file")
	cmd.Flags().StringVar(&confirmation, "confirm-plan-sha256", "", "Digest approved for this exact source and target")
	cmd.Flags().BoolVar(&inspect, "status", false, "Read the original maintenance journal without performing a step")
	return cmd
}

func validateRestrictedPlan(p hostMaintenancePlan) error {
	digest := p.PlanSHA
	p.PlanSHA = ""
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if digest == "" || hostDigest(raw) != digest || p.Schema != 1 || !p.Executable || len(p.Blockers) != 0 || p.Native == nil || !p.Native.SameGeneration(*p.Native) || p.Native.UID != os.Getuid() || p.Native.PID != p.Source.PID || !validPoolID(p.Source.Instance) || len(p.Contexts) != p.Source.Contexts || len(p.Contexts) == 0 || !filepath.IsAbs(p.Root) || !filepath.IsAbs(p.Target) || len(p.TargetSHA) != 64 || p.Policy != admission.WorkerAdmissionPolicy {
		return errors.New("restricted maintenance plan is invalid, blocked or changed")
	}
	for _, name := range []string{"bootstrap.json", "pool.json", "quota/quota.json", "receipts.json"} {
		if len(p.Files[name]) != 64 {
			return errors.New("required original maintenance source digest is missing")
		}
	}
	for _, cp := range p.Contexts {
		if len(cp.Blockers) != 0 || !filepath.IsAbs(cp.RequestFile) || !filepath.IsAbs(cp.ConfigSource) || len(cp.Files[cp.RequestFile]) != 64 || cp.Files[cp.ConfigSource] != cp.Original.ConfigSHA256 || !validPoolID(cp.Original.ContextID) || !validPoolID(cp.Original.RequestID) || cp.Original.HostInstanceID != p.Source.Instance || cp.Original.HostPID != p.Source.PID {
			return errors.New("original context evidence is incomplete")
		}
	}
	return nil
}

func readRestrictedMaintenance(dir string) (restrictedMaintenance, error) {
	var j restrictedMaintenance
	raw, err := os.ReadFile(filepath.Join(dir, "journal.json"))
	if err == nil {
		err = json.Unmarshal(raw, &j)
	}
	if err == nil && (j.Schema != 1 || j.Phase == "" || validateRestrictedPlan(j.Plan) != nil) {
		err = errors.New("original restricted maintenance journal is invalid")
	}
	if err == nil {
		switch j.Phase {
		case "prepared", "fencing", "holding", "detached", "stopping", "stopped", "imported", "launching", "active":
		default:
			return j, errors.New("unknown original maintenance phase")
		}
		if len(j.Tasks) != len(j.Plan.Contexts) {
			return j, errors.New("original maintenance task set changed")
		}
		for i, t := range j.Tasks {
			want, _ := json.Marshal(j.Plan.Contexts[i])
			got, _ := json.Marshal(t.Plan)
			if !bytes.Equal(want, got) {
				return j, errors.New("original maintenance task binding changed")
			}
			switch t.Phase {
			case "prepared", "pausing", "checkpointing", "held", "detaching", "detached":
			default:
				return j, errors.New("unknown original task maintenance phase")
			}
		}
	}
	return j, err
}

func saveRestrictedMaintenance(dir string, j restrictedMaintenance) error {
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(dir, "journal.json"), raw)
}

func verifyRestrictedSources(ctx context.Context, p hostMaintenancePlan) error {
	if err := verifyRollingTarget(ctx, p.Target, p.TargetSHA); err != nil {
		return err
	}
	for name, digest := range p.Files {
		raw, err := os.ReadFile(filepath.Join(p.Root, name))
		if err != nil || hostDigest(raw) != digest {
			return fmt.Errorf("original maintenance source changed: %s", name)
		}
	}
	for _, cp := range p.Contexts {
		for name, digest := range cp.Files {
			path := name
			if !filepath.IsAbs(path) {
				path = filepath.Join(cp.Original.ProjectDir, ".noodle", name)
			}
			raw, err := os.ReadFile(path)
			if digest == "absent" && os.IsNotExist(err) {
				continue
			}
			if err != nil || hostDigest(raw) != digest {
				return fmt.Errorf("original context source changed: %s", path)
			}
		}
	}
	return verifyRestrictedOwner(ctx, p, p.Root)
}

func verifyRestrictedOwner(ctx context.Context, p hostMaintenancePlan, dir string) error {
	native, err := procx.ObserveNativeIdentity(p.Source.PID)
	if err != nil || !p.Native.SameGeneration(native) || native.Zombie {
		return errors.New("original owner generation changed; readback only")
	}
	r, err := readLegacyResources(ctx, dir)
	if err != nil {
		return err
	}
	if r.Instance != p.Source.Instance || r.PID != p.Source.PID {
		return errors.New("original Host identity changed")
	}
	active := 0
	for _, cp := range p.Contexts {
		if err = verifyLegacyBootstrapBinary(cp.Original.BinaryPath, cp.Original.BinarySHA256); err != nil {
			return err
		}
		if _, err = legacyProcessIdentity(ctx, p.Source.PID, cp.Original.BinaryPath); err != nil {
			return err
		}
		req := bootstrapTaskRequest(hostBootstrapTask{Original: cp.Original})
		req.Operation = "status"
		raw, readErr := nativeHostRPC(ctx, dir, req)
		var observed hostResponse
		if readErr != nil || json.Unmarshal(raw, &observed) != nil || observed.Status != "ok" || observed.BinarySHA256 != cp.Original.BinarySHA256 || !hostReceiptMatches(req, observed.hostReceipt) {
			return errors.New("original context identity or effect is unresolved")
		}
		if observed.ContextState == "active" {
			active++
		} else if observed.ContextState != "detached" || observed.TerminalCause != loop.TerminalMigrationDetach {
			return errors.New("original context ended outside this maintenance boundary")
		}
	}
	if active != r.Contexts {
		return errors.New("original Host has an unplanned context; no control was sent")
	}
	return nil
}

func restrictedSource(p hostMaintenancePlan) hostGeneration {
	r := p.Contexts[0].Original
	return hostGeneration{ID: p.Source.Instance, Directory: filepath.Join(p.Root, "g", p.Source.Instance), Binary: r.BinaryPath, SHA: r.BinarySHA256, State: "active",
		Process: procx.ProcessRecord{PID: p.Source.PID, SessionID: p.Source.Instance, StartedAt: time.Unix(int64(p.Native.StartSec), int64(p.Native.StartUsec)*1000)}}
}

func runRestrictedMaintenance(ctx context.Context, p hostMaintenancePlan, dir string, out *json.Encoder) error {
	lock, err := lockfile.TryLock(filepath.Join(p.Root, "maintenance.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	j, err := readRestrictedMaintenance(dir)
	if os.IsNotExist(err) {
		if err = verifyRestrictedSources(ctx, p); err != nil {
			return err
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		for name := range p.Files {
			raw, readErr := os.ReadFile(filepath.Join(p.Root, name))
			if readErr != nil {
				return readErr
			}
			if err = saveHostStorageBytes(filepath.Join(dir, "original-"+filepath.Base(name)), raw); err != nil {
				return err
			}
		}
		j = restrictedMaintenance{Schema: 1, Plan: p, Phase: "prepared"}
		for _, cp := range p.Contexts {
			j.Tasks = append(j.Tasks, restrictedTask{Plan: cp, Phase: "prepared"})
		}
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if j.Plan.PlanSHA != p.PlanSHA {
		return errors.New("another original maintenance plan is already recorded")
	}
	source := restrictedSource(p)
	if j.Phase == "active" {
		return out.Encode(j)
	}
	if j.Phase == "launching" && restrictedPublicationObserved(j) {
		// Publication may have completed before its final journal fsync. Read
		// the selected native owner instead of selecting or spawning it again.
		j.Phase = "active"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
		return out.Encode(j)
	}
	// A foreground gate has no workers. The original bootstrap marker blocks
	// cooperative launchers if this command exits while effects need readback.
	gate, err := openHostMaintenanceGate(p.Root, source.Directory)
	if err != nil {
		return err
	}
	defer gate.Close()
	if j.Phase == "prepared" {
		if err = verifyRestrictedSources(ctx, p); err != nil {
			return err
		}
		identity, err := legacyProcessIdentity(ctx, p.Source.PID, source.Binary)
		if err != nil {
			return err
		}
		marker := hostBootstrap{Schema: 1, Phase: "restricted-maintenance", Target: p.Target, TargetSHA: p.TargetSHA, Source: source, ProcessIdentity: identity}
		if err = saveHostBootstrap(p.Root, marker); err != nil {
			return err
		}
		j.Phase = "fencing"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
		if err = os.MkdirAll(source.Directory, 0700); err != nil {
			return err
		}
		if err = os.Rename(filepath.Join(p.Root, "host.sock"), filepath.Join(source.Directory, "host.sock")); err != nil {
			return err
		}
		if err = filex.SyncDir(source.Directory); err != nil {
			return err
		}
	}
	if j.Phase == "fencing" {
		// Never repeat a rename with an unknown outcome. A positive private
		// socket readback can confirm the already recorded move.
		if err = verifyRestrictedOwner(ctx, p, source.Directory); err != nil {
			return err
		}
		if err = publishRestrictedGate(p.Root); err != nil {
			return err
		}
		j.Phase = "holding"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	if j.Phase == "holding" {
		locks, lockErr := acquireRestrictedRuntimeLocks(dir, &j)
		if lockErr != nil {
			return lockErr
		}
		defer func() {
			for _, held := range locks {
				_ = held.Close()
			}
		}()
		if err = publishRestrictedGate(p.Root); err != nil {
			return err
		}
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			done, stepErr := captureRestrictedTasks(ctx, dir, &j, source)
			if stepErr != nil {
				return stepErr
			}
			if done {
				j.Phase = "detached"
				if err = saveRestrictedMaintenance(dir, j); err != nil {
					return err
				}
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if j.Phase == "holding" {
			return out.Encode(j)
		}
	}
	gate.freeze()
	if j.Phase == "detached" {
		if err = verifyRestrictedOwner(ctx, p, source.Directory); err != nil {
			return err
		}
		identity, err := legacyProcessIdentity(ctx, p.Source.PID, source.Binary)
		if err != nil {
			return err
		}
		if err = verifyEmptyLegacyHost(ctx, hostBootstrap{Source: source, ProcessIdentity: identity}); err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(p.Root, "receipts.json"))
		if err != nil {
			return err
		}
		if err = saveHostStorageBytes(filepath.Join(source.Directory, "receipts.json"), raw); err != nil {
			return err
		}
		j.Phase = "stopping"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
		if err = procx.SignalNativeIdentity(*p.Native, false); err != nil {
			return err
		}
	}
	if j.Phase == "stopping" {
		native, readErr := procx.ObserveNativeIdentity(p.Source.PID)
		if readErr == nil && p.Native.SameGeneration(native) {
			return out.Encode(j)
		}
		if !errors.Is(readErr, procx.ErrProcessGone) {
			return errors.New("original Host exit is not proved; no replacement control was sent")
		}
		absent, groupErr := source.Process.GroupAbsent()
		if groupErr != nil || !absent {
			return errors.New("original Host group exit is unconfirmed")
		}
		j.Phase = "stopped"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	// Existing Host launchers hold the same shared fence. The old standalone
	// Host used the exclusive side; no private force-stop path is introduced.
	fence, err := lockfile.TryLockShared(filepath.Join(p.Root, "host.lock"))
	if err != nil {
		return err
	}
	defer fence.Close()
	if j.Phase == "stopped" {
		// The old owner's normal defer removes its original public socket path,
		// including the gate symlink. Restore only an absent/our-gate entry after
		// native exit and the shared Host fence have both been verified.
		if err = publishRestrictedGate(p.Root); err != nil {
			return err
		}
		oldPool, err := os.ReadFile(filepath.Join(dir, "original-pool.json"))
		if err != nil {
			return err
		}
		var pool hostPool
		if err = json.Unmarshal(oldPool, &pool); err != nil {
			return err
		}
		for _, g := range pool.Generations {
			absent, readErr := g.Process.GroupAbsent()
			if readErr != nil || !absent {
				return errors.New("old quota writer retirement changed; migration refused")
			}
		}
		quota, err := os.ReadFile(filepath.Join(dir, "original-quota.json"))
		if err != nil {
			return err
		}
		if err = admission.MigrateEmptySharedQuota(ctx, filepath.Join(p.Root, "quota"), dir, quota); err != nil {
			return err
		}
		source.State = "exited"
		expected, err := restrictedImportedPool(pool, source, j)
		if err != nil {
			return err
		}
		if err = replaceRestrictedPool(p.Root, dir, p.Files["pool.json"], expected); err != nil {
			return err
		}
		j.Phase = "imported"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	if j.Phase == "imported" {
		j.Phase = "launching"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
		g, launchErr := launchHostGeneration(ctx, p.Root, p.Target, p.TargetSHA)
		j.Generation = &g
		if !validPoolID(g.ID) {
			j.Generation = nil
		}
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
		if launchErr != nil {
			return launchErr
		}
	}
	if j.Phase == "launching" {
		if j.Generation == nil {
			return errors.New("new Host launch outcome needs original generation readback; no spawn repeated")
		}
		if err = verifyRestrictedTarget(ctx, j); err != nil {
			return err
		}
		g := *j.Generation
		if err = updateHostPool(p.Root, func(pool *hostPool) error {
			if pool.Active != "" && pool.Active != g.ID {
				return errors.New("selected Host changed during publication")
			}
			actual, ok := pool.Generations[g.ID]
			if !ok || actual.SHA != g.SHA || actual.Process.PID != g.Process.PID {
				return errors.New("selected launch generation changed")
			}
			pool.Active = g.ID
			g.State = "active"
			pool.Generations[g.ID] = g
			return nil
		}); err != nil {
			return err
		}
		marker, err := readHostBootstrap(p.Root)
		if err != nil || marker.TargetSHA != p.TargetSHA || marker.Source.ID != p.Source.Instance {
			return errors.New("restricted maintenance marker changed")
		}
		marker.Phase = "active"
		if err = saveHostBootstrap(p.Root, marker); err != nil {
			return err
		}
		if target, readErr := os.Readlink(filepath.Join(p.Root, "host.sock")); readErr != nil || (target != filepath.Join(p.Root, "maintenance.sock") && target != filepath.Join(g.Directory, "host.sock")) {
			return errors.New("public Host entry changed before target publication")
		}
		if err = publishPoolSocket(p.Root, g.Directory); err != nil {
			return err
		}
		j.Phase = "active"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	return out.Encode(j)
}

func restrictedPublicationObserved(j restrictedMaintenance) bool {
	if j.Generation == nil {
		return false
	}
	g := j.Generation
	marker, err := readHostBootstrap(j.Plan.Root)
	if err != nil || marker.Phase != "active" || marker.TargetSHA != j.Plan.TargetSHA || marker.Source.ID != j.Plan.Source.Instance {
		return false
	}
	p, err := readHostPool(j.Plan.Root)
	if err != nil || p.Active != g.ID {
		return false
	}
	target, err := os.Readlink(filepath.Join(j.Plan.Root, "host.sock"))
	if err != nil || target != filepath.Join(g.Directory, "host.sock") {
		return false
	}
	raw, err := nativeHostRPC(context.Background(), g.Directory, hostRequest{Operation: "resources"})
	if err != nil {
		return false
	}
	var observed struct {
		Instance  string             `json:"host_instance_id"`
		PID       int                `json:"host_pid"`
		Binary    string             `json:"binary_sha256"`
		Resources admission.Snapshot `json:"resources"`
	}
	return json.Unmarshal(raw, &observed) == nil && observed.Instance == g.ID && observed.PID == g.Process.PID && observed.Binary == j.Plan.TargetSHA && observed.Resources.AdmissionPolicy == admission.WorkerAdmissionPolicy && observed.Resources.SwapUsedLimit == admission.DefaultSwapUsedLimit
}

func captureRestrictedTasks(ctx context.Context, dir string, j *restrictedMaintenance, source hostGeneration) (bool, error) {
	if err := verifyRestrictedOwner(ctx, j.Plan, source.Directory); err != nil {
		return false, err
	}
	return captureRestrictedTasksFromOwner(ctx, dir, j, source)
}

func captureRestrictedTasksFromOwner(ctx context.Context, dir string, j *restrictedMaintenance, source hostGeneration) (bool, error) {
	for i := range j.Tasks {
		t := &j.Tasks[i]
		custody := filepath.Join(dir, t.Plan.Original.RequestID)
		if err := os.MkdirAll(custody, 0700); err != nil {
			return false, err
		}
		runtimeDir := filepath.Join(t.Plan.Original.ProjectDir, ".noodle")
		if t.Phase == "prepared" {
			// Preserve original sources without restoring files into a live checkout.
			for name, path := range map[string]string{"host-request.json": t.Plan.RequestFile, "noodle.toml": t.Plan.ConfigSource} {
				raw, err := os.ReadFile(path)
				if err != nil {
					return false, err
				}
				if hostDigest(raw) != t.Plan.Files[path] {
					return false, errors.New("original accepted source changed before hold")
				}
				if err = saveHostStorageBytes(filepath.Join(custody, name), raw); err != nil {
					return false, err
				}
			}
			for _, name := range []string{"status.json", "state.snapshot.json", "state.json", "orders.json", "pending-review.json", "tickets.json", "control-ack.ndjson"} {
				raw, readErr := os.ReadFile(filepath.Join(runtimeDir, name))
				if os.IsNotExist(readErr) {
					continue
				}
				if readErr != nil {
					return false, readErr
				}
				if err := saveHostStorageBytes(filepath.Join(custody, "before-"+name), raw); err != nil {
					return false, err
				}
			}
			t.Phase = "pausing"
			if err := saveRestrictedMaintenance(dir, *j); err != nil {
				return false, err
			}
			if _, err := legacyPauseCapture(ctx, runtimeDir, filepath.Join(custody, "pause.json"), t.Plan.RecoveryRequired); err != nil {
				return false, err
			}
		}
		if t.Phase == "pausing" {
			if _, err := os.Stat(filepath.Join(custody, "pause.json")); err != nil {
				return false, errors.New("pause intent boundary unresolved; readback only")
			}
			held, err := legacyPauseCapture(ctx, runtimeDir, filepath.Join(custody, "pause.json"), t.Plan.RecoveryRequired)
			if err != nil || !held {
				return false, err
			}
			t.Phase = "checkpointing"
			if err = saveRestrictedMaintenance(dir, *j); err != nil {
				return false, err
			}
			if _, err = legacyModeCheckpoint(ctx, runtimeDir, filepath.Join(custody, "mode.json")); err != nil {
				return false, err
			}
		}
		if t.Phase == "checkpointing" {
			if _, err := os.Stat(filepath.Join(custody, "mode.json")); err != nil {
				return false, errors.New("checkpoint intent boundary unresolved; readback only")
			}
			round, err := legacyModeCheckpoint(ctx, runtimeDir, filepath.Join(custody, "mode.json"))
			if err != nil || round == nil {
				return false, err
			}
			if round.DispatchState != loop.StatePaused {
				return false, errors.New("original checkpoint is not paused")
			}
			if err = verifyRestrictedCheckpoint(t.Plan, runtimeDir); err != nil {
				return false, err
			}
			t.Round, t.Phase = round, "held"
			if err = saveRestrictedMaintenance(dir, *j); err != nil {
				return false, err
			}
		}
	}
	r, err := readLegacyResources(ctx, source.Directory)
	if err != nil {
		return false, err
	}
	if r.Resources.Workers != 0 || r.Resources.Residents != 0 || r.Resources.Allocations != 0 || r.Resources.Unresolved != 0 || r.Residents.Error != nil {
		return false, nil
	}
	for i := range j.Tasks {
		t := &j.Tasks[i]
		if t.Phase == "detached" {
			continue
		}
		custody := filepath.Join(dir, t.Plan.Original.RequestID)
		path := filepath.Join(custody, "detach.json")
		if t.Phase == "detaching" {
			if _, err = os.Stat(path); err != nil {
				return false, errors.New("detach intent boundary unresolved; readback only")
			}
		}
		if t.Phase == "held" {
			status, readErr := os.ReadFile(filepath.Join(t.Plan.Original.ProjectDir, ".noodle", "status.json"))
			if readErr != nil {
				return false, readErr
			}
			mode, dispatch, readErr := readMaintenanceStatus(status)
			if readErr != nil || mode != "manual" || dispatch != "paused" {
				return false, errors.New("original maintenance hold changed before detach")
			}
			if _, readErr = legacyModeCheckpoint(ctx, filepath.Join(t.Plan.Original.ProjectDir, ".noodle"), filepath.Join(custody, "mode.json")); readErr != nil {
				return false, readErr
			}
			t.Phase = "detaching"
			if err = saveRestrictedMaintenance(dir, *j); err != nil {
				return false, err
			}
		}
		req := bootstrapTaskRequest(hostBootstrapTask{Original: t.Plan.Original})
		req.Operation = "detach"
		response, callErr := legacyDetachOnce(ctx, source.Directory, path, req)
		if callErr != nil {
			return false, callErr
		}
		if response.Status != "ok" || response.ContextState != "detached" || response.TerminalCause != loop.TerminalMigrationDetach {
			return false, fmt.Errorf("original detach unresolved: %s %s", response.Status, response.Error)
		}
		for _, name := range []string{"state.snapshot.json", "status.json", "control-ack.ndjson", "state.json", "orders.json", "pending-review.json", "tickets.json"} {
			raw, readErr := os.ReadFile(filepath.Join(t.Plan.Original.ProjectDir, ".noodle", name))
			if readErr != nil {
				return false, readErr
			}
			if err = saveHostStorageBytes(filepath.Join(custody, name), raw); err != nil {
				return false, err
			}
		}
		t.Detached, t.Phase = &response.hostReceipt, "detached"
		if err = saveRestrictedMaintenance(dir, *j); err != nil {
			return false, err
		}
	}
	return true, nil
}

func verifyRestrictedCheckpoint(cp hostMaintenanceContextPlan, runtimeDir string) error {
	raw, err := os.ReadFile(cp.ConfigSource)
	if err != nil {
		return err
	}
	if hostDigest(raw) != cp.Original.ConfigSHA256 {
		return errors.New("original admitted config changed during checkpoint capture")
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return err
	}
	snapshot, err := reducer.ReadSnapshot(filepath.Join(runtimeDir, "state.snapshot.json"))
	if err != nil {
		return err
	}
	if snapshot.EffectLedger == nil || snapshot.GeneratedAt.IsZero() {
		return errors.New("native checkpoint is incomplete")
	}
	for _, id := range cfg.Runtime.Process.AdmittedOrders {
		order, ok := snapshot.State.Orders[id]
		if !ok || order.OrderID != id {
			return fmt.Errorf("native checkpoint lacks original admitted order %s; retain held and read back", id)
		}
	}
	return nil
}

func restrictedImportedPool(old hostPool, source hostGeneration, j restrictedMaintenance) (hostPool, error) {
	raw, err := json.Marshal(old)
	if err != nil {
		return old, err
	}
	var p hostPool
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	for id, g := range p.Generations {
		g.State = "exited"
		p.Generations[id] = g
	}
	p.Generations[source.ID] = source
	p.Active = ""
	for id, owner := range p.Owners {
		if owner.MaintenanceHold == "" {
			owner.MaintenanceHold = j.Plan.PlanSHA
		}
		p.Owners[id] = owner
	}
	for i, q := range p.Queue {
		if q.State == "queued" || q.State == "starting" || q.State == "blocked" {
			q.MaintenanceState = q.State
			q.State = "maintenance-held"
			p.Queue[i] = q
		}
	}
	for _, t := range j.Tasks {
		if t.Detached == nil || t.Round == nil || t.Round.DispatchState != loop.StatePaused || t.Detached.TerminalCause != loop.TerminalMigrationDetach || !hostReceiptMatches(bootstrapTaskRequest(hostBootstrapTask{Original: t.Plan.Original}), *t.Detached) {
			return p, errors.New("original held task lacks verified native detach")
		}
		if previous, exists := p.Owners[t.Plan.Original.RequestID]; exists && previous.Original != t.Plan.Original {
			return p, errors.New("original request conflicts with historical pool owner")
		}
		var b hostRequestBinding
		raw, err := os.ReadFile(t.Plan.RequestFile)
		if err != nil {
			return p, err
		}
		if err = json.Unmarshal(raw, &b); err != nil {
			return p, err
		}
		p.Owners[t.Plan.Original.RequestID] = hostTaskOwner{Original: t.Plan.Original, Current: *t.Detached, State: "ready", RoundState: t.Round, EnvironmentSHA: b.EnvironmentSHA256, MaintenanceHold: j.Plan.PlanSHA}
	}
	return p, nil
}

func replaceRestrictedPool(root, custody, originalDigest string, next hostPool) error {
	lock, err := lockfile.TryLock(filepath.Join(root, "pool.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	raw, err := os.ReadFile(filepath.Join(root, "pool.json"))
	if err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if bytes.Equal(raw, data) {
		return nil
	}
	intentPath := filepath.Join(custody, "pool-import.json")
	if _, err = os.Stat(intentPath); err == nil {
		return errors.New("pool import outcome unresolved; readback only, no rewrite")
	} else if !os.IsNotExist(err) {
		return err
	}
	if hostDigest(raw) != originalDigest {
		return errors.New("original pool changed; held import refused")
	}
	intent, err := json.Marshal(map[string]string{"before": originalDigest, "after": hostDigest(data)})
	if err != nil {
		return err
	}
	if err = filex.WriteFileAtomicDurable(intentPath, intent); err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(root, "pool.json"), data)
}

func publishRestrictedGate(root string) error {
	path := filepath.Join(root, "host.sock")
	if target, err := os.Readlink(path); err == nil {
		if target != filepath.Join(root, "maintenance.sock") {
			return errors.New("public Host entry changed; no replacement was performed")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return errors.New("public Host entry is no longer the original fenced entry")
	}
	return publishMaintenanceSocket(root)
}

func verifyRestrictedTarget(ctx context.Context, j restrictedMaintenance) error {
	raw, err := nativeHostRPC(ctx, j.Generation.Directory, hostRequest{Operation: "resources"})
	if err != nil {
		return err
	}
	var observed struct {
		Binary    string             `json:"binary_sha256"`
		Instance  string             `json:"host_instance_id"`
		PID       int                `json:"host_pid"`
		Contexts  int                `json:"contexts"`
		Resources admission.Snapshot `json:"resources"`
	}
	if err = json.Unmarshal(raw, &observed); err != nil {
		return err
	}
	if observed.Instance != j.Generation.ID || observed.PID != j.Generation.Process.PID || observed.Binary != j.Plan.TargetSHA || observed.Contexts != 0 || observed.Resources.AdmissionPolicy != admission.WorkerAdmissionPolicy || observed.Resources.SwapUsedLimit != admission.DefaultSwapUsedLimit || observed.Resources.Allocations != 0 {
		return errors.New("new Host policy, ownership or zero-dispatch readback differs")
	}
	if _, err = legacyProcessIdentity(ctx, observed.PID, j.Plan.Target); err != nil {
		return err
	}
	p, err := readHostPool(j.Plan.Root)
	if err != nil {
		return err
	}
	for _, owner := range p.Owners {
		if owner.MaintenanceHold == "" {
			return errors.New("business owner was not preserved held")
		}
	}
	return nil
}
