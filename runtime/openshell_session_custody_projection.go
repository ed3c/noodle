package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/big"
	"path/filepath"
	"strings"
)

const SelectedSessionStopped = "selected_session_stopped"

type SessionCustodySubject struct {
	ContextID       string `json:"context_id"`
	OrderID         string `json:"order_id"`
	SessionID       string `json:"session_id"`
	NativeAttemptID string `json:"native_attempt_id"`
}

type SessionCustodyProjection struct {
	Schema               int                    `json:"schema"`
	Scope                string                 `json:"scope"`
	Subject              SessionCustodySubject   `json:"subject"`
	ProfileSHA256        string                 `json:"profile_sha256"`
	RootSHA256           string                 `json:"root_sha256"`
	SelectedRecordSHA256 string                 `json:"selected_record_sha256"`
	Before               SessionReadbackFileRef `json:"before"`
	After                SessionReadbackFileRef `json:"after"`
}

type completionCustodySnapshot struct {
	ref                              SessionReadbackFileRef
	record                           OpenShellCustodyRecord
	rootSHA, selectedSHA, retainedSHA string
}

type completionCustodyQualification struct {
	// A wire-shaped struct alone cannot acquire the standalone producer's local
	// qualification. The contract's serialized proof remains unchanged.
	projection  SessionCustodyProjection
	identity    OpenShellIdentity
	retainedSHA string
}

// The raw tree and typed ledger come from one bounded read. Typed decoding alone
// would discard opaque fields and silently round large numbers in projections.
func readCompletionCustody(spec OpenShellGatewaySpec, subject SessionCustodySubject, expected *OpenShellIdentity) (out completionCustodySnapshot, err error) {
	var raw json.RawMessage
	out.ref, err = ReadSessionReadbackFile(filepath.Join(spec.Binding.StateRoot, "owner.json"), &raw)
	if err != nil {
		return out, err
	}
	root, err := completionJSON(raw)
	if err != nil {
		return out, err
	}
	object, ok := root.(map[string]any)
	if !ok {
		return out, fmt.Errorf("original observer custody root is not an object")
	}
	var custody OpenShellCustodyState
	if err = json.Unmarshal(raw, &custody); err != nil {
		return out, fmt.Errorf("original observer custody shape invalid")
	}
	if !openShellCustodySupportsProfile(custody, spec) || custody.Binding != spec.Binding || custody.ControlRoot != spec.ControlRoot || custody.Blocked != "" {
		return out, fmt.Errorf("original observer custody binding differs")
	}
	if err = validateOpenShellCustodyLedger(custody); err != nil {
		return out, err
	}
	if custody.Handover != nil {
		if err = verifyCustodyHandover(*custody.Handover, custody); err != nil {
			return out, err
		}
	}
	if err = verifyOpenShellRetainedRecovery(spec.Binding); err != nil {
		return out, err
	}
	if len(spec.Retained) != 0 && !sameOpenShellRetained(custody, spec.Retained) {
		return out, fmt.Errorf("original retained receipt set differs")
	}
	rows, ok := object["Records"].([]any)
	if !ok || len(rows) != len(custody.Records) {
		return out, fmt.Errorf("original observer custody records shape differs")
	}
	// Reject aliases of the two excluded collections. No alternate spelling may
	// feed the typed decoder while escaping the raw selected-record digest.
	for key := range object {
		if (strings.EqualFold(key, "Records") && key != "Records") || (strings.EqualFold(key, "Credentials") && key != "Credentials") {
			return out, fmt.Errorf("original observer custody collection alias")
		}
	}
	selected, count := -1, 0
	retained := map[string]any{}
	for i, row := range custody.Records {
		if row.Retained {
			retained[row.Name] = rows[i]
		}
		id := row.Worker.Identity
		if id.OrderID == subject.OrderID && id.SessionID == subject.SessionID && id.AttemptID == subject.NativeAttemptID {
			selected, count = i, count+1
		}
	}
	if count != 1 {
		return out, fmt.Errorf("original observer completion missing or ambiguous")
	}
	out.record = custody.Records[selected]
	identity := out.record.Worker.Identity
	if expected != nil && identity != *expected {
		return out, fmt.Errorf("original observer native identity differs")
	}
	if err = validateCompletedRecord(out.record, identity); err != nil {
		return out, err
	}
	out.selectedSHA, err = completionJSONDigest(rows[selected])
	if err != nil {
		return out, err
	}
	out.retainedSHA, err = completionJSONDigest(retained)
	if err != nil {
		return out, err
	}
	delete(object, "Records")
	delete(object, "Credentials")
	out.rootSHA, err = completionJSONDigest(object)
	return out, err
}

