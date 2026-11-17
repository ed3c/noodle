package admission

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
)

// This is an observation of the original Loop's waiting lifetime, not a second
// reservation journal. The machine row stays byte-compatible with old Hosts.
type preparationWait struct {
	Schema    int                              `json:"schema"`
	Identity  string                           `json:"machine_identity"`
	Ref       PreparationRef                   `json:"preparation"`
	Owner     string                           `json:"owner"`
	ContextID string                           `json:"context_id"`
	OrderID   string                           `json:"order_id"`
	Runtime   string                           `json:"runtime"`
	Source    string                           `json:"source"`
	Policy    CapacityPolicy                   `json:"policy"`
	Process   procx.NativeIdentity             `json:"process"`
	ReadyAt   time.Time                        `json:"ready_at"`
	Sources   map[string]MachineSettlementFile `json:"sources"`
}

func (m *machineCapacity) preparationWaitPaths(id string) (string, string) {
	digest := sha256.Sum256([]byte(id))
	path := filepath.Join(m.dir, "preparation-wait-"+hex.EncodeToString(digest[:]))
	return path + ".json", path + ".lock"
}

// BeginPreparationWait is called by the original Loop actor only after its
// entire startup Cycle and watcher setup have finished with no worker attempt.
// The returned lock lives until that Loop exits. Failed/abandoned startup can
// never leave a static receipt that discounts CPU after its actor has gone.
func (b *Budget) BeginPreparationWait(ref PreparationRef, contextID, orderID string, sources map[string]MachineSettlementFile) (io.Closer, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil {
		return nil, errors.New("original preparation wait owner unavailable")
	}
	if b.machineFault != "" || len(b.pendingMachineAcquires) != 0 || len(b.pendingMachineReturns) != 0 || len(b.pendingSharedReturns) != 0 {
		return nil, errors.New("original preparation wait has unresolved resource mutations")
	}
	var lifetime *lockfile.Lock
	err := b.machine.access(false, func(state *machineState) (bool, error) {
		lease, ok := state.Leases[ref.ID]
		if !ok || lease.Owner != b.machine.owner || lease.ContextID != contextID || lease.OrderID != orderID ||
			!preparationCanWait(lease) || lease.Preparation.SHA256 != ref.SHA256 {
			return false, errors.New("original preparation wait binding differs or has worker effects")
		}
		if err := verifyPreparationWaitSources(lease, sources); err != nil {
			return false, err
		}
		process, err := procx.ObserveNativeIdentity(os.Getpid())
		if err != nil || !process.SameGeneration(process) || process.Zombie {
			return false, errors.New("original preparation wait process identity unavailable")
		}
		path, lockPath := b.machine.preparationWaitPaths(ref.ID)
		for _, file := range []string{path, lockPath} {
			if _, err := os.Lstat(file); err == nil {
				if err = capacityFileIdentity(file, false); err != nil {
					return false, err
				}
			} else if !os.IsNotExist(err) {
				return false, err
			}
		}
		lifetime, err = lockfile.TryLock(lockPath)
		if err != nil {
			return false, fmt.Errorf("original preparation wait actor already bound: %w", err)
		}
		proof := preparationWait{Schema: 1, Identity: b.machine.identity, Ref: ref, Owner: lease.Owner, ContextID: contextID,
			OrderID: orderID, Runtime: lease.Runtime, Source: lease.Source, Policy: lease.Preparation.Claim.Policy,
			Process: process, ReadyAt: time.Now(), Sources: sources}
		if previous, err := os.ReadFile(path); err == nil {
			var saved preparationWait
			decoder := json.NewDecoder(bytes.NewReader(previous))
			decoder.DisallowUnknownFields()
			if uniqueCapacityJSON(json.NewDecoder(bytes.NewReader(previous))) != nil || decoder.Decode(&saved) != nil ||
				decoder.Decode(new(any)) != io.EOF || saved.ReadyAt.IsZero() || saved.ReadyAt.After(time.Now()) {
				return false, errors.New("original preparation wait receipt requires readback")
			}
			proof.ReadyAt = saved.ReadyAt
			if !reflect.DeepEqual(saved, proof) {
				return false, errors.New("original preparation wait receipt differs")
			}
			return false, bindPreparationWaitLock(lockPath, process.PID, previous) // Lost ACK: same actor/source/nonce.
		} else if !os.IsNotExist(err) {
			return false, err
		}
		data, err := json.Marshal(proof)
		if err != nil {
			return false, err
		}
		// No machine row is mutated: old readers keep the original full CPU.
		if err = b.machine.write(path, data); err != nil {
			return false, err
		}
		return false, bindPreparationWaitLock(lockPath, process.PID, data)
	})
	if err != nil {
		if lifetime != nil {
			_ = lifetime.Close()
		}
		return nil, err
	}
	return lifetime, nil
}

