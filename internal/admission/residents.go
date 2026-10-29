package admission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

// ResidentSpec is an explicit service contract. Stdio sessions cannot be shared.
type ResidentSpec struct {
	Name        string   `json:"name"`
	Argv        []string `json:"argv"`
	Directory   string   `json:"directory"`
	Environment []string `json:"environment"`
	Sharing     string   `json:"sharing"`
	Transport   string   `json:"transport"`
	// The service owner declares that no state must survive after its last lease.
	ForceAfterGrace bool `json:"force_after_grace,omitempty"`
	// TestFixture opts a verified stateless test helper into recovery journaling.
	TestFixture bool `json:"test_fixture,omitempty"`
}
type Resident struct {
	ID                  string               `json:"id"`
	Name                string               `json:"name"`
	Fingerprint         string               `json:"fingerprint"`
	Sharing             string               `json:"sharing"`
	PID                 int                  `json:"pid"`
	ProcessGroup        int                  `json:"process_group"`
	Started             string               `json:"started"`
	State               string               `json:"state"`
	Contexts            []string             `json:"contexts"`
	Released            []string             `json:"released_contexts"`
	Error               string               `json:"error,omitempty"`
	ForceAfterGrace     bool                 `json:"force_after_grace,omitempty"`
	GuardID             string               `json:"guard_id"`
	StopRequested       string               `json:"stop_requested,omitempty"`
	ForceRequested      string               `json:"force_requested,omitempty"`
	History             []Resident           `json:"history,omitempty"`
	Errors              []string             `json:"errors,omitempty"`
	ConsumerLeases      []ResidentConsumer   `json:"consumer_leases,omitempty"`
	TestFixture         bool                 `json:"test_fixture,omitempty"`
	ObservedDescendants []ResidentDescendant `json:"observed_descendants,omitempty"`
}
type residentChild struct {
	mu       sync.Mutex
	reaped   bool
	process  *os.Process
	permit   *Permit
	done     chan struct{}
	guard    *ResidentGuard
	checking bool
}

// Residents belongs to the existing Host. Only its own child handles can be stopped.
type Residents struct {
	mu       sync.Mutex
	path     string
	records  map[string]*Resident
	children map[string]*residentChild
	failure  error
	budget   *Budget
	released chan struct{}
	fixtures *FixtureRecovery
}

// Released wakes the existing Host actor after an observed allocation release.
func (r *Residents) Released() <-chan struct{} { return r.released }

func (r *Residents) notifyReleased() {
	select {
	case r.released <- struct{}{}:
	default:
	}
}

func (r *Residents) BindBudget(b *Budget) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budget = b
	r.updateRecoveryBudget()
}

// Called with r.mu held. Unknown records have no transferable child handles.
func (r *Residents) updateRecoveryBudget() {
	if r.budget == nil {
		return
	}
	count := 0
	for id, v := range r.records {
		if v.State != "exited" && r.children[id] == nil {
			count++
		}
	}
	r.budget.SetUnresolvedResidents(count)
}

