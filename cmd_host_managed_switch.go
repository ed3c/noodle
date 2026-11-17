package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

type managedHostSwitch struct {
	Source         hostGeneration              `json:"source"`
	Native         procx.NativeIdentity        `json:"native"`
	SourceBindings admission.PreparationSource `json:"source_bindings"`
	Claim          admission.ResourceClaim     `json:"claim"`
	OwnersSHA      string                      `json:"owners_sha256"`
	Preparation    admission.PreparationRef    `json:"target_preparation"`
	RoutingFenced  bool                        `json:"routing_fenced,omitempty"`
}

type managedSwitchLaunchKey struct{}
type managedHostSwitchNative struct {
	emptyHostRetirementNative
	prepare func(context.Context, *admission.Budget, admission.PreparationSource, admission.ResourceClaim) (admission.PreparationRef, error)
	launch  func(context.Context, string, string, string) (hostGeneration, error)
}

func nativeManagedHostSwitch() managedHostSwitchNative {
	return managedHostSwitchNative{emptyHostRetirementNative: nativeEmptyHostRetirement(),
		prepare: func(ctx context.Context, b *admission.Budget, source admission.PreparationSource, claim admission.ResourceClaim) (admission.PreparationRef, error) {
			return b.PrepareMachineResources(ctx, source, claim, admission.NewSampler())
		},
		launch: launchHostGeneration}
}

func managedOwnersSHA(owners map[string]hostTaskOwner, hold string) (string, error) {
	before := make(map[string]hostTaskOwner, len(owners))
	for id, owner := range owners {
		if owner.MaintenanceHold != hold {
			return "", errors.New("original managed switch owner hold changed")
		}
		owner.MaintenanceHold = ""
		before[id] = owner
	}
	raw, err := json.Marshal(before)
	return hostDigest(raw), err
}

func managedSourcePool(p hostPool, q hostVersionRequest) error {
	m := q.Managed
	g, ok := p.Generations[m.Source.ID]
	if !ok || g.Preparation == nil || m.Source.Preparation == nil || *g.Preparation != *m.Source.Preparation || g.Process != m.Source.Process || g.Binary != m.Source.Binary || g.SHA != m.Source.SHA || g.SourceService != nil {
		return errors.New("original managed switch source generation changed")
	}
	hold := ""
	if m.RoutingFenced {
		hold = q.ID
	}
	sha, err := managedOwnersSHA(p.Owners, hold)
	if err != nil || sha != m.OwnersSHA {
		return errors.New("original managed switch task history changed")
	}
	if !m.RoutingFenced && (p.Active != g.ID || g.State != "active") || m.RoutingFenced && (p.Active != "" && p.Active != q.Generation || g.State != "retiring" && g.State != "exiting" && g.State != "exited") {
		return errors.New("original managed switch routing changed")
	}
	for id, other := range p.Generations {
		if id != g.ID && id != q.Generation && generationLive(other) {
			return errors.New("original managed switch has another live generation")
		}
	}
	for _, other := range p.Queue {
		if other.ID != q.ID && (other.State == "queued" || other.State == "starting") {
			return errors.New("original managed switch has another pending version")
		}
	}
	return nil
}

func managedSwitchRequest(p hostPool, id string) (hostVersionRequest, error) {
	for _, q := range p.Queue {
		if q.ID == id && q.Managed != nil && q.State == "starting" {
			return q, nil
		}
	}
	return hostVersionRequest{}, errors.New("original managed switch continuation changed")
}

func updateManagedSwitch(root, id string, apply func(*hostPool, *hostVersionRequest) error) (hostVersionRequest, error) {
	var result hostVersionRequest
	err := updateHostPool(root, func(p *hostPool) error {
		q, err := managedSwitchRequest(*p, id)
		if err != nil {
			return err
		}
		if err = managedSourcePool(*p, q); err != nil {
			return err
		}
		if err = apply(p, &q); err != nil {
			return err
		}
		for i := range p.Queue {
			if p.Queue[i].ID == id {
				p.Queue[i] = q
				result = q
				return nil
			}
		}
		return errors.New("original managed switch queue disappeared")
	})
	return result, err
}

func verifyManagedSwitchSources(root string, q hostVersionRequest) error {
	source := q.Managed.SourceBindings
	profile, err := loopruntime.ReadGoalOpenShellProfile(root)
	if err != nil {
		return err
	}
	if source.Purpose != "host-resident" || source.Bindings["host_root"] != root || source.Bindings["template_path"] != profile.Template.Path || source.Bindings["template_sha256"] != profile.Template.SHA256 || source.Bindings["template_policy"] != profile.TemplatePolicySHA256 {
		return errors.New("original managed switch profile source differs")
	}
	refs := map[string]loopruntime.SessionReadbackFileRef{
		"goal":     {Path: source.Bindings["goal_path"], SHA256: source.Bindings["goal_sha256"]},
		"template": profile.Template, "target": {Path: q.Binary, SHA256: q.SHA},
	}
	if path := source.Bindings["operation_path"]; path != "" {
		refs["operation"] = loopruntime.SessionReadbackFileRef{Path: path, SHA256: source.Bindings["operation_sha256"]}
	}
	return loopruntime.VerifySessionReadbackFiles(refs)
}

