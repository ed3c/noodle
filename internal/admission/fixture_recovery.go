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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

// FixtureCandidate is an explicit evidence record, never a PID adoption request.
type FixtureCandidate struct {
	ConsumerManaged bool                  `json:"consumer_managed,omitempty"`
	Origin          string                `json:"origin"`
	PID             int                   `json:"pid"`
	UID             int                   `json:"uid"`
	Started         string                `json:"started"`
	Argv            []string              `json:"argv"`
	Directory       string                `json:"directory"`
	LogPath         string                `json:"log_path"`
	Test            string                `json:"test"`
	ResidentID      string                `json:"resident_id,omitempty"`
	Native          *procx.NativeIdentity `json:"native,omitempty"`
	Owner           *procx.NativeIdentity `json:"original_owner,omitempty"`
}

type FixtureManifest struct {
	Schema         int                `json:"schema"`
	EvidenceSHA256 string             `json:"evidence_sha256"`
	Candidates     []FixtureCandidate `json:"candidates"`
}

type FixtureAction struct {
	Kind     string           `json:"kind"`
	IntentAt string           `json:"intent_at"`
	ResultAt string           `json:"result_at,omitempty"`
	Result   string           `json:"result,omitempty"`
	Error    string           `json:"error,omitempty"`
	Evidence *FixtureEvidence `json:"evidence,omitempty"`
}

type FixtureReclamation struct {
	ID             string                `json:"id"`
	Binding        string                `json:"binding"`
	EvidenceSHA256 string                `json:"evidence_sha256"`
	Candidate      FixtureCandidate      `json:"candidate"`
	State          string                `json:"state"`
	Error          string                `json:"error,omitempty"`
	Evidence       *FixtureEvidence      `json:"evidence,omitempty"`
	Actions        []FixtureAction       `json:"actions"`
	ExitAt         string                `json:"exit_observed_at,omitempty"`
	OriginJournal  string                `json:"origin_journal,omitempty"`
	ClaimPath      string                `json:"claim_path,omitempty"`
	RecoveryOwner  *procx.NativeIdentity `json:"recovery_owner,omitempty"`
}

// FixtureRecovery is part of the existing Host, with a separate durable journal.
// An existing action intent is always read back; even an interrupted intent is
// never replayed. Only this invocation's acknowledged TERM may advance to KILL.
type FixtureRecovery struct {
	mu      sync.Mutex
	path    string
	rows    map[string]*FixtureReclamation
	running bool
	failure error
	claims  string
}

func OpenFixtureRecovery(directory string) (*FixtureRecovery, error) {
	m := &FixtureRecovery{path: filepath.Join(directory, "fixture-recovery.json"), claims: filepath.Join(directory, "fixture-recovery-claims"), rows: map[string]*FixtureReclamation{}}
	data, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &m.rows); err != nil {
		return nil, fmt.Errorf("fixture recovery journal: %w", err)
	}
	if m.rows == nil {
		return nil, errors.New("fixture recovery journal is null")
	}
	for id, row := range m.rows {
		if row == nil || row.ID != id || fixtureKey(row.Candidate) != id {
			return nil, errors.New("invalid fixture recovery journal identity")
		}
		if err = validateFixtureCandidate(row.Candidate); err != nil {
			return nil, fmt.Errorf("invalid fixture recovery candidate: %w", err)
		}
		if len(row.Actions) > 0 && row.State != "exited" {
			row.State = "unknown"
			row.Error = "prior control intent exists; readback only, never resend"
		}
	}
	return m, nil
}

func fixtureKey(c FixtureCandidate) string {
	// Exclude the approving request: a new request cannot repeat a prior control.
	data, _ := json.Marshal(struct {
		PID     int
		UID     int
		Started string
	}{c.PID, c.UID, c.Started})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (m *FixtureRecovery) saveLocked() error {
	if m.failure != nil {
		return m.failure
	}
	data, err := json.MarshalIndent(m.rows, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), "fixture-recovery-*.tmp")
	if err != nil {
		m.failure = err
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(f.Name(), m.path)
	}
	if err == nil {
		var dir *os.File
		dir, err = os.Open(filepath.Dir(m.path))
		if err == nil {
			err = errors.Join(dir.Sync(), dir.Close())
		}
	}
	if err != nil {
		m.failure = fmt.Errorf("fixture recovery persistence: %w", err)
	}
	return m.failure
}

