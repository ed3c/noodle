package admission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/procx"
)

func (r *Residents) FixtureRecoveries() *FixtureRecovery { return r.fixtures }

func fixtureDirectoryPurpose(directory string) (string, error) {
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	directory = canonical
	directory = canonicalFixturePath(directory)
	if !strings.HasPrefix(directory, canonicalFixturePath(os.TempDir())+"/") {
		return "", errors.New("stateless fixture requires its dedicated temporary test directory")
	}
	for _, part := range strings.Split(filepath.Clean(directory), string(filepath.Separator)) {
		if !strings.HasPrefix(part, "Test") {
			continue
		}
		test := strings.TrimRight(part, "0123456789")
		if fixtureTestName.MatchString(test) && len(part) > len(test) {
			return test, nil
		}
	}
	return "", errors.New("stateless fixture test purpose missing; refuse before process launch")
}

func (r *Residents) registerFixture(spec ResidentSpec, v Resident, logPath, contextID string) error {
	test, err := fixtureDirectoryPurpose(spec.Directory)
	if err != nil {
		return err
	}
	c := FixtureCandidate{ConsumerManaged: len(v.ConsumerLeases) > 0, Origin: "registered", PID: v.PID, UID: os.Getuid(), Started: v.Started, Argv: append([]string{}, spec.Argv...), Directory: spec.Directory, LogPath: logPath, Test: test, ResidentID: v.ID}
	if native, err := procx.ObserveNativeIdentity(v.PID); err == nil {
		c.Native = &native
	}
	if owner, err := procx.ObserveNativeIdentity(os.Getpid()); err == nil {
		c.Owner = &owner
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	return r.fixtures.Register(FixtureManifest{Schema: 1, EvidenceSHA256: hex.EncodeToString(sum[:]), Candidates: []FixtureCandidate{c}}, contextID)
}

// Called by existing Host lifecycle ticks and explicit reconciliation only.
func (r *Residents) ReconcileFixtures(ctx context.Context) { r.fixtures.RunPending(ctx) }