// switchManagedHost continues the existing switch in the foreground. The old
// actor only consumes queued requests, so this owner claims starting atomically.
func switchManagedHost(ctx context.Context, root, binary, digest string, native managedHostSwitchNative) (hostVersionRequest, bool, error) {
	p, err := readHostPool(root)
	if err != nil {
		return hostVersionRequest{}, false, nil
	}
	managed := false
	for _, q := range p.Queue {
		if q.Managed != nil && q.State == "starting" {
			managed = true
		}
	}
	active := p.Generations[p.Active]
	if active.Preparation != nil && active.SourceService == nil {
		managed = true
	}
	if !managed {
		return hostVersionRequest{}, false, nil
	}
	var result hostVersionRequest
	fail := func(err error) (hostVersionRequest, bool, error) {
		result.Error = err.Error()
		return result, true, err
	}
	lock, err := lockfile.Acquire(ctx, filepath.Join(root, "managed-switch.lock"))
	if err != nil {
		return fail(err)
	}
	defer lock.Close()
	p, err = readHostPool(root)
	if err != nil {
		return fail(err)
	}
	for _, q := range p.Queue {
		if q.Managed != nil && q.State == "starting" {
			if q.Binary != binary || q.SHA != digest {
				return fail(errors.New("original managed switch has a fixed target; inspect its continuation"))
			}
			result = q
		}
	}
	active = p.Generations[p.Active]
	if result.ID == "" && active.Binary == binary && active.SHA == digest {
		if err = repairPoolSocket(root, active.ID); err != nil {
			return fail(err)
		}
		return hostVersionRequest{Binary: binary, SHA: digest, State: "active", Generation: active.ID}, true, nil
	}
	budget, err := native.budget(root)
	if err != nil {
		return fail(err)
	}
	if result.ID == "" {
		if active.Binary == binary || active.SHA == digest {
			return fail(errors.New("managed switch target must have a distinct immutable path and digest"))
		}
		if active.Preparation == nil || active.SourceService != nil || active.State != "active" || active.Process.PID <= 1 || active.Process.SessionID != active.ID || active.Process.StartedAt.IsZero() {
			return fail(errors.New("original managed switch has no bound active resident"))
		}
		source, claim, effects, e := budget.ReadPreparation(*active.Preparation)
		if e != nil || !effects {
			return fail(errors.New("original managed resident preparation unavailable"))
		}
		_, identity, e := residentRetirementResources(ctx, root, active, native.emptyHostRetirementNative, true)
		if e != nil {
			return fail(e)
		}
		ownersSHA, e := managedOwnersSHA(p.Owners, "")
		if e != nil {
			return fail(e)
		}
		id, e := hostID()
		if e != nil {
			return fail(e)
		}
		q := hostVersionRequest{ID: id, Binary: binary, SHA: digest, State: "starting", Managed: &managedHostSwitch{Source: active, Native: identity, SourceBindings: source, Claim: claim, OwnersSHA: ownersSHA}}
		if e = verifyManagedSwitchSources(root, q); e != nil {
			return fail(e)
		}
		err = updateHostPool(root, func(current *hostPool) error {
			for _, pending := range current.Queue {
				if pending.State != "queued" && pending.State != "starting" {
					continue
				}
				if pending.Managed != nil || pending.Binary != binary || pending.SHA != digest || pending.State != "queued" || pending.Generation != "" {
					return errors.New("original version queue has another continuation")
				}
				q.ID = pending.ID
			}
			if e := managedSourcePool(*current, q); e != nil {
				return e
			}
			for i := range current.Queue {
				if current.Queue[i].ID == q.ID {
					current.Queue[i] = q
					result = q
					return nil
				}
			}
			current.Queue = append(current.Queue, q)
			result = q
			return nil
		})
		if err != nil {
			return fail(err)
		}
	}
	if err = verifyManagedSwitchSources(root, result); err != nil {
		return fail(err)
	}
	if result.Managed.Preparation.ID == "" {
		bindings := make(map[string]string, len(result.Managed.SourceBindings.Bindings)+2)
		for key, value := range result.Managed.SourceBindings.Bindings {
			bindings[key] = value
		}
		bindings["binary_path"], bindings["binary_sha256"] = binary, digest
		bindings["managed_switch"], bindings["managed_switch_source"] = result.ID, result.Managed.Source.Preparation.ID
		ref, e := native.prepare(ctx, budget, admission.PreparationSource{Purpose: "host-resident", Bindings: bindings}, result.Managed.Claim)
		if e != nil {
			return fail(e)
		}
		result, err = updateManagedSwitch(root, result.ID, func(_ *hostPool, q *hostVersionRequest) error {
			if q.Managed.Preparation.ID != "" && q.Managed.Preparation != ref {
				return errors.New("original managed successor nonce changed")
			}
			q.Managed.Preparation = ref
			return nil
		})
		if err != nil {
			return fail(err)
		}
	}
	m := result.Managed
	fence := admission.ManagedResidentSwitchFence{HostRoot: root, SourceGeneration: m.Source.ID, SourceBinary: admission.MachineSettlementFile{Path: m.Source.Binary, SHA256: m.Source.SHA}, Request: result.ID, TargetBinary: admission.MachineSettlementFile{Path: binary, SHA256: digest}, Preparation: m.Preparation}
	if !m.RoutingFenced {
		_, identity, e := residentRetirementResources(ctx, root, m.Source, native.emptyHostRetirementNative, true)
		if e != nil {
			return fail(e)
		}
		if !m.Native.SameGeneration(identity) {
			return fail(errors.New("original managed resident kernel generation changed"))
		}
	}
	if err = budget.FenceManagedResidentSwitch(*m.Source.Preparation, fence, func(source admission.PreparationSource) error {
		if !reflect.DeepEqual(source, m.SourceBindings) {
			return errors.New("original managed resident source changed")
		}
		current, e := readHostPool(root)
		if e != nil {
			return e
		}
		if e = managedSourcePool(current, result); e != nil {
			return e
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	result, err = updateManagedSwitch(root, result.ID, func(p *hostPool, q *hostVersionRequest) error {
		if q.Managed.RoutingFenced {
			return nil
		}
		for id, owner := range p.Owners {
			owner.MaintenanceHold = q.ID
			p.Owners[id] = owner
		}
		old := p.Generations[q.Managed.Source.ID]
		old.State = "retiring"
		p.Generations[old.ID] = old
		p.Active = ""
		q.Managed.RoutingFenced = true
		return nil
	})
	if err != nil {
		return fail(err)
	}
	// The old actor can now take its existing inactive, zero-context exit path.
	// Never wait for it with the pool lock held: a timeout poisons its rolling loop.
	if _, _, e := residentRetirementResources(ctx, root, m.Source, native.emptyHostRetirementNative, true); e != nil {
		absent, absentErr := native.absent(m.Source.Process)
		if absentErr != nil || !absent {
			return fail(fmt.Errorf("original managed source barrier pending; no launch: %w", e))
		}
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		current, e := readHostPool(root)
		if e != nil {
			return fail(e)
		}
		q, e := managedSwitchRequest(current, result.ID)
		if e != nil {
			return fail(e)
		}
		if e = managedSourcePool(current, q); e != nil {
			return fail(e)
		}
		old := current.Generations[m.Source.ID]
		absent, absentErr := native.absent(old.Process)
		if absentErr != nil {
			return fail(absentErr)
		}
		if absent && old.ResidentExit != nil {
			if e = returnHostResidentResourcesWithGroupReadback(root, old, budget, native.absent); e != nil {
				return fail(e)
			}
			result, err = updateManagedSwitch(root, result.ID, func(p *hostPool, _ *hostVersionRequest) error {
				old := p.Generations[m.Source.ID]
				old.State = "exited"
				p.Generations[old.ID] = old
				return nil
			})
			if err != nil {
				return fail(err)
			}
			break
		}
		select {
		case <-wait.Done():
			return fail(errors.New("original managed source exit or witness pending; reservation retained"))
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err = verifyManagedSwitchSources(root, result); err != nil {
		return fail(err)
	}
	var target hostGeneration
	if result.Generation == "" {
		if err = budget.StartPreparationEffects(m.Preparation); err != nil {
			return fail(err)
		}
		launchCtx := context.WithValue(ctx, goalPreparationContextKey{}, goalResidentAdmission{root, loopruntime.SessionReadbackFileRef{Path: binary, SHA256: digest}, m.Preparation})
		launchCtx = context.WithValue(launchCtx, managedSwitchLaunchKey{}, result.ID)
		target, err = native.launch(launchCtx, root, binary, digest)
		if err != nil {
			return fail(err)
		}
	} else {
		current, e := readHostPool(root)
		if e != nil {
			return fail(e)
		}
		target = current.Generations[result.Generation]
	}
	current, err := readHostPool(root)
	if err != nil {
		return fail(err)
	}
	pending, err := managedSwitchRequest(current, result.ID)
	if err != nil {
		return fail(err)
	}
	if pending.Generation != target.ID || target.Preparation == nil || *target.Preparation != m.Preparation || target.Binary != binary || target.SHA != digest || target.Process.PID <= 1 || target.State != "starting" {
		return fail(errors.New("original managed target start is unknown; read original generation, no replay"))
	}
	if _, _, err = residentRetirementResources(ctx, root, target, native.emptyHostRetirementNative, true); err != nil {
		return fail(err)
	}
	result, err = updateManagedSwitch(root, result.ID, func(p *hostPool, q *hostVersionRequest) error {
		g := p.Generations[target.ID]
		if g.Process != target.Process || g.State != "starting" || g.Preparation == nil || *g.Preparation != m.Preparation || p.Active != "" {
			return errors.New("original managed target changed before activation")
		}
		g.State = "active"
		p.Generations[g.ID] = g
		p.Active = g.ID
		q.State = "active"
		q.Error = ""
		return nil
	})
	if err != nil {
		return fail(err)
	}
	if err = repairPoolSocket(root, target.ID); err != nil {
		return fail(err)
	}
	return result, true, nil
}