func (m *FixtureRecovery) Register(manifest FixtureManifest, binding string) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	var frozen FixtureManifest
	if err = json.Unmarshal(data, &frozen); err != nil {
		return err
	}
	manifest = frozen
	var recoveryOwner *procx.NativeIdentity
	if owner, readErr := procx.ObserveNativeIdentity(os.Getpid()); readErr == nil {
		recoveryOwner = &owner
	}
	if manifest.Schema != 1 || binding == "" || len(manifest.Candidates) == 0 || len(manifest.Candidates) > 512 {
		return errors.New("explicit bound fixture manifest required (1..512 candidates)")
	}
	if raw, err := hex.DecodeString(manifest.EvidenceSHA256); err != nil || len(raw) != sha256.Size {
		return errors.New("fixture source evidence SHA256 required")
	}
	seen := map[string]bool{}
	for _, c := range manifest.Candidates {
		if err := validateFixtureCandidate(c); err != nil {
			return err
		}
		key := fixtureKey(c)
		if seen[key] {
			return errors.New("duplicate fixture generation in manifest")
		}
		seen[key] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return m.failure
	}
	for _, c := range manifest.Candidates {
		key := fixtureKey(c)
		if old := m.rows[key]; old != nil {
			before, _ := json.Marshal(old.Candidate)
			after, _ := json.Marshal(c)
			if string(before) != string(after) {
				return errors.New("frozen fixture evidence conflicts with registered generation")
			}
		}
	}
	for _, c := range manifest.Candidates {
		key := fixtureKey(c)
		if m.rows[key] == nil {
			m.rows[key] = &FixtureReclamation{ID: key, Binding: binding, EvidenceSHA256: manifest.EvidenceSHA256, Candidate: c, State: "registered", Actions: []FixtureAction{}, RecoveryOwner: recoveryOwner}
			if c.Origin == "registered" && (c.Native == nil || c.Owner == nil) {
				m.rows[key].State = "unknown"
				m.rows[key].Error = "native registration evidence unavailable; preserve original owner lifecycle"
			}
		}
	}
	return m.saveLocked()
}

// Snapshot does not persist or schedule any work. Exit observations are overlays.
func (m *FixtureRecovery) Snapshot() ([]FixtureReclamation, error) {
	m.mu.Lock()
	data, err := json.Marshal(m.rows)
	failure := m.failure
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var copy map[string]*FixtureReclamation
	if err = json.Unmarshal(data, &copy); err != nil {
		return nil, err
	}
	out := make([]FixtureReclamation, 0, len(copy))
	for _, v := range copy {
		if gone, readErr := fixtureGenerationGone(v.Candidate, v.Evidence); readErr == nil && gone {
			v.State = "exited"
			v.ExitAt = time.Now().UTC().Format(time.RFC3339Nano)
		} else if readErr != nil && v.State != "exited" {
			v.State = "unknown"
			v.Error = readErr.Error()
		}
		out = append(out, *v)
	}
	slices.SortFunc(out, func(a, b FixtureReclamation) int { return strings.Compare(a.ID, b.ID) })
	return out, failure
}

// RunPending runs a bounded batch on the existing Host's context. It creates no
// listener, polling service, or daemon; callers are existing lifecycle events.
func (m *FixtureRecovery) RunPending(ctx context.Context) {
	m.mu.Lock()
	if m.running || m.failure != nil {
		m.mu.Unlock()
		return
	}
	jobs := []FixtureReclamation{}
	for _, row := range m.rows {
		if len(row.Actions) == 0 && (row.State == "registered" || row.State == "waiting_owner" || row.State == "eligible") {
			jobs = append(jobs, *row)
		}
	}
	if len(jobs) == 0 {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()
	go func() {
		defer func() { m.mu.Lock(); m.running = false; m.mu.Unlock() }()
		work := make(chan FixtureReclamation)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for row := range work {
					m.recoverOne(ctx, row)
				}
			}()
		}
		for _, row := range jobs {
			select {
			case work <- row:
			case <-ctx.Done():
				close(work)
				wg.Wait()
				return
			}
		}
		close(work)
		wg.Wait()
	}()
}

func (m *FixtureRecovery) update(id, state, reason string, evidence *FixtureEvidence) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.rows[id]
	if v.State == state && v.Error == reason && evidence == nil {
		return m.failure
	}
	v.State = state
	v.Error = reason
	if evidence != nil {
		v.Evidence = evidence
	}
	if state == "exited" {
		v.ExitAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return m.saveLocked()
}

