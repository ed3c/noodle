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
	"slices"
	"strings"

	"github.com/poteto/noodle/internal/procx"
)

type SourceReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type WorktreeIdentity struct {
	Root      string `json:"root"`
	GitDir    string `json:"git_dir,omitempty"`
	CommonDir string `json:"common_dir,omitempty"`
	HEAD      string `json:"head,omitempty"`
}
type ResidentConsumer struct {
	ID            string               `json:"id"`
	Connection    string               `json:"connection"`
	SessionID     string               `json:"session_id"`
	ContextID     string               `json:"context_id"`
	RequestID     string               `json:"request_id"`
	Worktree      WorktreeIdentity     `json:"worktree"`
	Process       procx.NativeIdentity `json:"process"`
	State         string               `json:"state"`
	EndReason     string               `json:"end_reason,omitempty"`
	Liveness      string               `json:"liveness,omitempty"`
	LivenessError string               `json:"liveness_error,omitempty"`
	verified      bool
}

func sourceDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func verifySourceReference(ref SourceReference) error {
	if !filepath.IsAbs(ref.Path) || len(ref.SHA256) != 64 {
		return errors.New("original absolute source reference required")
	}
	raw, err := os.ReadFile(ref.Path)
	if err != nil {
		return err
	}
	if sourceDigest(raw) != ref.SHA256 {
		return errors.New("original request source changed")
	}
	return nil
}

func ObserveWorktree(ctx context.Context, directory string) (WorktreeIdentity, error) {
	var w WorktreeIdentity
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return w, err
	}
	w.Root = root
	git := func(arg string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", arg)
		raw, e := cmd.Output()
		return strings.TrimSpace(string(raw)), e
	}
	top, err := git("--show-toplevel")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "not a git repository") {
			return w, nil
		}
		return w, fmt.Errorf("worktree identity readback: %w", err)
	}
	if w.Root, err = filepath.EvalSymlinks(top); err != nil {
		return w, err
	}
	if w.GitDir, err = git("--absolute-git-dir"); err != nil {
		return w, err
	}
	if w.CommonDir, err = git("--git-common-dir"); err != nil {
		return w, err
	}
	if !filepath.IsAbs(w.CommonDir) {
		w.CommonDir = filepath.Join(root, w.CommonDir)
	}
	if w.CommonDir, err = filepath.EvalSymlinks(w.CommonDir); err != nil {
		return w, err
	}
	w.HEAD, err = git("HEAD")
	return w, err
}

// Only the existing stateless fixture contract has proven request/effect
// quiescence here. Real services keep their own I/O and remain unsupported.
func (r *Residents) AcquireConsumer(spec ResidentSpec, c ResidentConsumer, budget *Budget) (Resident, error) {
	if !c.verified || c.Connection == "" || c.SessionID == "" || c.ContextID == "" || c.RequestID == "" || c.Worktree.Root == "" {
		return Resident{}, errors.New("verified native consumer required")
	}
	if !fixtureHelper(spec.Argv) {
		return Resident{}, errors.New("service client isolation and drain adapter unavailable; keep original private owner")
	}
	spec.TestFixture = true
	_, fingerprint, err := residentIdentity(spec, c.ContextID)
	if err != nil {
		return Resident{}, err
	}
	group := "fixture-" + fingerprint
	if spec.Sharing == "context" {
		group = c.ContextID + "/" + group
	}
	raw, err := json.Marshal([]string{group, c.Connection, c.SessionID, c.ContextID, c.RequestID, c.Worktree.Root, c.Worktree.GitDir})
	if err != nil {
		return Resident{}, err
	}
	c.ID = sourceDigest(raw)
	c.State = "active"
	c.Liveness = "observed"
	return r.acquire(spec, c.ID, c.ContextID, budget, &c, group)
}