func OpenResidents(directory string) (*Residents, error) {
	r := &Residents{path: filepath.Join(directory, "residents.json"), records: map[string]*Resident{}, children: map[string]*residentChild{}, released: make(chan struct{}, 1)}
	fixtures, err := OpenFixtureRecovery(directory)
	if err != nil {
		return nil, err
	}
	r.fixtures = fixtures
	data, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &r.records); err != nil {
		return nil, fmt.Errorf("read resident ownership: %w", err)
	}
	for _, v := range r.records {
		if v == nil {
			return nil, errors.New("resident record is null")
		}
		if v.State != "exited" {
			if v.Error != "" && (len(v.Errors) == 0 || v.Errors[len(v.Errors)-1] != v.Error) {
				v.Errors = append(v.Errors, v.Error)
			}
			v.State = "needs_owner_readback"
			v.Error = "prior Host child handle unavailable; do not adopt or signal by PID"
		}
	}
	return r, nil
}
func (r *Residents) save() error {
	data, err := json.MarshalIndent(r.records, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(r.path), "residents-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), r.path)
}
func (r *Residents) persist() error {
	for _, v := range r.records {
		if v.Error != "" && (len(v.Errors) == 0 || v.Errors[len(v.Errors)-1] != v.Error) {
			v.Errors = append(v.Errors, v.Error)
		}
	}
	if err := r.save(); err != nil {
		r.failure = err
		return err
	}
	return nil
}
func cloneResident(v *Resident) Resident {
	out := *v
	out.ConsumerLeases = append([]ResidentConsumer{}, v.ConsumerLeases...)
	out.Contexts = append([]string{}, v.Contexts...)
	out.Released = append([]string{}, v.Released...)
	out.Errors = append([]string{}, v.Errors...)
	out.ObservedDescendants = append([]ResidentDescendant{}, v.ObservedDescendants...)
	out.History = make([]Resident, len(v.History))
	for i := range v.History {
		out.History[i] = cloneResident(&v.History[i])
	}
	slices.Sort(out.Contexts)
	return out
}
func (r *Residents) Snapshot() ([]Resident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Resident, 0, len(r.records))
	for _, v := range r.records {
		out = append(out, cloneResident(v))
	}
	slices.SortFunc(out, func(a, b Resident) int { return strings.Compare(a.ID, b.ID) })
	return out, r.failure
}

var residentName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)