func (m *FixtureRecovery) act(id string, p procx.NativeIdentity, force bool) error {
	m.mu.Lock()
	v := m.rows[id]
	if m.failure != nil {
		err := m.failure
		m.mu.Unlock()
		return err
	}
	kind := "term"
	if force {
		kind = "kill"
	}
	if (!force && len(v.Actions) != 0) || (force && (len(v.Actions) != 1 || v.Actions[0].Result != "sent")) {
		m.mu.Unlock()
		return errors.New("existing or unknown control intent; readback only")
	}
	if !force {
		if err := m.claimLocked(v, p); err != nil {
			v.State = "unknown"
			v.Error = err.Error()
			saveErr := m.saveLocked()
			m.mu.Unlock()
			return errors.Join(err, saveErr)
		}
	}
	v.Actions = append(v.Actions, FixtureAction{Kind: kind, IntentAt: time.Now().UTC().Format(time.RFC3339Nano), Evidence: v.Evidence})
	v.State = "control_pending"
	v.Error = "control intent persisted; response pending"
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	err = procx.SignalNativeIdentity(p, force)
	m.mu.Lock()
	defer m.mu.Unlock()
	a := &v.Actions[len(v.Actions)-1]
	a.ResultAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err != nil {
		v.State = "unknown"
		a.Result = "unknown"
		a.Error = err.Error()
		v.Error = err.Error()
	} else {
		a.Result = "sent"
		v.State = "stopping"
		v.Error = ""
	}
	return errors.Join(err, m.saveLocked())
}

func (m *FixtureRecovery) recoverOne(ctx context.Context, row FixtureReclamation) {
	c := row.Candidate
	if gone, err := fixtureGenerationGone(c, row.Evidence); err != nil {
		_ = m.update(row.ID, "unknown", err.Error(), nil)
		return
	} else if gone {
		_ = m.update(row.ID, "exited", "original generation absent; no control", nil)
		return
	}
	if c.Owner != nil {
		owner, err := procx.ObserveNativeIdentity(c.Owner.PID)
		if err != nil && !errors.Is(err, procx.ErrProcessGone) {
			_ = m.update(row.ID, "unknown", "owner exit unavailable: "+err.Error(), nil)
			return
		}
		if err == nil && !owner.Zombie && c.Owner.SameGeneration(owner) {
			_ = m.update(row.ID, "waiting_owner", "original owner still live; preserve leases", nil)
			return
		}
	}
	evidence, err := observeFixtureEvidence(ctx, c)
	if err != nil {
		_ = m.update(row.ID, "unknown", err.Error(), nil)
		return
	}
	if c.ConsumerManaged {
		if consumerErr := fixtureConsumersEnded(c); consumerErr != nil {
			_ = m.update(row.ID, "waiting_owner", consumerErr.Error(), nil)
			return
		}
	}
	if err = m.update(row.ID, "eligible", "", &evidence); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		_ = m.update(row.ID, "unknown", err.Error(), nil)
		return
	}
	if err = m.act(row.ID, evidence.Native, false); err != nil {
		return
	}
	deadline := time.Now().Add(residentStopGrace)
	for {
		gone, readErr := fixtureGenerationGone(c, &evidence)
		if readErr != nil {
			_ = m.update(row.ID, "unknown", readErr.Error(), nil)
			return
		}
		if gone {
			_ = m.update(row.ID, "exited", "original generation exit observed", nil)
			return
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			_ = m.update(row.ID, "unknown", ctx.Err().Error(), nil)
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	fresh, err := observeFixtureEvidence(ctx, c)
	if err != nil || !evidence.Native.SameGeneration(fresh.Native) {
		_ = m.update(row.ID, "unknown", fmt.Sprintf("force revalidation failed: %v", err), nil)
		return
	}
	if err = m.update(row.ID, "eligible", "", &fresh); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		_ = m.update(row.ID, "unknown", err.Error(), nil)
		return
	}
	if err = m.act(row.ID, fresh.Native, true); err != nil {
		return
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gone, readErr := fixtureGenerationGone(c, &fresh)
		if readErr != nil {
			_ = m.update(row.ID, "unknown", readErr.Error(), nil)
			return
		}
		if gone {
			_ = m.update(row.ID, "exited", "original generation exit observed", nil)
			return
		}
		select {
		case <-ctx.Done():
			_ = m.update(row.ID, "unknown", ctx.Err().Error(), nil)
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	_ = m.update(row.ID, "unknown", "force acknowledged; exit still unobserved; readback only", nil)
}
