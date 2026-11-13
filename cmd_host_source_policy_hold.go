package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

// Only new admission is held. This does not pause a Loop, control a process,
// certify empty compute or perform the unfinished policy revision.
type hostSourcePolicyPlan struct {
	Operation string                             `json:"operation"`
	Snapshot  loopruntime.SessionReadbackFileRef `json:"snapshot"`
}

func readSourcePolicySnapshot(ref loopruntime.SessionReadbackFileRef) (hostOperatorPlan, error) {
	var op hostOperatorPlan
	current, err := loopruntime.ReadSessionReadbackFile(ref.Path, &op)
	if err != nil {
		return op, err
	}
	if current != ref || op.Schema != 1 || op.Executable || len(op.Effects) != 0 || op.NativeScope == nil || op.NativeScope.Schema != 1 {
		return op, errors.New("original Source native-scope snapshot is incomplete or changed")
	}
	return op, nil
}

func prepareSourcePolicyHold(path string) (hostMaintenancePlan, error) {
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		return hostMaintenancePlan{}, err
	}
	op, err := readSourcePolicySnapshot(ref)
	if err != nil {
		return hostMaintenancePlan{}, err
	}
	var source struct {
		legacyHostResources
		Root string `json:"host_root"`
	}
	if err = json.Unmarshal(op.Source, &source); err != nil {
		return hostMaintenancePlan{}, err
	}
	p := hostMaintenancePlan{Schema: 1, Root: source.Root, Source: source.legacyHostResources, Native: &op.NativeScope.Host.Process,
		Target: op.NativeScope.Host.Binary.Path, TargetSHA: op.NativeScope.Host.Binary.SHA256,
		SourcePolicy: &hostSourcePolicyPlan{Operation: "admission-hold", Snapshot: ref}, Files: map[string]string{},
		Contexts: []hostMaintenanceContextPlan{}, Blockers: []string{}, Effects: []string{}, Executable: true,
		Observations: []string{"Only the cooperative Host entry and selected pool owners are held; live Loops, direct inputs and compute are not fixed. No process control, zero-worker/compute proof, operator activation or policy revision"},
		Steps:        []string{"Preserve original pool/bootstrap bytes in the existing maintenance epoch", "Publish the original maintenance marker/gate without moving the generation socket", "Hold only the original Source pool owners without changing outcomes", "Return source-policy-admission-held; later maintenance must still fix every live producer and qualify retirement"}}
	for _, receipt := range op.Contexts {
		p.Contexts = append(p.Contexts, hostMaintenanceContextPlan{Original: receipt})
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	p.PlanSHA = hostDigest(raw)
	return p, validateSourcePolicyHold(p)
}

func validateSourcePolicyHold(p hostMaintenancePlan) error {
	if p.SourcePolicy == nil || p.SourcePolicy.Operation != "admission-hold" {
		return errors.New("Source admission-only operation differs")
	}
	return validateSourcePolicyPlan(p)
}

func validateSourcePolicyPlan(p hostMaintenancePlan) error {
	digest := p.PlanSHA
	p.PlanSHA = ""
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if digest == "" || digest != hostDigest(raw) || p.Schema != 1 || !p.Executable || len(p.Effects) != 0 || len(p.Blockers) != 0 || p.Handover != nil || p.SourcePolicy == nil || p.Native == nil || !p.Native.SameGeneration(*p.Native) || p.Native.UID != os.Getuid() || !filepath.IsAbs(p.Root) || !validPoolID(p.Source.Instance) || p.Source.Protocol != hostProtocol || p.Source.Rolling != 1 || p.Native.PID != p.Source.PID || len(p.Contexts) == 0 || len(p.Contexts) != p.Source.Contexts {
		return errors.New("Source hold plan is invalid, changed or outside its bounded effect")
	}
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		return err
	}
	var source legacyHostResources
	var root struct {
		Root string `json:"host_root"`
	}
	if json.Unmarshal(op.Source, &source) != nil || json.Unmarshal(op.Source, &root) != nil || !reflect.DeepEqual(source, p.Source) || root.Root != p.Root || !p.Native.SameGeneration(op.NativeScope.Host.Process) || len(op.Contexts) != len(p.Contexts) {
		return errors.New("Source hold differs from the original snapshot")
	}
	if p.SourcePolicy.Operation == "admission-hold" {
		if p.Policy != "" || p.Target != op.NativeScope.Host.Binary.Path || p.TargetSHA != op.NativeScope.Host.Binary.SHA256 {
			return errors.New("Source hold target or policy changed")
		}
	} else if p.SourcePolicy.Operation == "operator-service-restore" {
		if err := validateSourceServiceRestorePlan(p, op); err != nil {
			return err
		}
	} else {
		return errors.New("unknown Source maintenance operation")
	}
	seen := map[string]bool{}
	for i, cp := range p.Contexts {
		r := cp.Original
		if !reflect.DeepEqual(r, op.Contexts[i]) || !validPoolID(r.RequestID) || !validPoolID(r.ContextID) || seen[r.RequestID] || r.HostInstanceID != p.Source.Instance || r.HostPID != p.Source.PID || !filepath.IsAbs(r.ProjectDir) || (r.ContextState != "active" && r.ContextState != "failed") {
			return errors.New("Source hold context identity or original state differs")
		}
		seen[r.RequestID] = true
	}
	return nil
}