func (r *Residents) ReleaseConsumer(id string, c ResidentConsumer, leaseID string) (Resident, error) {
	if !c.verified {
		return Resident{}, errors.New("verified native consumer required")
	}
	r.mu.Lock()
	v := r.records[id]
	owns := false
	if v != nil {
		for _, lease := range v.ConsumerLeases {
			if lease.ID == leaseID && lease.Connection == c.Connection && lease.SessionID == c.SessionID && lease.ContextID == c.ContextID && lease.RequestID == c.RequestID && lease.Process.SameGeneration(c.Process) && lease.Worktree.Root == c.Worktree.Root && lease.Worktree.GitDir == c.Worktree.GitDir {
				owns = true
				break
			}
		}
	}
	r.mu.Unlock()
	if !owns {
		return Resident{}, errors.New("selected lease does not belong to the native consumer")
	}
	return r.Release(id, leaseID)
}

// Called by the original Host tick. Missing observations preserve leases;
// Stop, low CPU, idle time and a context event do not terminate a consumer.
func (r *Residents) ReconcileConsumerLeases(ctx context.Context) error {
	rows, err := r.Snapshot()
	if err != nil {
		return err
	}
	for _, v := range rows {
		for _, lease := range v.ConsumerLeases {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lease.State != "active" || !slices.Contains(v.Contexts, lease.ID) {
				continue
			}
			current, observeErr := procx.ObserveNativeIdentity(lease.Process.PID)
			gone := errors.Is(observeErr, procx.ErrProcessGone) || (observeErr == nil && (current.Zombie || current.StartSec != lease.Process.StartSec || current.StartUsec != lease.Process.StartUsec))
			if !gone {
				state, problem := "observed", ""
				if observeErr != nil || !lease.Process.SameGeneration(current) {
					state = "unknown"
					problem = fmt.Sprintf("native consumer generation unconfirmed: %v", observeErr)
				}
				r.mu.Lock()
				record := r.records[v.ID]
				if record != nil {
					for i := range record.ConsumerLeases {
						item := &record.ConsumerLeases[i]
						if item.ID == lease.ID && (item.Liveness != state || item.LivenessError != problem) {
							item.Liveness = state
							item.LivenessError = problem
							err = r.persist()
							break
						}
					}
				}
				r.mu.Unlock()
				if err != nil {
					return err
				}
				continue
			}
			if _, err = r.release(v.ID, lease.ID, "native_consumer_exit"); err != nil {
				return err
			}
		}
	}
	return nil
}

// A dead Host is not proof that its independent native consumers ended.
// Read the original resident ledger; never create replacement lease evidence.
func fixtureConsumersEnded(c FixtureCandidate) error {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(c.LogPath), "residents.json"))
	if err != nil {
		return fmt.Errorf("original consumer ledger unavailable: %w", err)
	}
	var rows map[string]*Resident
	if err = json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	v := rows[c.ResidentID]
	if v == nil || v.PID != c.PID || v.Started != c.Started || !v.TestFixture || len(v.ConsumerLeases) == 0 {
		return errors.New("original consumer evidence missing or changed")
	}
	known := map[string]bool{}
	for _, lease := range v.ConsumerLeases {
		known[lease.ID] = true
		if lease.State == "released" && !slices.Contains(v.Contexts, lease.ID) {
			continue
		}
		if lease.State != "active" || lease.Process.PID <= 1 || lease.Process.StartSec == 0 || len(lease.Process.Token) == 0 {
			return errors.New("consumer termination unknown; preserve shared fixture")
		}
		current, e := procx.ObserveNativeIdentity(lease.Process.PID)
		gone := errors.Is(e, procx.ErrProcessGone) || (e == nil && (current.Zombie || current.StartSec != lease.Process.StartSec || current.StartUsec != lease.Process.StartUsec))
		if !gone {
			return errors.New("native consumer live or unobserved; preserve shared fixture")
		}
	}
	for _, id := range v.Contexts {
		if !known[id] {
			return errors.New("unattributed context lease remains; preserve shared fixture")
		}
	}
	return nil
}