func residentIdentity(spec ResidentSpec, contextID string) (string, string, error) {
	if spec.TestFixture && !fixtureHelper(spec.Argv) {
		return "", "", errors.New("test fixture must use an explicit supported stateless helper")
	}
	if !residentName.MatchString(spec.Name) || len(spec.Argv) == 0 || !filepath.IsAbs(spec.Argv[0]) || !filepath.IsAbs(spec.Directory) {
		return "", "", errors.New("resident name, absolute executable or directory missing")
	}
	if spec.Transport != "network" || (spec.Sharing != "shared" && spec.Sharing != "context") {
		return "", "", errors.New("resident transport/sharing unsupported; stdio requires its original Session owner")
	}
	for _, arg := range spec.Argv {
		if strings.ContainsRune(arg, 0) {
			return "", "", errors.New("resident argument contains NUL")
		}
	}
	dir, err := filepath.EvalSymlinks(spec.Directory)
	if err != nil {
		return "", "", err
	}
	spec.Directory = dir
	spec.Argv = append([]string{}, spec.Argv...)
	spec.Argv[0], err = filepath.EvalSymlinks(spec.Argv[0])
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(spec.Argv[0])
	if err != nil {
		return "", "", err
	}
	exe := sha256.Sum256(data)
	spec.Environment = append([]string{}, spec.Environment...)
	slices.Sort(spec.Environment)
	seen := map[string]bool{}
	for _, entry := range spec.Environment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || seen[key] || strings.ContainsRune(entry, 0) {
			return "", "", errors.New("resident environment invalid or duplicate")
		}
		seen[key] = true
	}
	encoded, err := json.Marshal(struct {
		Spec       ResidentSpec
		Executable string
	}{spec, hex.EncodeToString(exe[:])})
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(encoded)
	key := spec.Name
	if spec.Sharing == "context" {
		key = contextID + "/" + key
	}
	return key, hex.EncodeToString(sum[:]), nil
}
func (r *Residents) Acquire(spec ResidentSpec, contextID string, budget *Budget) (Resident, error) {
	return r.acquire(spec, contextID, contextID, budget, nil, "", nil)
}
func (r *Residents) acquire(spec ResidentSpec, contextID, permitContext string, budget *Budget, consumer *ResidentConsumer, group string, guard *ResidentGuard) (Resident, error) {
	// These exact helper entry points implement the test-only stateless contract.
	// Other residents require an explicit opt-in and may not borrow this policy.
	if fixtureHelper(spec.Argv) {
		spec.TestFixture = true
	}
	if spec.Sharing == "shared" && !spec.TestFixture && guard == nil {
		return Resident{}, errors.New("shared service has no verified client isolation/drain adapter; retain private instances")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return Resident{}, fmt.Errorf("resident persistence failed; readback required: %w", r.failure)
	}
	key, fingerprint, err := residentIdentity(spec, contextID)
	if consumer != nil {
		key = group
	}
	if err != nil {
		return Resident{}, err
	}
	if spec.TestFixture {
		if _, err = fixtureDirectoryPurpose(spec.Directory); err != nil {
			return Resident{}, err
		}
	}
	guardID := ""
	if guard != nil {
		guardID = guard.id
		if spec.ForceAfterGrace {
			return Resident{}, errors.New("guarded resident force_after_grace unsupported")
		}
		fingerprint = guardedFingerprint(fingerprint, guardID)
	}
	for _, old := range r.records {
		if old.Name == spec.Name && old.Sharing != spec.Sharing && (old.GuardID != "" || guardID != "") {
			return cloneResident(old), errors.New("guarded resident sharing conflicts with original service identity")
		}
	}
	if old := r.records[key]; old != nil {
		if old.GuardID != guardID {
			return cloneResident(old), errors.New("resident release guard identity conflicts with original owner")
		}
		if child := r.children[key]; child != nil {
			if child.checking {
				return cloneResident(old), errors.New("resident release guard check in progress")
			}
			if child.guard != guard {
				return cloneResident(old), errors.New("resident release guard callback belongs to original owner")
			}
		}
	}
	if old := r.records[key]; old != nil && slices.Contains(old.Released, contextID) {
		return cloneResident(old), errors.New("resident lease already released; use original context lifecycle")
	}
	if old := r.records[key]; old != nil && old.State != "exited" {
		if old.State != "running" {
			return cloneResident(old), fmt.Errorf("resident %s requires readback in state %s", key, old.State)
		}
		if old.Fingerprint != fingerprint {
			return cloneResident(old), errors.New("resident service configuration conflicts with existing owner")
		}
		if !slices.Contains(old.Contexts, contextID) {
			old.Contexts = append(old.Contexts, contextID)
			if consumer != nil {
				old.ConsumerLeases = append(old.ConsumerLeases, *consumer)
			}
			if err = r.persist(); err != nil {
				return cloneResident(old), err
			}
		}
		return cloneResident(old), nil
	}
	if budget == nil {
		return Resident{}, errors.New("resident resource admission unavailable")
	}
	permit, err := budget.tryAcquire(permitContext, true, key, "")
	if err != nil {
		return Resident{}, err
	}
	v := &Resident{ID: key, Name: spec.Name, Fingerprint: fingerprint, Sharing: spec.Sharing, State: "starting", Contexts: []string{contextID}, ForceAfterGrace: spec.ForceAfterGrace, TestFixture: spec.TestFixture, GuardID: guardID}
	if consumer != nil {
		v.ConsumerLeases = []ResidentConsumer{*consumer}
	}
	if old := r.records[key]; old != nil {
		v.Released = append([]string{}, old.Released...)
		previous := cloneResident(old)
		previous.History = nil
		v.History = append(append([]Resident{}, old.History...), previous)
	}
	r.records[key] = v
	if err = r.persist(); err != nil {
		permit.Release()
		return cloneResident(v), err
	}
	// Each resident uses one log, not a per-context pipe or persistent proxy.
	sum := sha256.Sum256([]byte(key))
	logPath := filepath.Join(filepath.Dir(r.path), "resident-"+hex.EncodeToString(sum[:8])+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	var process *os.Process
	if err == nil {
		process, err = startResident(spec, log)
		log.Close()
	}
	if err != nil {
		v.State = "exited"
		v.Error = err.Error()
		v.Released = append(v.Released, v.Contexts...)
		v.Contexts = []string{}
		permit.Release()
		saveErr := r.persist()
		return cloneResident(v), errors.Join(err, saveErr)
	}
	v.PID = process.Pid
	v.ProcessGroup = process.Pid
	v.Error = "process start identity not observed; restart reconciliation requires owner readback"
	if rows, readErr := ObserveResidents(context.Background()); readErr == nil {
		for _, row := range rows {
			if row.PID == v.PID {
				v.Started = row.Started
				v.Error = ""
				break
			}
		}
		rememberResidentDescendants(v, rows)
	} else {
		v.Error = "process start identity unavailable: " + readErr.Error()
	}
	v.State = "running"
	child := &residentChild{process: process, permit: permit, done: make(chan struct{}), guard: guard}
	r.children[key] = child
	saveErr := r.persist()
	go func() {
		defer close(child.done)
		err := waitResident(child)
		// Wait reaps only the launcher. Observe the owned group before returning quota.
		absent, observeErr := procx.ProcessGroupAbsent(v.PID)
		deadline := time.Now().Add(time.Second)
		for !absent && observeErr == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			absent, observeErr = procx.ProcessGroupAbsent(v.PID)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.records[key] != v || r.children[key] != child {
			return
		}
		if err != nil && !slices.Contains(v.Errors, err.Error()) {
			v.Errors = append(v.Errors, err.Error())
		}
		if !absent || observeErr != nil {
			v.State = "needs_owner_readback"
			v.Error = fmt.Sprintf("launcher exited; owned process group absence unconfirmed: %v", observeErr)
			_ = r.persist()
			return
		}
		if len(v.ObservedDescendants) > 0 {
			rows, readErr := ObserveResidents(context.Background())
			if readErr != nil || residentDescendantsPresent(v, rows) {
				v.State = "needs_owner_readback"
				v.Error = fmt.Sprintf("launcher and group absent; observed descendant absence unconfirmed: %v", readErr)
				_ = r.persist()
				return
			}
		}
		v.State = "exited"
		v.Released = append(v.Released, v.Contexts...)
		v.Contexts = []string{}
		if err != nil {
			v.Error = err.Error()
		}
		delete(r.children, key)
		permit.Release()
		_ = r.persist()
		r.notifyReleased()
	}()
	if spec.TestFixture {
		saveErr = errors.Join(saveErr, r.registerFixture(spec, *v, logPath, contextID))
	}
	return cloneResident(v), saveErr
}
func (r *Residents) Release(id, contextID string) (Resident, error) {
	return r.release(id, contextID, "explicit_release")
}
func (r *Residents) release(id, contextID, reason string) (Resident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releaseLocked(id, contextID, reason)
}

// releaseLocked requires the original resident lock.
func (r *Residents) releaseLocked(id, contextID, reason string) (Resident, error) {
	v := r.records[id]
	if v == nil {
		return Resident{}, errors.New("resident identity not found")
	}
	if r.failure != nil {
		return cloneResident(v), fmt.Errorf("resident persistence failed; readback required: %w", r.failure)
	}
	if v.State == "needs_owner_readback" {
		return cloneResident(v), errors.New(v.Error)
	}
	if !slices.Contains(v.Contexts, contextID) {
		return cloneResident(v), nil
	}
	if child := r.children[id]; child != nil && child.checking {
		return cloneResident(v), errors.New("resident release guard check in progress")
	}
	if len(v.Contexts) == 1 && v.State == "running" && v.GuardID != "" {
		if err := r.checkRelease(v); err != nil {
			v.Error = "resident release guard refused: " + err.Error()
			return cloneResident(v), errors.Join(err, r.persist())
		}
	}
	if v.Sharing == "shared" && !v.TestFixture && v.GuardID == "" && len(v.Contexts) == 1 {
		return cloneResident(v), errors.New("legacy shared service drain unproven; original owner readback required")
	}
	for i := range v.ConsumerLeases {
		if v.ConsumerLeases[i].ID == contextID {
			v.ConsumerLeases[i].State = "released"
			v.ConsumerLeases[i].EndReason = reason
		}
	}
	v.Released = append(v.Released, contextID)
	v.Contexts = slices.DeleteFunc(v.Contexts, func(x string) bool { return x == contextID })
	if len(v.Contexts) == 0 && v.State == "running" {
		rows, readErr := ObserveResidents(context.Background())
		if readErr != nil {
			v.State = "needs_owner_readback"
			v.Error = "pre-stop descendant observation unavailable: " + readErr.Error()
			return cloneResident(v), errors.Join(readErr, r.persist())
		}
		rememberResidentDescendants(v, rows)
		v.State = "stopping"
		v.StopRequested = time.Now().UTC().Format(time.RFC3339Nano)
		if err := r.persist(); err != nil {
			return cloneResident(v), err
		}
		child := r.children[id]
		if child == nil {
			v.State = "needs_owner_readback"
			v.Error = "native child handle missing"
		} else if err := signalResident(v, child, false); err != nil && !errors.Is(err, os.ErrProcessDone) {
			v.State = "needs_owner_readback"
			v.Error = err.Error()
		} else if v.ForceAfterGrace {
			go r.forceAfterGrace(v, child)
		}
	}
	return cloneResident(v), r.persist()
}
func (r *Residents) ReleaseContext(contextID string) error {
	records, err := r.Snapshot()
	if err != nil {
		return err
	}
	for _, v := range records {
		if slices.Contains(v.Contexts, contextID) {
			if _, err = r.Release(v.ID, contextID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reconcile only proves absence of a previously recorded process. It never signals it.
func (r *Residents) Reconcile(ctx context.Context) ([]Resident, error) {
	rows, err := ObserveResidents(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, v := range r.records {
		if v.State != "needs_owner_readback" || v.PID <= 0 || v.Started == "" {
			continue
		}
		if child := r.children[v.ID]; child != nil {
			select {
			case <-child.done:
			default:
				// The original waiter must preserve exit evidence before a
				// replacement generation can reuse this resident identity.
				continue
			}
		}
		found := false
		for _, row := range rows {
			if row.PID == v.PID && row.Started == v.Started {
				found = true
				break
			}
		}
		if !found {
			if residentDescendantsPresent(v, rows) {
				continue
			}
			if v.ProcessGroup != v.PID {
				continue
			}
			absent, groupErr := procx.ProcessGroupAbsent(v.ProcessGroup)
			if groupErr != nil {
				return nil, groupErr
			}
			if !absent {
				continue
			}
			v.State = "exited"
			v.Error = "original process and owned group absent; no adoption or signal"
			v.Released = append(v.Released, v.Contexts...)
			v.Contexts = []string{}
			if child := r.children[v.ID]; child != nil {
				child.permit.Release()
				delete(r.children, v.ID)
			}
			changed = true
		}
	}
	if changed {
		if err = r.persist(); err != nil {
			return nil, err
		}
	}
	r.updateRecoveryBudget()
	out := []Resident{}
	for _, v := range r.records {
		out = append(out, cloneResident(v))
	}
	return out, r.failure
}
func (r *Residents) WaitClosed(ctx context.Context) error {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		r.mu.Lock()
		active := len(r.children)
		r.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d resident child exits not observed: %w", active, ctx.Err())
		case <-tick.C:
		}
	}
}

// Direct files need no Cmd pipe goroutines or second Wait after the sole reap.
func startResident(spec ResidentSpec, log *os.File) (*os.Process, error) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	return os.StartProcess(spec.Argv[0], spec.Argv, &os.ProcAttr{
		Dir: spec.Directory, Env: append([]string{}, spec.Environment...),
		Files: []*os.File{input, log, log}, Sys: residentProcessAttributes(),
	})
}
