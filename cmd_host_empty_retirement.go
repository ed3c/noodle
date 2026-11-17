package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type emptyHostRetirementIntent struct {
	Schema         int                                `json:"schema"`
	Goal           loopruntime.SessionReadbackFileRef `json:"goal"`
	Selection      loopruntime.SessionReadbackFileRef `json:"selection_before"`
	Selected       goalHostSelection                  `json:"selected"`
	Generation     hostGeneration                     `json:"generation"`
	Native         procx.NativeIdentity               `json:"native"`
	Resources      loopruntime.SessionReadbackFileRef `json:"resources_before"`
	Inventory      loopruntime.SessionReadbackFileRef `json:"machine_before"`
	TemplatePolicy string                             `json:"template_policy"`
}

type emptyHostRetirementJournal struct {
	Schema int                                `json:"schema"`
	Intent loopruntime.SessionReadbackFileRef `json:"intent"`
	Phase  string                             `json:"phase"`
}

type emptyHostRetirementNative struct {
	rpc     func(context.Context, string, hostRequest) (json.RawMessage, error)
	observe func(int) (procx.NativeIdentity, error)
	budget  func(string) (*admission.Budget, error)
	absent  func(procx.ProcessRecord) (bool, error)
	verify  func(map[string]loopruntime.SessionReadbackFileRef) error
}

func nativeEmptyHostRetirement() emptyHostRetirementNative {
	return emptyHostRetirementNative{rpc: nativeHostRPC, observe: procx.ObserveNativeIdentity,
		absent: func(process procx.ProcessRecord) (bool, error) { return process.GroupAbsent() },
		budget: func(root string) (*admission.Budget, error) {
			directory, err := admission.DefaultMachineCapacityDirectory()
			if err != nil {
				return nil, err
			}
			if _, err = loopruntime.ReadSessionReadbackFile(filepath.Join(directory, "capacity.json"), nil); err != nil {
				return nil, err
			}
			return preparationBudget(root + "/resident")
		}}
}

func newHostEmptyRetirementCmd() *cobra.Command {
	var goal string
	var apply bool
	cmd := &cobra.Command{Use: "retire-empty", Short: "Retire one proven unused Goal Host and return its original resident reservation", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cache, err := os.UserCacheDir()
			if err != nil {
				return err
			}
			result, err := retireEmptyGoalHost(cmd.Context(), filepath.Join(cache, "noodle"), goal, apply, nativeEmptyHostRetirement())
			if encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); encodeErr != nil {
				return encodeErr
			}
			return err
		}}
	cmd.Flags().StringVar(&goal, "goal-file", "", "Original immutable Goal source")
	cmd.Flags().BoolVar(&apply, "apply", false, "Continue this original empty Host retirement; unknown effects are read back")
	_ = cmd.MarkFlagRequired("goal-file")
	return cmd
}

func retirementImmutableFile(path string, raw []byte) (loopruntime.SessionReadbackFileRef, error) {
	if before, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(before, raw) {
			return loopruntime.SessionReadbackFileRef{}, errors.New("original empty retirement immutable source changed")
		}
	} else if !os.IsNotExist(err) {
		return loopruntime.SessionReadbackFileRef{}, err
	} else if err = filex.WriteFileAtomicDurable(path, raw); err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	return loopruntime.ReadSessionReadbackFile(path, nil)
}

func sameEmptyRetirementSelection(before, current []byte) bool {
	decode := func(raw []byte) (map[string]any, error) {
		var value map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err := decoder.Decode(&value)
		delete(value, "phase")
		return value, err
	}
	a, err := decode(before)
	if err != nil {
		return false
	}
	b, err := decode(current)
	return err == nil && reflect.DeepEqual(a, b)
}

func saveEmptyRetirementGoalPhase(path string, original []byte, phase string) error {
	current, err := os.ReadFile(path)
	if err != nil || !sameEmptyRetirementSelection(original, current) {
		return errors.New("original empty retirement Goal selection changed")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(current, &fields); err != nil {
		return err
	}
	fields["phase"], _ = json.Marshal(phase)
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, raw)
}

