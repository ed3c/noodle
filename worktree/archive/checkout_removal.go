package archive

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/poteto/noodle/internal/lockfile"
)

// CheckoutRemoval records an observed native removal. It grants no authority
// to remove a checkout; that decision remains in the original Loop actor.
type CheckoutRemoval struct {
	Schema          int              `json:"schema"`
	Operation       string           `json:"operation"`
	Identity        CheckoutIdentity `json:"identity"`
	PacketSignature string           `json:"packet_signature"`
	State           string           `json:"state"`
	Signature       string           `json:"signature"`
}

type CheckoutReclamationEvidence struct {
	Identity            CheckoutIdentity `json:"identity"`
	Packet              FileRef          `json:"packet"`
	Removal             FileRef          `json:"removal"`
	VerificationRelease FileRef          `json:"verification_release"`
}

// ReadCheckoutReclamation is stricter than removal readiness. Capacity cannot
// settle until the original verified restoration copy was also released.
func ReadCheckoutReclamation(p CheckoutPacket) (CheckoutReclamationEvidence, error) {
	var evidence CheckoutReclamationEvidence
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return evidence, err
	}
	lock, err := lockfile.TryLock(packetPath(c, p.Request.Operation) + ".release.lock")
	if err != nil {
		return evidence, err
	}
	defer lock.Close()
	if _, err = inspectCheckoutRemoval(p); err != nil {
		return evidence, err
	}
	if _, err = readRemoval(c, p, true); err != nil {
		return evidence, fmt.Errorf("original verification copy release is incomplete: %w", err)
	}
	evidence.Identity = p.Request.Identity
	for _, row := range []struct {
		path string
		ref  *FileRef
	}{{packetPath(c, p.Request.Operation), &evidence.Packet}, {removalPath(c, p, false), &evidence.Removal}, {removalPath(c, p, true), &evidence.VerificationRelease}} {
		hash, err := FileHash(row.path)
		if err != nil {
			return evidence, err
		}
		*row.ref = FileRef{Path: row.path, SHA256: hash}
	}
	return evidence, nil
}

func removalPath(c Config, p CheckoutPacket, released bool) string {
	suffix := ".removed.json"
	if released {
		suffix = ".verification-released.json"
	}
	return filepath.Join(filepath.Dir(packetPath(c, p.Request.Operation)), rootKey(p.Request.Operation)+suffix)
}

func removalIntentPath(c Config, p CheckoutPacket) string {
	return packetPath(c, p.Request.Operation) + ".remove-intent.json"
}

// BeginCheckoutRemoval seals the original effect attempt once. A saved intent
// is never permission to send the native remove command again.
func BeginCheckoutRemoval(p CheckoutPacket) error {
	if _, err := InspectCheckout(p); err != nil {
		return err
	}
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return err
	}
	intent, err := signRemoval(c, expectedRemoval(p, "remove_pending"))
	if err != nil {
		return err
	}
	if err = publishImmutable(removalIntentPath(c, p), intent); err != nil {
		return fmt.Errorf("original checkout removal intent exists or is UNKNOWN; read back, never replay: %w", err)
	}
	return nil
}

func CheckoutRemovalPending(p CheckoutPacket) (bool, error) {
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(removalIntentPath(c, p))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var intent CheckoutRemoval
	if err = DecodeCheckoutJSON(raw, &intent); err != nil {
		return false, err
	}
	expected, err := signRemoval(c, expectedRemoval(p, "remove_pending"))
	if err != nil || !reflect.DeepEqual(intent, expected) {
		return false, fmt.Errorf("original checkout removal intent differs: %v", err)
	}
	return true, nil
}

func signRemoval(c Config, r CheckoutRemoval) (CheckoutRemoval, error) {
	r.Signature = ""
	signature, err := authenticate(c, r)
	r.Signature = signature
	return r, err
}

func expectedRemoval(p CheckoutPacket, state string) CheckoutRemoval {
	return CheckoutRemoval{Schema: 1, Operation: p.Request.Operation, Identity: p.Request.Identity, PacketSignature: p.Signature, State: state}
}

func readRemoval(c Config, p CheckoutPacket, released bool) (CheckoutRemoval, error) {
	var r CheckoutRemoval
	data, err := os.ReadFile(removalPath(c, p, released))
	if err != nil {
		return r, err
	}
	if err = DecodeCheckoutJSON(data, &r); err != nil {
		return r, err
	}
	state := "removed"
	if released {
		state = "verification_released"
	}
	expected, err := signRemoval(c, expectedRemoval(p, state))
	if err != nil || !reflect.DeepEqual(r, expected) || !hmac.Equal([]byte(r.Signature), []byte(expected.Signature)) {
		return r, fmt.Errorf("original checkout removal evidence differs: %v", err)
	}
	return r, nil
}

func verifyRemovedSource(p CheckoutPacket) error {
	for _, path := range []string{p.Source.Path, p.Source.Admin} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("checkout removal is not observed; preserve identity %s: %v", path, err)
		}
	}
	head, err := git(p.Source.Common, "rev-parse", "--verify", p.Source.Branch)
	if err != nil || strings.TrimSpace(head) != p.Source.Head {
		return fmt.Errorf("removed checkout branch was not retained: %v", err)
	}
	return nil
}

