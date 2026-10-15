package admission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// ResidentSpec is an explicit service contract. Stdio sessions cannot be shared.
type ResidentSpec struct {
	Name        string   `json:"name"`
	Argv        []string `json:"argv"`
	Directory   string   `json:"directory"`
	Environment []string `json:"environment"`
	Sharing     string   `json:"sharing"`
	Transport   string   `json:"transport"`
}
type Resident struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Fingerprint string   `json:"fingerprint"`
	Sharing     string   `json:"sharing"`
	PID         int      `json:"pid"`
	Started     string   `json:"started"`
	State       string   `json:"state"`
	Contexts    []string `json:"contexts"`
	Released    []string `json:"released_contexts"`
	Error       string   `json:"error,omitempty"`
}
type residentChild struct {
	command *exec.Cmd
	permit  *Permit
}

// Residents belongs to the existing Host. Only its own child handles can be stopped.
type Residents struct {
	mu       sync.Mutex
	path     string
	records  map[string]*Resident
	children map[string]*residentChild
	failure  error
}

func OpenResidents(directory string) (*Residents, error) {
	r := &Residents{path: filepath.Join(directory, "residents.json"), records: map[string]*Resident{}, children: map[string]*residentChild{}}
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
	if err := r.save(); err != nil {
		r.failure = err
		return err
	}
	return nil
}
func cloneResident(v *Resident) Resident {
	out := *v
	out.Contexts = append([]string{}, v.Contexts...)
	out.Released = append([]string{}, v.Released...)
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
	if !residentName.MatchString(spec.Name) || len(spec.Argv) == 0 || !filepath.IsAbs(spec.Argv[0]) || !filepath.IsAbs(spec.Directory) {
		return "", "", errors.New("resident name, absolute executable or directory missing")
	}
	if spec.Transport != "network" || (spec.Sharing != "shared" && spec.Sharing != "context") {
		return "", "", errors.New("resident transport/sharing unsupported; stdio requires its original Session owner")
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return Resident{}, fmt.Errorf("resident persistence failed; readback required: %w", r.failure)
	}
	key, fingerprint, err := residentIdentity(spec, contextID)
	if err != nil {
		return Resident{}, err
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
			if err = r.persist(); err != nil {
				return cloneResident(old), err
			}
		}
		return cloneResident(old), nil
	}
	if budget == nil {
		return Resident{}, errors.New("resident resource admission unavailable")
	}
	permit, err := budget.TryAcquireResident(contextID)
	if err != nil {
		return Resident{}, err
	}
	v := &Resident{ID: key, Name: spec.Name, Fingerprint: fingerprint, Sharing: spec.Sharing, State: "starting", Contexts: []string{contextID}}
	if old := r.records[key]; old != nil {
		v.Released = append([]string{}, old.Released...)
	}
	r.records[key] = v
	if err = r.persist(); err != nil {
		permit.Release()
		return cloneResident(v), err
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Directory
	cmd.Env = append([]string{}, spec.Environment...)
	// Each resident uses one log, not a per-context pipe or persistent proxy.
	sum := sha256.Sum256([]byte(key))
	logPath := filepath.Join(filepath.Dir(r.path), "resident-"+hex.EncodeToString(sum[:8])+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err == nil {
		cmd.Stdout = log
		cmd.Stderr = log
		err = cmd.Start()
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
	v.PID = cmd.Process.Pid
	v.Error = "process start identity not observed; restart reconciliation requires owner readback"
	if rows, readErr := ObserveResidents(context.Background()); readErr == nil {
		for _, row := range rows {
			if row.PID == v.PID {
				v.Started = row.Started
				v.Error = ""
				break
			}
		}
	} else {
		v.Error = "process start identity unavailable: " + readErr.Error()
	}
	v.State = "running"
	r.children[key] = &residentChild{command: cmd, permit: permit}
	saveErr := r.persist()
	go func() {
		err := cmd.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		v.State = "exited"
		v.Released = append(v.Released, v.Contexts...)
		v.Contexts = []string{}
		if err != nil {
			v.Error = err.Error()
		}
		delete(r.children, key)
		permit.Release()
		_ = r.persist()
	}()
	return cloneResident(v), saveErr
}
func (r *Residents) Release(id, contextID string) (Resident, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.records[id]
	if v == nil {
		return Resident{}, errors.New("resident identity not found")
	}
	if v.State == "needs_owner_readback" {
		return cloneResident(v), errors.New(v.Error)
	}
	if !slices.Contains(v.Contexts, contextID) {
		return cloneResident(v), nil
	}
	v.Released = append(v.Released, contextID)
	v.Contexts = slices.DeleteFunc(v.Contexts, func(x string) bool { return x == contextID })
	if len(v.Contexts) == 0 && v.State == "running" {
		v.State = "stopping"
		if err := r.persist(); err != nil {
			return cloneResident(v), err
		}
		child := r.children[id]
		if child == nil {
			v.State = "needs_owner_readback"
			v.Error = "native child handle missing"
		} else if err := stopResident(child.command.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			v.State = "needs_owner_readback"
			v.Error = err.Error()
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
		found := false
		for _, row := range rows {
			if row.PID == v.PID && row.Started == v.Started {
				found = true
				break
			}
		}
		if !found {
			v.State = "exited"
			v.Error = "original process absent in fresh inventory; no adoption or signal"
			v.Released = append(v.Released, v.Contexts...)
			v.Contexts = []string{}
			changed = true
		}
	}
	if changed {
		if err = r.persist(); err != nil {
			return nil, err
		}
	}
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