// Patch only MaintenanceHold. Raw members preserve unknown fields, outcome,
// generation, queue and history instead of importing or detaching any task.
func sourcePolicyHeldPool(raw []byte, p hostMaintenancePlan) ([]byte, error) {
	var pool hostPool
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &pool) != nil || json.Unmarshal(raw, &document) != nil || pool.Schema != 1 || pool.Active != p.Source.Instance || len(p.Contexts) == 0 {
		return nil, errors.New("original Source pool identity changed")
	}
	var owners map[string]json.RawMessage
	if err := json.Unmarshal(document["owners"], &owners); err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	allHeld := true
	for _, cp := range p.Contexts {
		r := cp.Original
		if selected[r.RequestID] {
			return nil, errors.New("Source hold repeats a request")
		}
		selected[r.RequestID] = true
		o, ok := pool.Owners[r.RequestID]
		req := bootstrapTaskRequest(hostBootstrapTask{Original: r})
		if !ok || !hostReceiptMatches(req, o.Current) || o.Current.HostPID != r.HostPID || o.Current.HostInstanceID != r.HostInstanceID || o.Current.BinarySHA256 != r.BinarySHA256 || (o.MaintenanceHold != "" && o.MaintenanceHold != p.PlanSHA) {
			return nil, errors.New("Source pool owner changed or another maintenance epoch holds it")
		}
		allHeld = allHeld && o.MaintenanceHold == p.PlanSHA
		var members map[string]json.RawMessage
		if err := json.Unmarshal(owners[r.RequestID], &members); err != nil {
			return nil, err
		}
		hold, err := json.Marshal(p.PlanSHA)
		if err != nil {
			return nil, err
		}
		members["maintenance_hold"] = hold
		owners[r.RequestID], err = json.Marshal(members)
		if err != nil {
			return nil, err
		}
	}
	for id, owner := range pool.Owners {
		if owner.Current.HostInstanceID == p.Source.Instance && owner.Current.ContextState != "detached" && !selected[id] {
			return nil, errors.New("Source pool has an unplanned live owner")
		}
	}
	if allHeld {
		return raw, nil
	}
	ownerBytes, err := json.Marshal(owners)
	if err != nil {
		return nil, err
	}
	document["owners"] = ownerBytes
	return json.Marshal(document)
}

func verifySourcePolicyNativeOwner(ctx context.Context, p hostMaintenancePlan, op hostOperatorPlan, dir string) error {
	history, contexts, err := sourcePolicyCurrentHistory(op, dir)
	if err != nil {
		return err
	}
	current, err := procx.ObserveNativeIdentity(p.Source.PID)
	if err != nil {
		return err
	}
	if !p.Native.SameGeneration(current) || current.Zombie {
		return errors.New("original Source kernel generation changed")
	}
	raw, err := nativeHostRPC(ctx, filepath.Join(p.Root, "g", p.Source.Instance), hostRequest{Operation: "resources"})
	if err != nil {
		return err
	}
	var observed legacyHostResources
	if json.Unmarshal(raw, &observed) != nil || observed.Protocol != p.Source.Protocol || observed.Rolling != 1 || observed.Instance != p.Source.Instance || observed.PID != p.Source.PID || observed.Contexts != contexts {
		return errors.New("original Source resource identity or context set changed")
	}
	current, err = procx.ObserveNativeIdentity(p.Source.PID)
	if err != nil {
		return err
	}
	if !p.Native.SameGeneration(current) || current.Zombie {
		return errors.New("original Source kernel generation changed during resource readback")
	}
	finalHistory, err := loopruntime.ReadSessionReadbackFile(op.History.Path, nil)
	if err != nil {
		return err
	}
	if finalHistory != history {
		return errors.New("Source history changed during owner readback; retain progress and read back")
	}
	return nil
}

func sourcePolicyHoldPool(root, dir string, before []byte, p hostMaintenancePlan) error {
	_, err := sourcePolicyHeldPool(before, p)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(filepath.Join(root, "pool.json"))
	if err != nil {
		return err
	}
	readback, err := sourcePolicyHeldPool(current, p)
	if err != nil {
		return err
	}
	if bytes.Equal(readback, current) {
		return nil
	}
	// A native outcome may progress before the first hold as well. Archive that
	// exact CAS source instead of restoring the older snapshot's outcome bytes.
	digest := hostDigest(current)
	if err = saveHostStorageBytes(filepath.Join(dir, "pool-before-"+digest+".json"), current); err != nil {
		return err
	}
	return replaceRestrictedPoolBytes(root, dir, digest, readback)
}