// VerifyCompletedSessionReadback is reserved for the qualified Get-only caller.
// Other readers continue to use VerifySessionReadbackFiles without an exception.
func VerifyCompletedSessionReadback(e CompletedSessionReadback, profile OpenShellProfile, contextID, orderID, sessionID string) error {
	p := e.CustodyProjection
	if p == nil {
		return VerifySessionReadbackFiles(e.Sources)
	}
	qualification := e.custodyQualification
	if qualification == nil || qualification.projection != *p {
		return fmt.Errorf("original standalone custody qualification missing or changed")
	}
	subject := SessionCustodySubject{contextID, orderID, sessionID, e.NativeAttemptID}
	if contextID == "" || orderID == "" || sessionID == "" || e.NativeAttemptID == "" || p.Schema != 1 || p.Scope != SelectedSessionStopped || p.Subject != subject || p.ProfileSHA256 != e.ProfileSHA256 {
		return fmt.Errorf("original completion custody projection subject differs")
	}
	for _, digest := range []string{p.ProfileSHA256, p.RootSHA256, p.SelectedRecordSHA256, p.Before.SHA256, p.After.SHA256} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || strings.ToLower(digest) != digest {
			return fmt.Errorf("original completion custody projection digest invalid")
		}
	}
	if err := VerifyOpenShellProfile(profile); err != nil {
		return err
	}
	profileSHA, err := OpenShellDispatchSHA256(profile)
	if err != nil || profileSHA != e.ProfileSHA256 {
		return fmt.Errorf("original completion custody projection profile differs")
	}
	spec := profile.Gateway
	spec.Binding.StateRoot, err = filepath.EvalSymlinks(spec.Binding.StateRoot)
	if err != nil {
		return err
	}
	spec.ControlRoot, err = filepath.EvalSymlinks(spec.ControlRoot)
	if err != nil {
		return err
	}
	path := filepath.Join(spec.Binding.StateRoot, "owner.json")
	ref, exists := e.Sources["native_custody"]
	if !exists || p.Before.Path != path || p.After.Path != path || p.After != ref {
		return fmt.Errorf("original completion custody projection source differs")
	}
	current, err := readCompletionCustody(spec, subject, &qualification.identity)
	if err != nil {
		return err
	}
	if current.ref.Path != path || current.rootSHA != p.RootSHA256 || current.selectedSHA != p.SelectedRecordSHA256 || current.retainedSHA != qualification.retainedSHA {
		return fmt.Errorf("original completion custody projection changed")
	}
	record := current.record
	commandSHA, err := OpenShellCommandSHA256(record.Exec.Command)
	if err != nil || commandSHA != e.CommandSHA256 || e.NativePhase != "Stopped" || e.NativeExecExitCode != 0 || !e.StopAcknowledged || e.MainProcessInstanceID != record.MainProcessInstanceID {
		return fmt.Errorf("original completion custody projection execution differs")
	}
	refs := maps.Clone(e.Sources)
	delete(refs, "native_custody")
	if err = VerifySessionReadbackFiles(refs); err != nil {
		return err
	}
	afterRoot, err := filepath.EvalSymlinks(profile.Gateway.Binding.StateRoot)
	if err != nil || afterRoot != spec.Binding.StateRoot {
		return fmt.Errorf("original completion custody root changed")
	}
	afterControl, err := filepath.EvalSymlinks(profile.Gateway.ControlRoot)
	if err != nil || afterControl != spec.ControlRoot {
		return fmt.Errorf("original completion control root changed")
	}
	return VerifyOpenShellProfile(profile)
}

func completionJSONDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("original custody projection encoding failed")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Token decoding rejects duplicate object keys and preserves exact decimal
// values. Exponents stay symbolic so a bounded input cannot expand to 10^N bytes.
func completionJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 10000 {
			return nil, fmt.Errorf("custody JSON nesting exceeds bound")
		}
		token, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("custody JSON shape invalid")
		}
		switch value := token.(type) {
		case json.Delim:
			if value == '{' {
				object := map[string]any{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return nil, fmt.Errorf("custody JSON object invalid")
					}
					name, ok := key.(string)
					if !ok {
						return nil, fmt.Errorf("custody JSON key invalid")
					}
					if _, exists := object[name]; exists {
						return nil, fmt.Errorf("custody JSON key repeated")
					}
					object[name], err = read(depth + 1)
					if err != nil {
						return nil, err
					}
				}
				_, err = d.Token()
				return object, err
			}
			if value == '[' {
				array := []any{}
				for d.More() {
					element, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					array = append(array, element)
				}
				_, err = d.Token()
				return array, err
			}
			return nil, fmt.Errorf("custody JSON delimiter invalid")
		case json.Number:
			return canonicalCompletionNumber(value), nil
		default:
			return value, nil
		}
	}
	value, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, fmt.Errorf("custody JSON trailing data")
	}
	return value, nil
}

func canonicalCompletionNumber(number json.Number) json.Number {
	s := string(number)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	mantissa, exponent, found := strings.Cut(strings.ToLower(s), "e")
	e := new(big.Int)
	if found {
		e.SetString(exponent, 10)
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return json.Number("0")
	}
	trimmed := strings.TrimRight(digits, "0")
	e.Add(e, big.NewInt(int64(len(digits)-len(trimmed)-len(fraction))))
	return json.Number(sign + trimmed + "e" + e.String())
}