func emptyRetirementPool(root string, selected goalHostSelection) (hostPool, hostGeneration, error) {
	pool, err := readHostPool(root)
	if err != nil {
		return pool, hostGeneration{}, err
	}
	if len(pool.Generations) != 1 || len(pool.Owners) != 0 || len(pool.Queue) != 0 {
		return pool, hostGeneration{}, errors.New("empty retirement has other generations, requests or version queue")
	}
	for _, generation := range pool.Generations {
		if generation.SourceService != nil || generation.Preparation == nil || *generation.Preparation != selected.Preparation || generation.Binary != selected.Noodle.Path || generation.SHA != selected.Noodle.SHA256 || generation.Process.PID <= 1 || generation.Process.SessionID != generation.ID || generation.Process.StartedAt.IsZero() {
			return pool, generation, errors.New("original empty resident identity is missing, foreign or protected")
		}
		if pool.Active != generation.ID && pool.Active != "" {
			return pool, generation, errors.New("original empty resident active identity changed")
		}
		if pool.Active == "" && generation.State != "retiring" && generation.State != "exiting" && generation.State != "exited" || pool.Active == generation.ID && generation.State != "active" {
			return pool, generation, errors.New("original empty resident phase is unknown")
		}
		for _, directory := range []string{"controls", "openshell/custody"} {
			entries, readErr := os.ReadDir(filepath.Join(root, directory))
			if readErr != nil && !os.IsNotExist(readErr) || len(entries) != 0 {
				return pool, generation, errors.New("original empty Host has control or custody evidence; retirement refused")
			}
		}
		return pool, generation, nil
	}
	return pool, hostGeneration{}, errors.New("original empty resident generation missing")
}

func emptyRetirementResources(ctx context.Context, root string, generation hostGeneration, native emptyHostRetirementNative) (json.RawMessage, procx.NativeIdentity, error) {
	return residentRetirementResources(ctx, root, generation, native, false)
}

func residentRetirementResources(ctx context.Context, root string, generation hostGeneration, native emptyHostRetirementNative, completed bool) (json.RawMessage, procx.NativeIdentity, error) {
	raw, err := native.rpc(ctx, generation.Directory, hostRequest{Operation: "resources"})
	if err != nil {
		return raw, procx.NativeIdentity{}, err
	}
	var observed struct {
		Protocol     string          `json:"protocol"`
		Root         string          `json:"host_root"`
		Dir          string          `json:"host_dir"`
		PID          int             `json:"host_pid"`
		Instance     string          `json:"host_instance_id"`
		Binary       string          `json:"binary_path"`
		SHA          string          `json:"binary_sha256"`
		Contexts     *int            `json:"contexts"`
		Rolling      int             `json:"rolling_host_version"`
		RollingError string          `json:"rolling_error"`
		Source       json.RawMessage `json:"source_service"`
		Shutdown     *struct {
			Requested *bool `json:"requested"`
		} `json:"shutdown"`
		Residents *struct {
			Records []admission.Resident `json:"records"`
			Error   *string              `json:"error"`
		} `json:"residents"`
		Resources *admission.Snapshot `json:"resources"`
	}
	if err = json.Unmarshal(raw, &observed); err != nil {
		return raw, procx.NativeIdentity{}, err
	}
	if observed.Protocol != hostProtocol || observed.Root != root || observed.Dir != generation.Directory || observed.PID != generation.Process.PID || observed.Instance != generation.ID || observed.Binary != generation.Binary || observed.SHA != generation.SHA || observed.Contexts == nil || *observed.Contexts != 0 || observed.Rolling != 1 || observed.RollingError != "" || len(observed.Source) != 0 || observed.Shutdown == nil || observed.Shutdown.Requested == nil || *observed.Shutdown.Requested || observed.Residents == nil || observed.Residents.Error != nil || observed.Residents.Records == nil || !completed && len(observed.Residents.Records) != 0 || observed.Resources == nil {
		return raw, procx.NativeIdentity{}, errors.New("original empty Host resources are foreign, busy or unknown")
	}
	for _, resident := range observed.Residents.Records {
		if resident.State != "exited" || len(resident.Contexts) != 0 {
			return raw, procx.NativeIdentity{}, errors.New("original resident effects or consumers unresolved")
		}
		for _, consumer := range resident.ConsumerLeases {
			if consumer.State != "released" {
				return raw, procx.NativeIdentity{}, errors.New("original resident consumer lease unresolved")
			}
		}
	}
	resources := observed.Resources
	if resources.Allocations != 0 || resources.Active != 0 || resources.Residents != 0 || resources.UnresolvedResidents != 0 || resources.OwnedAllocations != 0 || resources.SharedQuota == nil || resources.SharedQuota.Error != "" || resources.SharedQuota.Allocations != 0 || resources.SharedQuota.Maximum != admission.DefaultWorkerLimit {
		return raw, procx.NativeIdentity{}, errors.New("original empty Host quota or consumers are not positively empty")
	}
	identity, err := native.observe(observed.PID)
	if err != nil || !identity.SameGeneration(identity) || identity.UID != os.Getuid() || identity.Zombie || identity.Stopped {
		return raw, identity, errors.New("original empty Host kernel identity unavailable")
	}
	return raw, identity, nil
}