// Outcome fields belong to the native owner and may progress while admission
// is held. Keep both full byte images; only immutable identity must stay equal.
func sourcePolicyCurrentHistory(op hostOperatorPlan, dir string) (loopruntime.SessionReadbackFileRef, int, error) {
	fail := func(err error) (loopruntime.SessionReadbackFileRef, int, error) {
		return loopruntime.SessionReadbackFileRef{}, 0, err
	}
	before, err := os.ReadFile(filepath.Join(dir, "original-history.json"))
	if err != nil {
		return fail(err)
	}
	if hostDigest(before) != op.History.SHA256 {
		return fail(errors.New("original Source history before-image changed"))
	}
	ref, err := loopruntime.ReadSessionReadbackFile(op.History.Path, nil)
	if err != nil {
		return fail(err)
	}
	raw, err := os.ReadFile(ref.Path)
	if err != nil {
		return fail(err)
	}
	if hostDigest(raw) != ref.SHA256 {
		return fail(errors.New("current Source history changed during readback"))
	}
	decode := func(data []byte) ([]map[string]any, error) {
		var rows []map[string]any
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		if err := d.Decode(&rows); err != nil {
			return nil, err
		}
		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			return nil, errors.New("Source history contains trailing or invalid JSON")
		}
		return rows, nil
	}
	old, err := decode(before)
	if err != nil {
		return fail(err)
	}
	now, err := decode(raw)
	if err != nil {
		return fail(err)
	}
	if len(old) != len(now) {
		return fail(errors.New("Source history context set changed"))
	}
	identities := map[string]map[string]any{}
	for _, row := range old {
		id, ok := row["context_id"].(string)
		if !ok || id == "" || identities[id] != nil {
			return fail(errors.New("original Source history context identity is ambiguous"))
		}
		identities[id] = row
	}
	contexts := 0
	for _, row := range now {
		id, _ := row["context_id"].(string)
		original := identities[id]
		if original == nil {
			return fail(errors.New("current Source history context identity changed"))
		}
		switch row["context_state"] {
		case "active", "failed":
			contexts++
		case "detached":
		default:
			return fail(errors.New("current Source history outcome is unknown"))
		}
		for _, name := range []string{"context_state", "terminal_cause", "admission_refusal"} {
			delete(original, name)
			delete(row, name)
		}
		if !reflect.DeepEqual(original, row) {
			return fail(errors.New("Source history immutable identity changed"))
		}
		delete(identities, id)
	}
	if err = saveHostStorageBytes(filepath.Join(dir, "history-readback-"+ref.SHA256+".json"), raw); err != nil {
		return fail(err)
	}
	return ref, contexts, nil
}

func sourcePolicyMarker(root, dir string, p hostMaintenancePlan) error {
	identity, err := json.Marshal(p.Native)
	if err != nil {
		return err
	}
	marker := hostBootstrap{Schema: 1, Phase: "source-policy-" + p.PlanSHA, Source: restrictedSource(p), Target: p.Target, TargetSHA: p.TargetSHA, ProcessIdentity: hostDigest(identity)}
	want, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	before, err := os.ReadFile(filepath.Join(dir, "original-bootstrap.json"))
	if err != nil {
		return err
	}
	current, err := os.ReadFile(filepath.Join(root, "bootstrap.json"))
	if err != nil {
		return err
	}
	if bytes.Equal(current, want) {
		return nil
	}
	if !bytes.Equal(current, before) {
		return errors.New("another Host maintenance marker replaced the original; no rewrite")
	}
	return saveHostBootstrap(root, marker)
}

func verifySourcePolicyHoldFiles(op hostOperatorPlan) error {
	return loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"request": op.Request, "config": op.Config, "profile": op.Profile})
}

func verifySourcePolicyCustody(op hostOperatorPlan, dir string) error {
	var before loopruntime.OpenShellCustodyState
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "original-custody.json"), &before)
	if err != nil {
		return err
	}
	if ref.SHA256 != op.Custody.SHA256 || (before.Version != 4 && before.Version != 5) || before.Binding != op.Binding {
		return errors.New("original Source custody before-image or binding changed")
	}
	var current loopruntime.OpenShellCustodyState
	ref, err = loopruntime.ReadSessionReadbackFile(op.Custody.Path, &current)
	if err != nil {
		return err
	}
	if before.Version != current.Version || before.ControlRoot != current.ControlRoot || before.Binding != current.Binding || !reflect.DeepEqual(before.Handover, current.Handover) {
		return errors.New("Source custody control identity changed")
	}
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return err
	}
	if hostDigest(data) != ref.SHA256 {
		return errors.New("Source custody changed during readback")
	}
	return saveHostStorageBytes(filepath.Join(dir, "custody-readback-"+ref.SHA256+".json"), data)
}

