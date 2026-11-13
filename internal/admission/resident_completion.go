package admission

import (
	"errors"
	"slices"
	"strings"
)

// VerifyResidentNormalExit interprets two snapshots from the same original
// parent. The caller must bind that parent's kernel generation, RPC identity
// and successful snapshot persistence readback.
// This proves only its guarded child's normal zero exit, not application drain,
// empty compute, custody release or permission to control a process.
func VerifyResidentNormalExit(before, after Resident) error {
	if before.State != "running" || before.ID == "" || before.Name == "" || before.PID <= 1 || before.ProcessGroup != before.PID || before.Started == "" || before.Fingerprint == "" || before.GuardID == "" || before.Sharing != "shared" || len(before.Contexts) == 0 {
		return errors.New("normal exit lacks the original running guarded resident")
	}
	if after.ID != before.ID || after.Name != before.Name || after.PID != before.PID || after.ProcessGroup != before.ProcessGroup || after.Started != before.Started || after.Fingerprint != before.Fingerprint || after.GuardID != before.GuardID || after.Sharing != before.Sharing {
		return errors.New("normal exit resident generation or deployment differs")
	}
	if before.ForceRequested != "" || after.ForceRequested != "" || before.ForceAfterGrace != after.ForceAfterGrace {
		return errors.New("normal exit has force intent or changed force policy")
	}
	// A successful waiter preserves earlier guard refusals. Those messages are
	// disjoint from wait/reconcile failures; arbitrary prior errors cannot be
	// accepted merely because an identical terminal error was already present.
	guardRefusal := func(message string) bool {
		return message == "" || strings.HasPrefix(message, "resident release guard refused: ")
	}
	if !guardRefusal(before.Error) || (before.Error != "" && !slices.Contains(before.Errors, before.Error)) {
		return errors.New("normal exit baseline diagnostics do not identify an original guard refusal")
	}
	for _, message := range before.Errors {
		if message == "" || !guardRefusal(message) {
			return errors.New("normal exit baseline includes an unqualified prior failure")
		}
	}
	if after.State != "exited" || after.Error != before.Error || !slices.Equal(after.Errors, before.Errors) {
		return errors.New("original waiter normal zero exit is not observed; preserve all diagnostics")
	}
	if len(after.Contexts) != 0 || len(after.Released) != len(before.Released)+len(before.Contexts) || !slices.Equal(after.Released[:len(before.Released)], before.Released) {
		return errors.New("normal exit did not preserve and release the original context leases")
	}
	// Snapshot sorts Contexts; the waiter appends their internal acquisition
	// order. Preserve the Released prefix and compare the new unique leases.
	contexts := slices.Clone(before.Contexts)
	released := slices.Clone(after.Released[len(before.Released):])
	slices.Sort(contexts)
	slices.Sort(released)
	for i, id := range contexts {
		if id == "" || (i > 0 && id == contexts[i-1]) {
			return errors.New("normal exit baseline context leases are invalid")
		}
	}
	if !slices.Equal(contexts, released) {
		return errors.New("normal exit released a different context lease set")
	}
	return nil
}