func verifyPoolClosure(p CheckoutPacket) error {
	if err := canonical(p.Pool); err != nil {
		return err
	}
	if err := filepath.WalkDir(filepath.Join(p.Pool, "objects"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive pool object symlink HOLD: %s", path)
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(p.Pool, "objects", "info", "alternates")); !os.IsNotExist(err) {
		return errors.New("archive pool borrows objects or has unknown storage")
	}
	if _, err := git(p.Pool, "fsck", "--full", "--no-reflogs", p.Source.Head); err != nil {
		return err
	}
	head, err := git(p.Pool, "rev-parse", "--verify", p.CandidateRef)
	if err != nil || strings.TrimSpace(head) != p.Source.Head {
		return fmt.Errorf("archive pool candidate closure differs: %v", err)
	}
	return nil
}

// ObserveCheckoutRemovalBeforeReceipt qualifies a lost removal acknowledgement
// from the existing packet, without resending removal or writing its receipt.
func ObserveCheckoutRemovalBeforeReceipt(p CheckoutPacket) error {
	if _, err := inspectStoredCheckout(p); err != nil {
		return err
	}
	if pending, err := CheckoutRemovalPending(p); err != nil || !pending {
		return fmt.Errorf("original native checkout removal intent unavailable: %v", err)
	}
	if err := verifyRemovedSource(p); err != nil {
		return err
	}
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return err
	}
	return verifyRecovery(c, p)
}

// RecordCheckoutRemoval is called only after the native worktree owner observes
// that removal completed and the original branch still names its exact commit.
func RecordCheckoutRemoval(p CheckoutPacket) (CheckoutRemoval, error) {
	var result CheckoutRemoval
	if _, err := inspectStoredCheckout(p); err != nil {
		return result, err
	}
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return result, err
	}
	lock, err := lockfile.TryLock(packetPath(c, p.Request.Operation) + ".release.lock")
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err = verifyRemovedSource(p); err != nil {
		return result, err
	}
	if pending, err := CheckoutRemovalPending(p); err != nil || !pending {
		return result, fmt.Errorf("original native checkout removal intent unavailable: %v", err)
	}
	if original, err := readRemoval(c, p, false); err == nil {
		return original, nil
	} else if !os.IsNotExist(err) {
		return result, err
	}
	if len(p.VerificationCopy.Entries) == 0 {
		return result, errors.New("original verified copy identity is unavailable")
	}
	if err = verifyRecovery(c, p); err != nil {
		return result, err
	}
	result, err = signRemoval(c, expectedRemoval(p, "removed"))
	if err != nil {
		return result, err
	}
	if err = publishImmutable(removalPath(c, p, false), result); err != nil {
		return result, fmt.Errorf("checkout removal publication UNKNOWN; no replay: %w", err)
	}
	return result, nil
}

// InspectCheckoutRemoval reads the known effect and the retained pool/CAS
// closure. Missing or partially released verification bytes remain UNKNOWN.
func InspectCheckoutRemoval(p CheckoutPacket) (CheckoutRemoval, error) {
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return CheckoutRemoval{}, err
	}
	lock, err := lockfile.TryLock(packetPath(c, p.Request.Operation) + ".release.lock")
	if err != nil {
		return CheckoutRemoval{}, err
	}
	defer lock.Close()
	return inspectCheckoutRemoval(p)
}

func inspectCheckoutRemoval(p CheckoutPacket) (CheckoutRemoval, error) {
	var result CheckoutRemoval
	if _, err := inspectStoredCheckout(p); err != nil {
		return result, err
	}
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return result, err
	}
	if result, err = readRemoval(c, p, false); err != nil {
		return result, err
	}
	if err = verifyRemovedSource(p); err != nil {
		return result, err
	}
	if err = verifyPoolClosure(p); err != nil {
		return result, err
	}
	if _, err = readRemoval(c, p, true); err == nil {
		if _, err = os.Lstat(filepath.Dir(p.Recovery)); !os.IsNotExist(err) {
			return result, fmt.Errorf("released verification copy reappeared or is unknown: %v", err)
		}
		return result, nil
	} else if !os.IsNotExist(err) {
		return result, err
	}
	return result, verifyRecovery(c, p)
}

// ReleaseCheckoutVerification removes only this producer's exact, unchanged
// verification copy, after a known checkout removal. Pool/CAS evidence remains.
func ReleaseCheckoutVerification(p CheckoutPacket) error {
	c, err := checkoutConfig(p.Request)
	if err != nil {
		return err
	}
	lock, err := lockfile.TryLock(packetPath(c, p.Request.Operation) + ".release.lock")
	if err != nil {
		return fmt.Errorf("verification copy has another owner consumer: %w", err)
	}
	defer lock.Close()
	if _, err = inspectCheckoutRemoval(p); err != nil {
		return err
	}
	if _, err = readRemoval(c, p, true); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = verifyRecovery(c, p); err != nil {
		return err
	}
	if err = os.RemoveAll(filepath.Dir(p.Recovery)); err != nil {
		return fmt.Errorf("verification release UNKNOWN; no replay: %w", err)
	}
	if err = syncDir(filepath.Dir(filepath.Dir(p.Recovery))); err != nil {
		return err
	}
	released, err := signRemoval(c, expectedRemoval(p, "verification_released"))
	if err != nil {
		return err
	}
	if err = publishImmutable(removalPath(c, p, true), released); err != nil {
		return fmt.Errorf("verification release publication UNKNOWN; no replay: %w", err)
	}
	_, err = inspectCheckoutRemoval(p)
	return err
}

// ReadCheckoutPacket reads the original immutable operation, without retrying it.
func ReadCheckoutPacket(config FileRef, operation string) (CheckoutPacket, error) {
	var packet CheckoutPacket
	raw, err := readRef(config)
	if err != nil {
		return packet, err
	}
	c, err := parseConfig(raw)
	if err != nil {
		return packet, err
	}
	raw, err = os.ReadFile(packetPath(c, operation))
	if err != nil {
		return packet, err
	}
	if err = json.Unmarshal(raw, &packet); err != nil {
		return packet, err
	}
	if packet.Request.Config != config || packet.Request.Operation != operation {
		return packet, errors.New("original checkout packet source differs")
	}
	return inspectStoredCheckout(packet)
}