func verifySourcePolicyAdmissionHold(root string, p hostMaintenancePlan) error {
	lock, err := lockfile.TryLock(filepath.Join(root, "pool.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	marker, err := readHostBootstrap(root)
	if err != nil || marker.Phase != "source-policy-"+p.PlanSHA || marker.Source.ID != p.Source.Instance || marker.Source.Process.PID != p.Source.PID {
		return errors.New("Source admission marker readback differs")
	}
	alias, err := os.Readlink(filepath.Join(root, "host.sock"))
	if err != nil || alias != filepath.Join(root, "maintenance.sock") {
		return errors.New("Source admission public alias readback differs")
	}
	current, err := os.ReadFile(filepath.Join(root, "pool.json"))
	if err != nil {
		return err
	}
	held, err := sourcePolicyHeldPool(current, p)
	if err != nil {
		return err
	}
	if !bytes.Equal(held, current) {
		return errors.New("Source admission owner hold readback differs")
	}
	return nil
}

func runSourcePolicyHold(ctx context.Context, p hostMaintenancePlan, dir string, out *json.Encoder) error {
	lock, err := lockfile.TryLock(filepath.Join(p.Root, "maintenance.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	return runSourcePolicyHoldLocked(ctx, p, dir, out)
}

func runSourcePolicyHoldLocked(ctx context.Context, p hostMaintenancePlan, dir string, out *json.Encoder) error {
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		return err
	}
	j, err := readRestrictedMaintenance(dir)
	if os.IsNotExist(err) {
		old, err := readHostBootstrap(p.Root)
		if err != nil || old.Phase != "active" {
			return errors.New("another or unreadable Host maintenance marker is present")
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		for name, ref := range map[string]loopruntime.SessionReadbackFileRef{"original-pool.json": op.Pool, "original-bootstrap.json": {Path: filepath.Join(p.Root, "bootstrap.json")}, "original-history.json": op.History, "original-request.json": op.Request, "original-config.toml": op.Config, "original-profile.json": op.Profile, "original-custody.json": op.Custody} {
			raw, err := os.ReadFile(ref.Path)
			if err != nil {
				return err
			}
			if ref.SHA256 != "" && hostDigest(raw) != ref.SHA256 {
				return errors.New("original Source bytes changed before archival")
			}
			if err = saveHostStorageBytes(filepath.Join(dir, name), raw); err != nil {
				return err
			}
		}
		j = restrictedMaintenance{Schema: 1, Plan: p, Phase: "source-policy-prepared"}
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if j.Plan.PlanSHA != p.PlanSHA {
		return errors.New("original Source maintenance epoch differs")
	}
	if err = verifySourcePolicyHoldFiles(op); err != nil {
		return err
	}
	if err = verifySourcePolicyCustody(op, dir); err != nil {
		return err
	}
	if err = verifySourcePolicyNativeOwner(ctx, p, op, dir); err != nil {
		return err
	}
	gate, err := openHostMaintenanceGate(p.Root, filepath.Join(p.Root, "g", p.Source.Instance))
	if err != nil {
		return err
	}
	defer gate.Close()
	if j.Phase == "source-policy-prepared" {
		if err = sourcePolicyMarker(p.Root, dir, p); err != nil {
			return err
		}
		j.Phase = "source-policy-fencing"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	marker, err := readHostBootstrap(p.Root)
	if err != nil || marker.Phase != "source-policy-"+p.PlanSHA || marker.Source.ID != p.Source.Instance || marker.Source.Process.PID != p.Source.PID {
		return errors.New("original Source maintenance marker changed")
	}
	alias, err := os.Readlink(filepath.Join(p.Root, "host.sock"))
	if err != nil || (alias != filepath.Join(p.Root, "g", p.Source.Instance, "host.sock") && alias != filepath.Join(p.Root, "maintenance.sock")) {
		return errors.New("original Source public socket changed; no replacement")
	}
	if err = publishMaintenanceSocket(p.Root); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "original-pool.json"))
	if err != nil || hostDigest(raw) != op.Pool.SHA256 {
		return errors.New("original Source pool before-image changed")
	}
	if err = sourcePolicyHoldPool(p.Root, dir, raw, p); err != nil {
		return err
	}
	if err = verifySourcePolicyNativeOwner(ctx, p, op, dir); err != nil {
		return err
	}
	if err = verifySourcePolicyAdmissionHold(p.Root, p); err != nil {
		return err
	}
	j.Phase = "source-policy-admission-held"
	if err = saveRestrictedMaintenance(dir, j); err != nil {
		return err
	}
	return out.Encode(j)
}
