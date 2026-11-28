package admission

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

const MachineAdmissionPolicy = "machine-capacity-v1"

// The native user's home supplies one pool. Per-Host HOME/XDG settings cannot
// create multiple independent budgets for the same physical machine and user.
func DefaultMachineCapacityDirectory() (string, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" { return "", errors.New("machine capacity directory unsupported") }
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || account.HomeDir == "" { return "", errors.New("native user home unavailable for machine capacity") }
	cache := filepath.Join(account.HomeDir, ".cache")
	if runtime.GOOS == "darwin" { cache = filepath.Join(account.HomeDir, "Library", "Caches") }
	return filepath.Join(cache, "noodle", "machine-capacity"), nil
}

// CapacityPolicy comes from the bound operator configuration, never an order.
// MemoryBytes and CPUs preserve legacy budget values for readback.
type CapacityPolicy struct {
	MemoryBytes uint64 `json:"memory_bytes" toml:"memory_bytes"`
	CPUs uint64 `json:"cpus" toml:"cpus"`
	DiskSafetyBytes uint64 `json:"disk_safety_bytes" toml:"disk_safety_bytes"`
	MaxSwapUsedBytes uint64 `json:"max_swap_used_bytes" toml:"max_swap_used_bytes"`
	MaxSwapoutBytesPerSecond uint64 `json:"max_swapout_bytes_per_second" toml:"max_swapout_bytes_per_second"`
	RecoverySamples int `json:"recovery_samples" toml:"recovery_samples"`
	RequireIOObservation bool `json:"require_io_observation" toml:"require_io_observation"`
}

type ResourceClaim struct {
	Source string
	MemoryBytes, CPUs, DiskBytes uint64
	DiskPaths []string
	Policy CapacityPolicy
}

type ResourceSource func(runtime string) (ResourceClaim, error)

type resourceSource struct {
	runtime string
	read ResourceSource
	source string
	policy CapacityPolicy
	normals int
}

type MachineSnapshot struct {
	Scope string `json:"scope"`
	Uncovered string `json:"uncovered"`
	ReadbackSource string `json:"readback_source"`
	Policy string `json:"policy"`
	Reason string `json:"reason"`
	RAM string `json:"ram_observation"`
	RAMObservationError string `json:"ram_observation_error,omitempty"`
	CPU string `json:"cpu_observation"`
	CPUCountRole string `json:"cpu_count_role"`
	Swap string `json:"swap_observation"`
	Disk string `json:"disk_observation"`
	IO string `json:"io_observation"`
	Leases int `json:"held_leases"`
	MemoryBytes uint64 `json:"held_memory_bytes"`
	CPUs uint64 `json:"held_cpus"`
	Retention string `json:"retention"`
	Next string `json:"next"`
	ObservedMemoryBytes *uint64 `json:"observed_total_memory_bytes"`
	ObservedCPUs *int `json:"observed_cpus"`
	Capacity *CapacityPolicy `json:"operator_capacity"`
	DiskBytes uint64 `json:"held_disk_peak_bytes"`
}

