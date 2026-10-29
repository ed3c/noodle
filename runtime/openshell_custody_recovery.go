package runtime

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
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

const retainedInitializationBlock = "retained initialization supplied for existing custody"
const retainedRecoveryAuditFile = "retained-init-recovery.json"
const retainedRecoveryCompleteFile = "retained-init-recovery-complete.json"

type OpenShellRetainedRecovery struct {
	Status       string `json:"status"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
	EvidencePath string `json:"evidence_path"`
}
type retainedRecoveryAudit struct {
	Version                   int
	Action                    string
	Binding                   OpenShellCustodyBinding
	BeforeSHA256, AfterSHA256 string
	Before, After             []byte
}
type retainedRecoveryCompletion struct{ AuditSHA256, AfterSHA256 string }

func retainedRecoveryHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RecoverOpenShellRetainedInitialization repairs only the former local pre-start
// rejection. It holds original custody, performs no RPC, and preserves all input
// bytes before changing Blocked. Native retained readback remains mandatory.
func RecoverOpenShellRetainedInitialization(binding OpenShellCustodyBinding, retained []OpenShellRetainedSandbox, expectedSHA256 string, apply bool) (OpenShellRetainedRecovery, error) {
	return recoverOpenShellRetainedInitialization(binding, retained, expectedSHA256, apply, filex.WriteFileAtomicDurable)
}

func recoverOpenShellRetainedInitialization(binding OpenShellCustodyBinding, retained []OpenShellRetainedSandbox, expectedSHA256 string, apply bool, write func(string, []byte) error) (result OpenShellRetainedRecovery, err error) {
	for _, value := range []string{binding.StateRoot, binding.Gateway, binding.Workspace, binding.Owner, binding.ConfigSource, binding.ConfigFingerprint} {
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
			return result, fmt.Errorf("openshell recovery binding missing or invalid")
		}
	}
	hash, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(hash) != sha256.Size {
		return result, fmt.Errorf("reviewed owner SHA-256 invalid")
	}
	root, err := filepath.EvalSymlinks(binding.StateRoot)
	if err != nil {
		return result, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return result, err
	}
	binding.StateRoot = root
	lock, err := lockfile.TryLock(filepath.Join(root, "owner.lock"))
	if err != nil {
		return result, fmt.Errorf("original custody is still owned: %w", err)
	}
	defer lock.Close()
	for _, name := range []string{retainedRecoveryAuditFile, retainedRecoveryCompleteFile} {
		if _, err = os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			return result, fmt.Errorf("retained initialization recovery evidence already exists; automatic retry refused")
		}
	}
	ownerPath := filepath.Join(root, "owner.json")
	info, err := os.Lstat(ownerPath)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("custody ledger is not a regular file")
	}
	before, err := os.ReadFile(ownerPath)
	if err != nil {
		return result, err
	}
	if retainedRecoveryHash(before) != expectedSHA256 {
		return result, fmt.Errorf("reviewed custody ledger changed")
	}
	var state OpenShellCustodyState
	decoder := json.NewDecoder(bytes.NewReader(before))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&state); err != nil {
		return result, err
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return result, fmt.Errorf("custody ledger has trailing data")
	}
	legacy := state.Version == 3 && state.ControlRoot == "" && state.Handover == nil
	current := state.Version == openShellCustodyVersion && validateOpenShellControlRoot(state.ControlRoot) == nil
	if (!legacy && !current) || state.Binding != binding {
		return result, fmt.Errorf("retained recovery custody binding/version differs")
	}
	if current && state.Handover != nil {
		if err = verifyCustodyHandover(*state.Handover, state); err != nil {
			return result, err
		}
	}
	if state.Blocked != retainedInitializationBlock || len(state.Records) != 0 || len(state.PendingRetained) == 0 || len(retained) == 0 || !sameOpenShellRetained(state, retained) {
		return result, fmt.Errorf("custody is not the exact pre-start retained initialization rejection")
	}
	if err = (&OpenShellCustody{state: state}).validateLedger(); err != nil {
		return result, err
	}
	state.Blocked = ""
	after, err := json.Marshal(state)
	if err != nil {
		return result, err
	}
	result = OpenShellRetainedRecovery{Status: "eligible", BeforeSHA256: expectedSHA256, AfterSHA256: retainedRecoveryHash(after), EvidencePath: filepath.Join(root, retainedRecoveryAuditFile)}
	if !apply {
		return result, nil
	}
	audit := retainedRecoveryAudit{Version: 1, Action: "clear-local-retained-initialization-rejection", Binding: binding, BeforeSHA256: result.BeforeSHA256, AfterSHA256: result.AfterSHA256, Before: before, After: after}
	proof, err := json.Marshal(audit)
	if err != nil {
		return result, err
	}
	result.Status = "unknown_effect"
	if err = write(result.EvidencePath, proof); err != nil {
		return result, fmt.Errorf("persist retained recovery intent: %w", err)
	}
	if err = write(ownerPath, after); err != nil {
		return result, fmt.Errorf("persist retained recovery owner; pending evidence retained: %w", err)
	}
	observed, err := os.ReadFile(ownerPath)
	if err != nil {
		return result, fmt.Errorf("retained recovery owner readback failed: %w", err)
	}
	if retainedRecoveryHash(observed) != result.AfterSHA256 {
		return result, fmt.Errorf("retained recovery owner readback hash differs")
	}
	completion, _ := json.Marshal(retainedRecoveryCompletion{AuditSHA256: retainedRecoveryHash(proof), AfterSHA256: result.AfterSHA256})
	completePath := filepath.Join(root, retainedRecoveryCompleteFile)
	if err = write(completePath, completion); err != nil {
		// A renamed-but-unsynced completion is not success. Remove only that
		// completion marker; the immutable intent and original bytes stay intact.
		removeErr := os.Remove(completePath)
		if os.IsNotExist(removeErr) {
			removeErr = nil
		}
		return result, errors.Join(fmt.Errorf("persist retained recovery completion: %w", err), removeErr, filex.SyncDir(root))
	}
	result.Status = "applied"
	return result, nil
}

// Called with owner.lock held before any RPC or service spawn. An incomplete or
// malformed recovery journal is a local block, never an invitation to retry.
func verifyOpenShellRetainedRecovery(binding OpenShellCustodyBinding) error {
	root := binding.StateRoot
	proof, err := os.ReadFile(filepath.Join(root, retainedRecoveryAuditFile))
	if os.IsNotExist(err) {
		if _, e := os.Lstat(filepath.Join(root, retainedRecoveryCompleteFile)); os.IsNotExist(e) {
			return nil
		}
		return fmt.Errorf("retained recovery completion lacks its original evidence")
	}
	if err != nil {
		return err
	}
	var audit retainedRecoveryAudit
	if err = json.Unmarshal(proof, &audit); err != nil {
		return fmt.Errorf("retained recovery evidence invalid: %w", err)
	}
	if audit.Version != 1 || audit.Action != "clear-local-retained-initialization-rejection" || audit.Binding != binding || audit.BeforeSHA256 != retainedRecoveryHash(audit.Before) || audit.AfterSHA256 != retainedRecoveryHash(audit.After) {
		return fmt.Errorf("retained recovery evidence binding/hash differs")
	}
	raw, err := os.ReadFile(filepath.Join(root, retainedRecoveryCompleteFile))
	if err != nil {
		return fmt.Errorf("retained recovery incomplete; original evidence retained: %w", err)
	}
	var completed retainedRecoveryCompletion
	if err = json.Unmarshal(raw, &completed); err != nil {
		return err
	}
	if completed.AuditSHA256 != retainedRecoveryHash(proof) || completed.AfterSHA256 != audit.AfterSHA256 {
		return fmt.Errorf("retained recovery completion does not match its evidence")
	}
	return nil
}
