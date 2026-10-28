package admission

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

type FixtureEvidence struct {
	ObservedAt string               `json:"observed_at"`
	Native     procx.NativeIdentity `json:"native"`
	PS         string               `json:"ps"`
	OpenFiles  string               `json:"open_files"`
}

var fixtureTestName = regexp.MustCompile(`^Test[A-Za-z0-9_-]+$`)

func fixtureHelper(argv []string) bool {
	if len(argv) != 2 {
		return false
	}
	base := filepath.Base(argv[0])
	return (base == "admission.test" && (argv[1] == "-test.run=^TestResidentChildHelper$" || argv[1] == "-test.run=^TestResidentUnresponsiveHelper$")) ||
		(base == "noodle.test" && argv[1] == "-test.run=^TestHostResidentChildHelper$")
}

func canonicalFixturePath(path string) string {
	path = filepath.Clean(path)
	if strings.HasPrefix(path, "/var/") {
		return "/private" + path
	}
	return path
}

func validateFixtureCandidate(c FixtureCandidate) error {
	if c.Origin != "historical" && c.Origin != "registered" {
		return errors.New("fixture recovery origin unsupported")
	}
	if c.PID <= 1 || c.UID != os.Getuid() || c.Started == "" || !fixtureHelper(c.Argv) || !filepath.IsAbs(c.Argv[0]) || !fixtureTestName.MatchString(c.Test) {
		return errors.New("explicit test fixture identity, user, helper and purpose required")
	}
	dir := canonicalFixturePath(c.Directory)
	log := canonicalFixturePath(c.LogPath)
	if !filepath.IsAbs(dir) || !strings.HasPrefix(dir, canonicalFixturePath(os.TempDir())+"/") || !filepath.IsAbs(log) || !strings.HasPrefix(filepath.Base(log), "resident-") || filepath.Ext(log) != ".log" {
		return errors.New("fixture working directory and original resident log origin required")
	}
	found := false
	for _, part := range strings.Split(dir, "/") {
		if strings.HasPrefix(part, c.Test) && len(part) > len(c.Test) {
			found = true
		}
	}
	if !found {
		return errors.New("fixture directory/log do not match test purpose")
	}
	if !strings.Contains(log, "/"+c.Test) {
		if err := registeredFixtureLedger(c); err != nil {
			return err
		}
	}
	if c.Origin == "registered" && c.ResidentID == "" {
		return errors.New("registered fixture requires original resident identity")
	}
	if c.Origin == "historical" && (c.Owner != nil || c.Native != nil) {
		return errors.New("historical evidence must not fabricate original native handles")
	}
	return nil
}

// A production Host log has no Go test-directory prefix. Its original persisted
// resident plus native owner/child identities replace that test-only path proof.
// Historical recovery keeps the original directory/log rule above.
func registeredFixtureLedger(c FixtureCandidate) error {
	if c.Origin != "registered" || c.ResidentID == "" || c.Native == nil || c.Owner == nil ||
		c.Native.PID != c.PID || c.Native.UID != c.UID || c.Owner.PID <= 1 || c.Owner.UID != c.UID ||
		len(c.Native.Token) == 0 || len(c.Owner.Token) == 0 {
		return errors.New("fixture log outside test directory requires original native owner ledger")
	}
	sum := sha256.Sum256([]byte(c.ResidentID))
	if filepath.Base(c.LogPath) != fmt.Sprintf("resident-%x.log", sum[:8]) {
		return errors.New("fixture log does not match original resident identity")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(c.LogPath), "residents.json"))
	if err != nil {
		return fmt.Errorf("original fixture ledger unavailable: %w", err)
	}
	var rows map[string]Resident
	if err = json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	v, ok := rows[c.ResidentID]
	if ok {
		for _, generation := range append([]Resident{v}, v.History...) {
			if generation.ID == c.ResidentID && generation.PID == c.PID && generation.Started == c.Started && generation.TestFixture && c.ConsumerManaged == (len(generation.ConsumerLeases) > 0) {
				return nil
			}
		}
	}
	return errors.New("original fixture ledger identity differs")
}

func fixtureStarted(p procx.NativeIdentity) string {
	return strings.Join(strings.Fields(time.Unix(int64(p.StartSec), 0).Local().Format("Mon Jan _2 15:04:05 2006")), " ")
}