type machineLease struct {
	ManagedSwitch *ManagedResidentSwitchFence `json:"managed_host_switch,omitempty"`
	EmptyRetirement *EmptyResidentRetirementFence `json:"empty_host_retirement,omitempty"`
	Preparation *machinePreparation `json:"preparation,omitempty"`
	PreparationPromoted bool `json:"preparation_promoted,omitempty"`
	PreparationClosed map[string]MachineSettlementFile `json:"preparation_closed,omitempty"`
	ContextID string `json:"context_id,omitempty"`
	OrderID string `json:"order_id,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	Binding *MachineSessionBinding `json:"session_binding,omitempty"`
	Settlement *MachineReclamation `json:"settlement,omitempty"`
	Owner string `json:"owner"`
	Source string `json:"source"`
	MemoryBytes uint64 `json:"memory_bytes"`
	CPUs uint64 `json:"cpus"`
	Disks map[string]uint64 `json:"disk_bytes_by_volume"`
	EffectsPossible bool `json:"effects_possible"`
}

type machineState struct {
	Schema int `json:"schema"`
	Identity string `json:"identity"`
	Policy string `json:"policy"`
	Capacity *CapacityPolicy `json:"capacity"`
	Leases map[string]machineLease `json:"leases"`
}

type diskCapacity struct { Volume string; Available uint64 }
type machineCapacity struct {
	dir, identity, owner string
	probe func(string) (diskCapacity, error)
	write func(string, []byte) error
}
type machineWriteUncertain struct { error }
type machinePending struct {
	id, sharedID string
	claim ResourceClaim
	preparation *PreparationRef
	attempt *MachineAttempt
}

func canonicalCapacityPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("machine capacity path is not absolute and canonical")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("machine capacity path contains an alias or unavailable component")
	}
	return nil
}

// MachineCapacityPolicyReadback is a copy of the native machine's fixed policy.
// It grants no lease or permission to change the original journal.
type MachineCapacityPolicyReadback struct {
	Source   string         `toml:"source"`
	Capacity CapacityPolicy `toml:"capacity"`
}

// ReadMachineCapacityPolicy never initializes a pool. A missing first pool has
// no policy; missing state after a lock exists requires original owner readback.
func ReadMachineCapacityPolicy(dir string) (*MachineCapacityPolicyReadback, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, errors.New("machine capacity directory is not canonical")
	}
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := canonicalCapacityPath(dir); err != nil {
		return nil, err
	}
	if err := capacityFileIdentity(dir, true); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "capacity.lock")
	if _, err := os.Lstat(lockPath); os.IsNotExist(err) {
		if _, stateErr := os.Lstat(filepath.Join(dir, "capacity.json")); !os.IsNotExist(stateErr) {
			return nil, errors.New("machine capacity lock missing; original owner readback required")
		}
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	identity, err := nativeMachineIdentity()
	if err != nil {
		return nil, err
	}
	m := &machineCapacity{dir: dir, identity: identity}
	var result *MachineCapacityPolicyReadback
	err = m.access(false, func(state *machineState) (bool, error) {
		if state.Capacity != nil {
			result = &MachineCapacityPolicyReadback{Source: filepath.Join(dir, "capacity.json"), Capacity: *state.Capacity}
		}
		return false, nil
	})
	return result, err
}

// BindMachineCapacity shares only resource leases across native Hosts. Existing
// quota/custody journals remain the authority for workers and original effects.
func (b *Budget) BindMachineCapacity(dir, owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.machineRequired = true
	fail := func(err error) error { b.machineReason = err.Error(); return err }
	if b.machine != nil || b.active != 0 || owner == "" {
		return fail(errors.New("machine capacity owner is already bound or unavailable"))
	}
	identity, err := nativeMachineIdentity()
	if err != nil { return fail(err) }
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir { return fail(errors.New("machine capacity directory is not canonical")) }
	if err = os.MkdirAll(dir, 0700); err != nil { return fail(err) }
	if err = canonicalCapacityPath(dir); err != nil { return fail(err) }
	if err = capacityFileIdentity(dir, true); err != nil { return fail(err) }
	m := &machineCapacity{dir: dir, identity: identity, owner: owner, probe: nativeDiskCapacity, write: filex.WriteFileAtomicDurable}
	if err = m.access(true, nil); err != nil { return fail(err) }
	b.machine = m
	b.machineReason = "trusted_worker_peak_missing"
	return nil
}

func (b *Budget) BindResourceSource(id, runtime string, source ResourceSource) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.resourceSources == nil { b.resourceSources = make(map[string]*resourceSource) }
	// A context's source is immutable even before its first worker.
	if _, exists := b.resourceSources[id]; !exists { b.resourceSources[id] = &resourceSource{runtime: runtime, read: source} }
}

// Swap recovery tracks each sample under the verified operator policy, even
// when no worker is waiting to start.
func (b *Budget) observeMachineHealth() {
	for _, source := range b.resourceSources {
		normals := source.normals
		source.normals = 0
		if source.read == nil || source.runtime == "" || b.stale() || b.unsupported || !b.rateValid { continue }
		claim, err := source.read(source.runtime)
		if err != nil || validateResourceClaim(claim) != nil {
			source.source = ""
			continue
		}
		if source.source != claim.Source || !sameAdmissionCapacity(source.policy, claim.Policy) { normals = 0 }
		source.source, source.policy = claim.Source, claim.Policy
		sample := b.previous
		if math.IsNaN(b.rate) || math.IsInf(b.rate, 0) || b.rate < 0 ||
			sample.SwapUsedBytes > claim.Policy.MaxSwapUsedBytes || b.rate > float64(claim.Policy.MaxSwapoutBytesPerSecond) { continue }
		source.normals = min(normals+1, MaxRecoverySamples)
	}
}

func (m *machineCapacity) access(initialize bool, apply func(*machineState) (bool, error)) error {
	if err := canonicalCapacityPath(m.dir); err != nil { return err }
	if err := capacityFileIdentity(m.dir, true); err != nil { return err }
	lockPath := filepath.Join(m.dir, "capacity.lock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	created := false
	if initialize {
		// Only the exclusive creator may initialize. The lock file is durable
		// evidence of an attempted creation, even if the first state write fails.
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err == nil {
			created = true
			syncErr := file.Sync()
			closeErr := file.Close()
			if syncErr != nil { return syncErr }
			if closeErr != nil { return closeErr }
			if err = filex.SyncDir(m.dir); err != nil { return err }
		} else if !os.IsExist(err) { return err }
	}
	if err := capacityFileIdentity(lockPath, false); err != nil { return err }
	path := filepath.Join(m.dir, "capacity.json")
	if initialize && !created {
		// A first caller can publish the lock before taking flock. Let that
		// caller finish without allowing a peer to recreate unknown state.
		for {
			if _, err := os.Lstat(path); err == nil { break
			} else if !os.IsNotExist(err) { return err }
			select {
			case <-ctx.Done(): return errors.New("machine capacity state missing; original owner readback required")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	lock, err := lockfile.Acquire(ctx, lockPath)
	if err != nil { return fmt.Errorf("machine capacity lock: %w", err) }
	defer lock.Close()
	if err = capacityFileIdentity(lockPath, false); err != nil { return err }
	raw, err := os.ReadFile(path)
	var s machineState
	changed := false
	if err == nil {
		if err = capacityFileIdentity(path, false); err != nil { return err }
		if err = uniqueCapacityJSON(json.NewDecoder(bytes.NewReader(raw))); err != nil { return err }
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&s); err != nil { return fmt.Errorf("machine capacity state corrupt: %w", err) }
		if err = decoder.Decode(new(any)); err != io.EOF { return errors.New("machine capacity state has trailing content") }
	} else if !os.IsNotExist(err) { return err
	} else if !created {
		return errors.New("machine capacity state missing; original owner readback required")
	} else {
		s = machineState{Schema: 1, Identity: m.identity, Policy: MachineAdmissionPolicy, Leases: map[string]machineLease{}}
		changed = true
	}
	if s.Schema != 1 || s.Identity != m.identity || s.Policy != MachineAdmissionPolicy || s.Leases == nil {
		return errors.New("machine capacity identity, schema or policy differs")
	}
	if len(s.Leases) != 0 && s.Capacity == nil { return errors.New("machine capacity policy missing for held leases") }
	if s.Capacity != nil {
		if err := validateCapacityPolicy(*s.Capacity); err != nil { return err }
	}
	for id, lease := range s.Leases {
		if err := validateMachinePreparation(lease); err != nil { return err }
		if err := validateMachineLeaseSettlement(lease); err != nil { return err }
		if id == "" || lease.Owner == "" || lease.Source == "" || lease.MemoryBytes == 0 || lease.CPUs == 0 || len(lease.Disks) == 0 {
			return errors.New("machine capacity lease corrupt")
		}
		for volume, size := range lease.Disks {
			if volume == "" || size == 0 { return errors.New("machine capacity disk lease corrupt") }
		}
	}
	if apply != nil {
		update, e := apply(&s)
		if e != nil { return e }
		changed = changed || update
	}
	if !changed { return nil }
	data, err := json.Marshal(s)
	if err != nil { return err }
	if err = m.write(path, data); err != nil { return &machineWriteUncertain{err} }
	return nil
}

func uniqueCapacityJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil { return fmt.Errorf("machine capacity JSON corrupt: %w", err) }
	delimiter, nested := token.(json.Delim)
	if !nested { return nil }
	keys := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			token, err = decoder.Token()
			if err != nil { return err }
			key, ok := token.(string)
			if !ok || keys[key] { return errors.New("machine capacity JSON contains duplicate or invalid keys") }
			keys[key] = true
		}
		if err = uniqueCapacityJSON(decoder); err != nil { return err }
	}
	_, err = decoder.Token()
	return err
}

func validateResourceClaim(c ResourceClaim) error {
	if c.Source == "" || c.MemoryBytes == 0 || c.CPUs == 0 || c.DiskBytes == 0 || len(c.DiskPaths) == 0 {
		return errors.New("trusted_worker_peak_missing")
	}
	return validateCapacityPolicy(c.Policy)
}

// Frozen claims retain their original counts. Live compatibility compares the enforced policy.
func sameAdmissionCapacity(a, b CapacityPolicy) bool {
	a.MemoryBytes, b.MemoryBytes = 0, 0
	a.CPUs, b.CPUs = 0, 0
	return a == b
}

func validateCapacityPolicy(p CapacityPolicy) error {
	if p.DiskSafetyBytes == 0 || p.RecoverySamples < 2 || p.RecoverySamples > MaxRecoverySamples {
		return errors.New("operator_machine_policy_missing_or_invalid")
	}
	return nil
}

func (m *machineCapacity) acquire(id, contextID, orderID, runtime string, c ResourceClaim, sample Sample, rate float64, rateValid bool, normals int, now time.Time) error {
	if err := validateResourceClaim(c); err != nil { return err }
	return m.access(false, func(s *machineState) (bool, error) {
		return m.reserve(s, id, contextID, orderID, runtime, c, sample, rate, rateValid, normals, now)
	})
}

// The caller owns the same machine journal lock for lookup, nonce creation,
// volume observation and reservation. Preparation cannot race worker claims.
func (m *machineCapacity) reserve(s *machineState, id, contextID, orderID, runtime string, c ResourceClaim, sample Sample, rate float64, rateValid bool, normals int, now time.Time) (bool, error) {
		lease := machineLease{ContextID: contextID, OrderID: orderID, Runtime: runtime, Owner: m.owner, Source: c.Source, MemoryBytes: c.MemoryBytes, CPUs: c.CPUs, Disks: map[string]uint64{}}
		available := map[string]uint64{}
		for _, path := range c.DiskPaths {
			disk, err := m.probe(path)
			if err != nil { return false, fmt.Errorf("disk_observation_unavailable: %w", err) }
			if disk.Volume == "" { return false, errors.New("disk_volume_identity_unknown") }
			lease.Disks[disk.Volume] = c.DiskBytes
			if old, ok := available[disk.Volume]; !ok || disk.Available < old { available[disk.Volume] = disk.Available }
		}
		if s.Capacity != nil && !sameAdmissionCapacity(*s.Capacity, c.Policy) {
			for _, held := range s.Leases {
				if held.Settlement == nil && held.PreparationClosed == nil { return false, errors.New("machine_operator_policy_differs_with_held_leases") }
			}
		}
		switch {
		case sample.At.IsZero(): return false, errors.New("machine_sample_missing")
		case now.Before(sample.At) || now.Sub(sample.At) > MaxSampleAge: return false, errors.New("machine_sample_stale")
		case sample.Unsupported: return false, errors.New("machine_sample_unsupported")
		case !rateValid || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0: return false, errors.New("swap_rate_unavailable")
		case sample.SwapUsedBytes > c.Policy.MaxSwapUsedBytes: return false, errors.New("swap_used_pressure")
		case rate > float64(c.Policy.MaxSwapoutBytesPerSecond): return false, errors.New("swapout_pressure")
		case normals < c.Policy.RecoverySamples: return false, errors.New("machine_pressure_recovering")
		case c.Policy.RequireIOObservation: return false, errors.New("io_observation_unsupported")
		}
		if original, exists := s.Leases[id]; exists {
			if !reflect.DeepEqual(original, lease) { return false, errors.New("original_machine_lease_binding_differs_or_effects_possible") }
			return false, nil
		}
		disks := map[string]uint64{}
		for volume, free := range available {
			if free < c.Policy.DiskSafetyBytes { return false, errors.New("disk_safety_margin") }
			disks[volume] = free - c.Policy.DiskSafetyBytes
		}
		consume := func(l machineLease) error {
			peak := uint64(0)
			for _, size := range l.Disks { peak = max(peak, size) }
			// APFS volumes may share free storage. Charge every held peak against
			// every probed volume rather than claiming their free bytes are disjoint.
			for volume, remaining := range disks {
				if peak > remaining { return errors.New("disk_reservation_capacity_insufficient") }
				disks[volume] -= peak
			}
			return nil
		}
		for _, held := range s.Leases {
			if held.Settlement != nil || held.PreparationClosed != nil { continue }
			if err := consume(held); err != nil { return false, err }
		}
		if err := consume(lease); err != nil { return false, err }
		policy := c.Policy
		if s.Capacity != nil && sameAdmissionCapacity(*s.Capacity, policy) {
			policy = *s.Capacity
		}
		s.Capacity = &policy
		s.Leases[id] = lease
		return true, nil
}

func (m *machineCapacity) start(id string) error {
	return m.access(false, func(s *machineState) (bool, error) {
		lease, exists := s.Leases[id]
		if !exists || lease.Owner != m.owner { return false, errors.New("original machine lease missing or foreign") }
		if lease.EffectsPossible { return false, nil }
		lease.EffectsPossible = true
		s.Leases[id] = lease
		return true, nil
	})
}

func (m *machineCapacity) cancelUnstarted(id string) error {
	return m.access(false, func(s *machineState) (bool, error) {
		lease, exists := s.Leases[id]
		if !exists { return false, nil }
		if lease.Owner != m.owner || lease.EffectsPossible { return false, errors.New("machine resources held for original VM retirement and GC evidence") }
		delete(s.Leases, id)
		return true, nil
	})
}

func machineNonce() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil { return "", err }
	return hex.EncodeToString(nonce[:]), nil
}

// Caller holds the original Budget mutex. The quota and machine journals have
// separate locks; an uncertain machine acquire retains its original quota nonce.
func (b *Budget) acquireMachineTask(contextID, order, runtime string, attempt *MachineAttempt) (*Permit, error) {
	deferReason := func(reason string) (*Permit, error) {
		b.machineReason = reason
		return nil, fmt.Errorf("%w: %s", ErrDeferred, reason)
	}
	if b.machineFault != "" { return deferReason(b.machineFault) }
	if attempt != nil && (attempt.StageIndex < 0 || attempt.RetryCount < 0 || order == "") {
		return deferReason("original_machine_attempt_identity_missing")
	}
	if len(b.pendingMachineReturns) != 0 { return deferReason("machine_unstarted_return_readback_required") }
	if b.machine == nil { return deferReason("machine_owner_unavailable: " + b.machineReason) }
	source := b.resourceSources[contextID]
	if source == nil || source.read == nil { return deferReason("trusted_worker_peak_missing") }
	claim, err := source.read(runtime)
	if err == nil { err = validateResourceClaim(claim) }
	if err != nil {
		if runtime == source.runtime { source.source, source.normals = "", 0 }
		return deferReason(err.Error())
	}
	if source.source != claim.Source || !sameAdmissionCapacity(source.policy, claim.Policy) { source.normals = 0 }
	key := contextID + "\x00" + order
	if b.machineAllocations[key] != nil { return deferReason("original_machine_allocation_active") }
	pending, exists := b.pendingMachineAcquires[key]
	if !exists && len(b.pendingMachineAcquires) != 0 { return deferReason("original_machine_acquire_readback_required") }
	if exists && (!reflect.DeepEqual(pending.claim, claim) || !reflect.DeepEqual(pending.attempt, attempt)) {
		return deferReason("original_machine_acquire_source_changed")
	}
	if !exists {
		pending.claim = claim
		pending.attempt = attempt
		if prepared, ok := b.preparedMachine[key]; ok {
			pending.id, pending.preparation = prepared.ID, &prepared
		} else {
			pending.id, err = machineNonce()
			if err != nil { return deferReason(err.Error()) }
		}
		if b.shared != nil {
			pending.sharedID, err = b.shared.acquire(contextID, b.maximum, false, "", b.switching)
			if err != nil {
				var uncertain *quotaWriteUncertain
				if errors.As(err, &uncertain) { b.rememberSharedReturn(pending.sharedID) }
				b.sharedError = err.Error()
				return deferReason("host_quota_unavailable")
			}
			b.sharedError = ""
		}
	}
	sample := Sample{}
	if b.previous != nil { sample = *b.previous }
	normals := 0
	if source.source == claim.Source && sameAdmissionCapacity(source.policy, claim.Policy) { normals = source.normals }
	if pending.preparation == nil {
		err = b.machine.acquire(pending.id, contextID, order, runtime, claim, sample, b.rate, b.rateValid, normals, b.now())
	} else {
		if pending.id == pending.preparation.ID {
			err = b.machine.promotePreparation(*pending.preparation, contextID, order, runtime, claim, sample, b.rate, b.rateValid, normals, b.now(), exists)
			if !exists && attempt != nil && errors.Is(err, errPreparationDispatched) {
				pending.id, err = machineNonce()
				if err == nil && pending.id == pending.preparation.ID {
					err = errors.New("successor acquired the original preparation nonce")
				}
			}
		}
		if err == nil && pending.id != pending.preparation.ID {
			err = b.machine.acquirePreparedSuccessor(*pending.preparation, pending.id, contextID, order, runtime, *pending.attempt, claim, sample, b.rate, b.rateValid, normals, b.now())
		}
	}
	if err != nil {
		var uncertain *machineWriteUncertain
		if exists || errors.As(err, &uncertain) {
			if b.pendingMachineAcquires == nil { b.pendingMachineAcquires = make(map[string]machinePending) }
			b.pendingMachineAcquires[key] = pending
		} else if b.shared != nil { b.returnSharedAllocation(pending.sharedID) }
		return deferReason(err.Error())
	}
	delete(b.pendingMachineAcquires, key)
	if b.machineAllocations == nil { b.machineAllocations = make(map[string]*allocation) }
	a := &allocation{budget: b, refs: 1, sharedID: pending.sharedID, machineID: pending.id, machineKey: key,
		machineAttempt: pending.attempt, effectsPossible: pending.preparation != nil && pending.preparation.ID == pending.id}
	b.machineAllocations[key] = a
	b.active++
	b.machineReason = "admitted"
	return &Permit{allocation: a}, nil
}

func capacityIdentity(machine string, uid int) (string, error) {
	if machine == "" || uid < 0 { return "", errors.New("physical machine or user identity unavailable") }
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/uid:%d", machine, uid)))
	return hex.EncodeToString(sum[:]), nil
}

const platformUUIDOutputLimit = 64 * 1024

func parseDarwinPlatformUUID(data []byte) (string, error) {
	if len(data) > platformUUIDOutputLimit { return "", errors.New("IOPlatformUUID observation exceeds output limit") }
	value := ""
	for _, line := range strings.Split(string(data), "\n") {
		key, raw, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok || key != `"IOPlatformUUID"` { continue }
		if value != "" { return "", errors.New("IOPlatformUUID observation is ambiguous") }
		var err error
		value, err = strconv.Unquote(raw)
		if err != nil || len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
			return "", errors.New("IOPlatformUUID observation is invalid")
		}
		id, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
		if err != nil || len(id) != 16 || bytes.Equal(id, make([]byte, 16)) { return "", errors.New("IOPlatformUUID observation is invalid") }
	}
	if value == "" { return "", errors.New("IOPlatformUUID observation is missing") }
	return strings.ToLower(value), nil
}

func capacityProbePath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path { return "", errors.New("disk probe path is not canonical") }
	for {
		_, err := os.Lstat(path)
		if err == nil { return path, canonicalCapacityPath(path) }
		if !os.IsNotExist(err) { return "", err }
		parent := filepath.Dir(path)
		if parent == path { return "", errors.New("disk probe ancestor unavailable") }
		path = parent
	}
}

// StartResourceEffects is persisted before the launch callback can create a
// checkout. A lost write result holds resources; worker Done only returns quota.
func (p *Permit) StartResourceEffects() error {
	if p == nil { return nil }
	b := p.allocation.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.released { return fmt.Errorf("%w: original permit released", ErrDeferred) }
	if b.machineFault != "" { return fmt.Errorf("%w: %s", ErrDeferred, b.machineFault) }
	if p.allocation.machineID == "" { return nil }
	// Reentry of this launch fence is idempotent. ReserveReplacement refuses a
	// new sandbox while the original physical resources still lack retirement.
	if p.allocation.effectsPossible { return nil }
	p.allocation.effectsPossible = true
	if err := b.machine.start(p.allocation.machineID); err != nil {
		b.machineFault = "machine_effect_fence_readback_required: " + err.Error()
		b.machineReason = b.machineFault
		return fmt.Errorf("%w: %s", ErrDeferred, b.machineFault)
	}
	return nil
}

func (b *Budget) returnUnstartedMachine(id string) {
	if err := b.machine.cancelUnstarted(id); err != nil {
		if b.pendingMachineReturns == nil { b.pendingMachineReturns = make(map[string]bool) }
		b.pendingMachineReturns[id] = true
		b.machineReason = "machine_unstarted_return_readback_required: " + err.Error()
		return
	}
	delete(b.pendingMachineReturns, id)
}

func (b *Budget) machineSnapshot() *MachineSnapshot {
	if !b.machineRequired { return nil }
	s := &MachineSnapshot{Scope: "native_uid_participating_reservations_and_machine_telemetry", Uncovered: "legacy_and_nonparticipating_processes_not_assumed_reserved", Policy: MachineAdmissionPolicy, Reason: b.machineReason, RAM: "unknown", CPU: "observation_only", CPUCountRole: "configuration_and_legacy_lease_counts_not_busy_percent", Swap: "used_and_swapout_rate", Disk: "statfs_at_admission_only_all_machine_peaks_deducted", IO: "unknown_no_reliable_observer", Retention: "effects_possible_held_for_vm_retirement_and_gc", Next: "original_resource_owner_return_or_pressure_recovery"}
	if b.stale() || b.unsupported { s.CPU = "unknown"; s.Swap = "unknown" }
	if !b.stale() && !b.unsupported && b.previous != nil {
		memory, cpus := b.previous.TotalMemoryBytes, b.previous.CPUs
		if memory != 0 { s.ObservedMemoryBytes = &memory; s.RAM = "observation_only" }
		s.RAMObservationError = b.previous.MemoryObservationError
		if cpus > 0 { s.ObservedCPUs = &cpus }
	}
	if len(b.pendingMachineAcquires) != 0 || len(b.pendingMachineReturns) != 0 || b.machineFault != "" { s.Next = "original_machine_nonce_readback" }
	if b.machine == nil { s.Disk = "unknown"; return s }
	s.ReadbackSource = filepath.Join(b.machine.dir, "capacity.json")
	err := b.machine.access(false, func(state *machineState) (bool, error) {
		s.Leases = 0
		if state.Capacity != nil { capacity := *state.Capacity; s.Capacity = &capacity }
		for id, lease := range state.Leases {
			if lease.Settlement != nil || lease.PreparationClosed != nil { continue }
			s.Leases++
			cpus := b.machine.preparationCPUs(id, lease)
			if lease.MemoryBytes > math.MaxUint64-s.MemoryBytes || cpus > math.MaxUint64-s.CPUs { return false, errors.New("machine capacity totals overflow") }
			s.MemoryBytes += lease.MemoryBytes; s.CPUs += cpus
			peak := uint64(0)
			for _, size := range lease.Disks { peak = max(peak, size) }
			if peak > math.MaxUint64-s.DiskBytes { return false, errors.New("machine disk capacity totals overflow") }
			s.DiskBytes += peak
		}
		return false, nil
	})
	if err != nil { s.Reason = err.Error(); s.Disk = "unknown" }
	return s
}