func preparationCanWait(lease machineLease) bool {
	return lease.Preparation != nil && lease.Preparation.Source.Purpose == "task-preparation" &&
		!lease.PreparationPromoted && lease.Binding == nil && lease.Settlement == nil && lease.PreparationClosed == nil &&
		lease.EffectsPossible && lease.ContextID != "" && lease.OrderID != "" && lease.Runtime == "openshell"
}

func verifyPreparationWaitSources(lease machineLease, files map[string]MachineSettlementFile) error {
	bindings := lease.Preparation.Source.Bindings
	for _, role := range []string{"task", "goal"} {
		ref := files[role]
		if ref.Path == "" || ref.Path != bindings[role+"_path"] || ref.SHA256 != bindings[role+"_sha256"] {
			return errors.New("original preparation wait task or Goal source differs")
		}
	}
	if ref := files["execution_binary"]; ref.Path == "" || ref.Path+"/"+ref.SHA256 != bindings["execution_binary"] {
		return errors.New("original preparation wait execution binary differs")
	}
	if files["profile"].Path != filepath.Join(bindings["host_root"], "openshell", "profile.json") ||
		files["control"].Path != filepath.Join(filepath.Dir(bindings["control_root"]), "control.json") {
		return errors.New("original preparation wait profile or control source differs")
	}
	return verifyMachineSettlementFiles(files)
}

// Called only while holding the existing machine lock. Any missing, foreign,
// stale or unsupported observation retains the original CPU; never skip a row.
func (m *machineCapacity) preparationCPUs(id string, lease machineLease) uint64 {
	if !preparationCanWait(lease) {
		return lease.CPUs
	}
	path, lockPath := m.preparationWaitPaths(id)
	if canonicalCapacityPath(path) != nil || capacityFileIdentity(path, false) != nil ||
		canonicalCapacityPath(lockPath) != nil || capacityFileIdentity(lockPath, false) != nil {
		return lease.CPUs
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 1<<20 {
		return lease.CPUs
	}
	raw, err := os.ReadFile(path)
	if err != nil || uniqueCapacityJSON(json.NewDecoder(bytes.NewReader(raw))) != nil {
		return lease.CPUs
	}
	var proof preparationWait
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&proof) != nil || decoder.Decode(new(any)) != io.EOF || proof.Schema != 1 || proof.ReadyAt.IsZero() ||
		proof.ReadyAt.After(time.Now()) || proof.Identity != m.identity || proof.Ref != (PreparationRef{id, lease.Preparation.SHA256}) ||
		proof.Owner != lease.Owner || proof.ContextID != lease.ContextID || proof.OrderID != lease.OrderID ||
		proof.Runtime != lease.Runtime || proof.Source != lease.Source || proof.Policy != lease.Preparation.Claim.Policy {
		return lease.CPUs
	}
	process, err := procx.ObserveNativeIdentity(proof.Process.PID)
	if err != nil || process.Zombie || !proof.Process.SameGeneration(process) {
		return lease.CPUs
	}
	if !preparationWaitLockHeld(lockPath, process.PID, raw) || verifyPreparationWaitSources(lease, proof.Sources) != nil {
		return lease.CPUs
	}
	// Recheck after potentially slow source readback; a released lock, replaced
	// receipt or exited generation must not survive as a zero-CPU observation.
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) || !preparationWaitLockHeld(lockPath, process.PID, raw) {
		return lease.CPUs
	}
	processAfter, err := procx.ObserveNativeIdentity(process.PID)
	if err != nil || processAfter.Zombie || !process.SameGeneration(processAfter) {
		return lease.CPUs
	}
	return 0
}

func preparationWaitWitness(pid int, proof []byte) []byte {
	digest := sha256.Sum256(proof)
	return []byte(fmt.Sprintf("%d\n%s\n", pid, hex.EncodeToString(digest[:])))
}

// Keep the kernel-locked inode. Replacing it would disconnect the observation
// from the original actor. The witness binds the entire source closure, so a
// partial/tampered sidecar cannot retain a live discount.
func bindPreparationWaitLock(path string, pid int, proof []byte) error {
	if err := capacityFileIdentity(path, false); err != nil {
		return err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, current) {
		_ = file.Close()
		return errors.New("original preparation wait lock inode changed before publication")
	}
	if err = file.Truncate(0); err != nil {
		_ = file.Close()
		return err
	}
	_, writeErr := file.Write(preparationWaitWitness(pid, proof))
	syncErr := file.Sync()
	after, statErr := file.Stat()
	current, pathErr = os.Lstat(path)
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, statErr, pathErr, closeErr); err != nil {
		return err
	}
	if !os.SameFile(before, after) || !os.SameFile(before, current) {
		return errors.New("original preparation wait lock inode changed")
	}
	return capacityFileIdentity(path, false)
}