func retireEmptyGoalHost(ctx context.Context, ownerRoot, goalFile string, apply bool, native emptyHostRetirementNative) (map[string]any, error) {
	result := map[string]any{"schema": 1, "owner": "Noodle.Host", "status": "refused", "effects": []string{}}
	fail := func(err error) (map[string]any, error) { result["error"] = err.Error(); return result, err }
	goal, err := loopruntime.ReadSessionReadbackFile(goalFile, nil)
	if err != nil {
		return fail(err)
	}
	selectionPath := filepath.Join(ownerRoot, "goals", hostDigest([]byte(goal.Path+"\x00"+goal.SHA256))+".json")
	var selected goalHostSelection
	if _, err = loopruntime.ReadSessionReadbackFile(selectionPath, &selected); err != nil || selected.Goal != goal {
		return fail(errors.New("original empty retirement Goal selection unavailable"))
	}
	if selected.Phase != "ready" && selected.Phase != "retiring" && selected.Phase != "retired" {
		return fail(errors.New("original Goal Host has an unresolved preparation"))
	}
	root := selected.Root
	if canonical, e := filepath.EvalSymlinks(root); e != nil || canonical != root || !filepath.IsAbs(root) {
		return fail(errors.New("original empty Host root is not canonical"))
	}
	result["goal"], result["host_root"] = goal, root
	result["next"] = map[string]any{"owner": "Noodle.Host", "operation": "empty_host_retirement", "argv": []string{os.Args[0], "host", "retire-empty", "--goal-file", goal.Path, "--apply"}}
	journalPath, intentPath := filepath.Join(root, "empty-retirement.json"), filepath.Join(root, "empty-retirement-intent.json")
	var journal emptyHostRetirementJournal
	_, journalErr := loopruntime.ReadSessionReadbackFile(journalPath, &journal)
	if journalErr != nil && !os.IsNotExist(journalErr) {
		return fail(journalErr)
	}
	var savedIntent emptyHostRetirementIntent
	if journalErr == nil {
		ref, e := loopruntime.ReadSessionReadbackFile(intentPath, &savedIntent)
		if e != nil || ref != journal.Intent || journal.Schema != 1 || savedIntent.Schema != 1 || savedIntent.Goal != goal || savedIntent.Selected.Root != root || journal.Phase != "prepared" && journal.Phase != "fenced" && journal.Phase != "retired" {
			return fail(errors.New("original empty retirement intent is missing, foreign or changed"))
		}
		original, e := os.ReadFile(savedIntent.Selection.Path)
		current, currentErr := os.ReadFile(selectionPath)
		if e != nil || currentErr != nil || hostDigest(original) != savedIntent.Selection.SHA256 || !sameEmptyRetirementSelection(original, current) {
			return fail(errors.New("original empty retirement Goal history changed"))
		}
		if apply {
			result["status"], result["effects_possible"] = "unknown", true
		}
	}
	if !apply {
		if journalErr == nil {
			if journal.Phase != "prepared" {
				budget, e := native.budget(root)
				if e != nil {
					return fail(e)
				}
				fence := admission.EmptyResidentRetirementFence{HostRoot: root, Generation: savedIntent.Generation.ID, Intent: admission.MachineSettlementFile(journal.Intent), Inventory: admission.MachineSettlementFile(savedIntent.Inventory)}
				closed, e := budget.ReadEmptyResidentRetirement(*savedIntent.Generation.Preparation, fence)
				if e != nil || journal.Phase == "retired" && !closed {
					return fail(errors.New("original empty retirement machine closure unavailable"))
				}
			}
			result["status"], result["journal"] = journal.Phase, journal
			return result, nil
		}
		pool, generation, e := emptyRetirementPool(root, selected)
		if e != nil || pool.Active != generation.ID || generation.State != "active" {
			return fail(errors.New("original empty Host is not an active unused generation"))
		}
		if _, _, e = emptyRetirementResources(ctx, root, generation, native); e != nil {
			return fail(e)
		}
		result["status"] = "empty_actor_observed"
		return result, nil
	}
	goalLock, err := lockfile.Acquire(ctx, selectionPath+".lock")
	if err != nil {
		return fail(err)
	}
	defer goalLock.Close()
	var lockedSelection goalHostSelection
	if _, err = loopruntime.ReadSessionReadbackFile(selectionPath, &lockedSelection); err != nil || !reflect.DeepEqual(lockedSelection, selected) {
		return fail(errors.New("original empty retirement Goal selection changed before admission"))
	}
	var initialResources json.RawMessage
	var initialIdentity procx.NativeIdentity
	var initialTemplatePolicy string
	if journalErr != nil {
		reservation, e := loopruntime.ReadGoalOpenShellProfile(root)
		if e != nil || reservation.Profile != selected.Profile || reservation.Template != selected.Template {
			return fail(errors.New("original empty Host profile changed"))
		}
		initialTemplatePolicy = reservation.TemplatePolicySHA256
		binary, e := hostReadbackBinaryRef(selected.Noodle.Path)
		if e != nil || binary != selected.Noodle {
			return fail(errors.New("original empty Host binary changed"))
		}
		_, initialGeneration, e := emptyRetirementPool(root, selected)
		if e != nil {
			return fail(e)
		}
		initialResources, initialIdentity, e = emptyRetirementResources(ctx, root, initialGeneration, native)
		if e != nil {
			return fail(e)
		}
	}
	poolLock, err := lockfile.Acquire(ctx, filepath.Join(root, "pool.lock"))
	if err != nil {
		return fail(err)
	}
	locked := true
	defer func() {
		if locked {
			poolLock.Close()
		}
	}()
	pool, generation, err := emptyRetirementPool(root, selected)
	if err != nil {
		return fail(err)
	}
	budget, err := native.budget(root)
	if err != nil {
		return fail(err)
	}
	var intent emptyHostRetirementIntent
	if journalErr == nil {
		ref, e := loopruntime.ReadSessionReadbackFile(intentPath, &intent)
		if e != nil || ref != journal.Intent || journal.Schema != 1 || intent.Schema != 1 || intent.Goal != goal || intent.Selected.Root != root || intent.Generation.ID != generation.ID || intent.Generation.Process != generation.Process || intent.Generation.Binary != generation.Binary || intent.Generation.SHA != generation.SHA {
			return fail(errors.New("original empty retirement intent binding changed"))
		}
	} else {
		if pool.Active != generation.ID || generation.State != "active" || selected.Phase != "ready" {
			return fail(errors.New("original empty resident is not eligible for a new intent"))
		}
		before, e := os.ReadFile(selectionPath)
		if e != nil {
			return fail(e)
		}
		result["status"], result["effects_possible"] = "unknown", true
		selection, e := retirementImmutableFile(filepath.Join(root, "empty-retirement-goal-before.json"), before)
		if e != nil {
			return fail(e)
		}
		resources, e := retirementImmutableFile(filepath.Join(root, "empty-retirement-resources-before.json"), initialResources)
		if e != nil {
			return fail(e)
		}
		snapshot := budget.Snapshot().MachineCapacity
		if snapshot == nil || snapshot.ReadbackSource == "" {
			return fail(errors.New("original machine inventory unavailable"))
		}
		machine, e := os.ReadFile(snapshot.ReadbackSource)
		if e != nil {
			return fail(e)
		}
		inventory, e := retirementImmutableFile(filepath.Join(root, "empty-retirement-machine-before.json"), machine)
		if e != nil {
			return fail(e)
		}
		intent = emptyHostRetirementIntent{Schema: 1, Goal: goal, Selection: selection, Selected: selected, Generation: generation, Native: initialIdentity, Resources: resources, Inventory: inventory, TemplatePolicy: initialTemplatePolicy}
		encoded, e := json.Marshal(intent)
		if e != nil {
			return fail(e)
		}
		ref, e := retirementImmutableFile(intentPath, encoded)
		if e != nil {
			return fail(e)
		}
		journal = emptyHostRetirementJournal{1, ref, "prepared"}
		if err = saveEmptyHostRetirement(journalPath, journal); err != nil {
			return fail(err)
		}
	}
	before, err := os.ReadFile(intent.Selection.Path)
	if err != nil || hostDigest(before) != intent.Selection.SHA256 {
		return fail(errors.New("original empty retirement Goal preimage changed"))
	}
	if selected.Phase != "retired" {
		result["status"], result["effects_possible"] = "unknown", true
		if err = saveEmptyRetirementGoalPhase(selectionPath, before, "retiring"); err != nil {
			return fail(err)
		}
	}
	result["effects"] = []string{"original_goal_retirement_fence"}
	fence := admission.EmptyResidentRetirementFence{HostRoot: root, Generation: generation.ID, Intent: admission.MachineSettlementFile(journal.Intent), Inventory: admission.MachineSettlementFile(intent.Inventory)}
	if err = budget.FenceEmptyResidentPreparation(*generation.Preparation, fence, func(source admission.PreparationSource) error {
		if source.Bindings["host_root"] != root || source.Bindings["native_root"] != ownerRoot || source.Bindings["binary_path"] != generation.Binary || source.Bindings["binary_sha256"] != generation.SHA || source.Bindings["goal_path"] != goal.Path || source.Bindings["goal_sha256"] != goal.SHA256 || source.Bindings["template_path"] != selected.Template.Path || source.Bindings["template_sha256"] != selected.Template.SHA256 || source.Bindings["template_policy"] != intent.TemplatePolicy {
			return errors.New("original empty resident Goal source differs")
		}
		if selected.Operation != nil && (source.Bindings["operation_path"] != selected.Operation.Path || source.Bindings["operation_sha256"] != selected.Operation.SHA256) {
			return errors.New("original empty resident operation source differs")
		}
		verify := native.verify
		if verify == nil {
			verify = loopruntime.VerifySessionReadbackFiles
		}
		if err := verify(map[string]loopruntime.SessionReadbackFileRef{"goal": goal, "template": selected.Template, "binary": selected.Noodle, "profile": selected.Profile}); err != nil {
			return err
		}
		identity, e := native.observe(generation.Process.PID)
		if e != nil || !intent.Native.SameGeneration(identity) {
			return errors.New("original empty Host kernel generation changed before fencing")
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	result["effects"] = []string{"original_goal_retirement_fence", "original_resident_machine_fence"}
	if pool.Active == generation.ID {
		pool.Active = ""
		generation.State = "retiring"
		pool.Generations[generation.ID] = generation
		encoded, e := json.MarshalIndent(pool, "", "  ")
		if e != nil {
			return fail(e)
		}
		if err = filex.WriteFileAtomicDurable(filepath.Join(root, "pool.json"), encoded); err != nil {
			return fail(err)
		}
	}
	result["effects"] = []string{"original_goal_retirement_fence", "original_resident_machine_fence", "original_host_routing_fence"}
	// Never wait on the actor while holding its pool lock: its regular socket
	// repair also needs that lock. The machine fence is already durable, so a
	// cached pre-fence attach cannot dispatch. Any owner it records remains
	// unknown and makes the next empty check refuse; it is never deleted.
	poolLock.Close()
	locked = false
	if _, identity, e := emptyRetirementResources(ctx, root, generation, native); e == nil {
		if !intent.Native.SameGeneration(identity) {
			return fail(errors.New("original empty Host kernel generation changed"))
		}
	} else if absent, absenceErr := native.absent(generation.Process); absenceErr != nil || !absent {
		return fail(fmt.Errorf("original empty retirement barrier pending; only readback is legal: %w", e))
	}
	journal.Phase = "fenced"
	if err = saveEmptyHostRetirement(journalPath, journal); err != nil {
		return fail(err)
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		current, currentGeneration, e := emptyRetirementPool(root, selected)
		if e != nil || current.Active != "" {
			return fail(errors.New("original empty retirement fenced pool changed"))
		}
		absent, absenceErr := native.absent(generation.Process)
		if absenceErr == nil && absent && currentGeneration.ResidentExit != nil {
			if _, err = budget.ReadEmptyResidentRetirement(*currentGeneration.Preparation, fence); err != nil {
				return fail(err)
			}
			if err = returnHostResidentResourcesWithGroupReadback(root, currentGeneration, budget, native.absent); err != nil {
				return fail(err)
			}
			if err = saveEmptyRetirementGoalPhase(selectionPath, before, "retired"); err != nil {
				return fail(err)
			}
			journal.Phase = "retired"
			if err = saveEmptyHostRetirement(journalPath, journal); err != nil {
				return fail(err)
			}
			result["status"], result["journal"], result["next"] = "retired", journal, nil
			result["effects"] = []string{"original_goal_retirement_fence", "original_resident_machine_fence", "original_host_routing_fence", "original_actor_exit_witness", "original_resident_reservation_returned"}
			return result, nil
		}
		select {
		case <-wait.Done():
			result["status"], result["journal"] = "unknown", journal
			return fail(errors.New("original empty retirement exit or witness pending; reservation retained"))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func saveEmptyHostRetirement(path string, journal emptyHostRetirementJournal) error {
	raw, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, raw)
}