func fixtureGenerationGone(c FixtureCandidate, e *FixtureEvidence) (bool, error) {
	p, err := procx.ObserveNativeIdentity(c.PID)
	if errors.Is(err, procx.ErrProcessGone) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if fixtureStarted(p) != c.Started {
		return true, nil
	}
	if c.Native != nil && (c.Native.StartSec != p.StartSec || c.Native.StartUsec != p.StartUsec) {
		return true, nil
	}
	if e != nil && (e.Native.StartSec != p.StartSec || e.Native.StartUsec != p.StartUsec) {
		return true, nil
	}
	if p.Zombie {
		return true, nil
	}
	if p.UID != c.UID || (c.Native != nil && !c.Native.SameGeneration(p)) || (e != nil && !e.Native.SameGeneration(p)) {
		return false, errors.New("process identity changed without proven lifetime exit; readback only")
	}
	return false, nil
}

func observeFixtureEvidence(ctx context.Context, c FixtureCandidate) (FixtureEvidence, error) {
	e := FixtureEvidence{ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := validateFixtureCandidate(c); err != nil {
		return e, err
	}
	if !procx.NativeFixtureControlAvailable() {
		return e, errors.New("generation-bound native control unavailable; no PID fallback")
	}
	p, err := procx.ObserveNativeIdentity(c.PID)
	if err != nil {
		return e, err
	}
	if p.UID != c.UID || fixtureStarted(p) != c.Started || p.ParentPID != 1 || p.Group != c.PID || p.Zombie {
		return e, errors.New("fixture UID/start/orphan/group identity no longer matches")
	}
	if c.Native != nil && !c.Native.SameGeneration(p) {
		return e, errors.New("registered fixture generation changed")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ps := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(c.PID), "-o", "uid=,ppid=,pgid=,lstart=,command=")
	ps.Env = append(os.Environ(), "LC_ALL=C")
	raw, err := ps.Output()
	if err != nil {
		return e, fmt.Errorf("fixture argv observation: %w", err)
	}
	e.PS = string(raw)
	f := strings.Fields(e.PS)
	if len(f) < 10 || f[0] != strconv.Itoa(c.UID) || f[1] != "1" || f[2] != strconv.Itoa(c.PID) || strings.Join(f[3:8], " ") != c.Started || canonicalFixturePath(strings.Join(f[8:], " ")) != canonicalFixturePath(strings.Join(c.Argv, " ")) {
		return e, errors.New("frozen fixture argv/start/group mismatch")
	}
	files := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(c.PID), "-Fpcftn")
	raw, err = files.Output()
	if err != nil {
		return e, fmt.Errorf("fixture cwd/log observation: %w", err)
	}
	e.OpenFiles = string(raw)
	fd := ""
	cwd, stdout, stderr, image := false, false, false, false
	for _, line := range strings.Split(e.OpenFiles, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			if line[1:] != strconv.Itoa(c.PID) {
				return e, errors.New("unexpected process in fixture observation")
			}
		case 'f':
			fd = line[1:]
		case 't':
			if line[1:] == "IPv4" || line[1:] == "IPv6" {
				return e, errors.New("fixture has network sockets; purpose requires readback")
			}
		case 'n':
			path := canonicalFixturePath(line[1:])
			if fd == "cwd" && path == canonicalFixturePath(c.Directory) {
				cwd = true
			}
			if fd == "1" && path == canonicalFixturePath(c.LogPath) {
				stdout = true
			}
			if fd == "2" && path == canonicalFixturePath(c.LogPath) {
				stderr = true
			}
			if fd == "txt" && path == canonicalFixturePath(c.Argv[0]) {
				image = true
			}
		}
	}
	if !cwd || !stdout || !stderr || !image {
		return e, errors.New("fixture working directory, mapped image or original stdout/stderr log identity missing")
	}
	q, err := procx.ObserveNativeIdentity(c.PID)
	if err != nil || !p.SameGeneration(q) || p.ParentPID != q.ParentPID || p.Group != q.Group {
		return e, fmt.Errorf("fixture changed during evidence capture: %v", err)
	}
	e.Native = q
	return e, nil
}
