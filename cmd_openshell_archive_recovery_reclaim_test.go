package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree/archive"
)

func TestArchiveRecoveryReclamationAdvertisesOnlyTheCurrentResident(t *testing.T) {
	h, req, _, requestPath := hostSessionReadbackFixture(t)
	var binding hostRequestBinding
	if _, err := loopruntime.ReadSessionReadbackFile(requestPath, &binding); err != nil {
		t.Fatal(err)
	}
	binding.CleanupCompleted = true
	binding.RequiredHostInstanceID = req.InstanceID
	h.receipts[0].CleanupCompleted = true
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(req.ProjectDir, ".noodle", "sessions", req.ReadbackSessionID)
	if err := os.MkdirAll(session, 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(req.ProjectDir, "retired-checkout")
	path := filepath.Join(session, archiveRecoveryCleanupDispositionFile)
	write := func(path string, value any) loopruntime.SessionReadbackFileRef {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	d := archiveRecoveryCleanupDisposition{
		archiveRecoveryAppliedIdentity: archiveRecoveryAppliedIdentity{OrderID: req.ReadbackOrderID, SessionID: req.ReadbackSessionID, AttemptID: "original-attempt"},
		Schema:                         1, Kind: "archive_recovery_cleanup_disposition", HostInstanceID: req.InstanceID, ContextID: req.ContextID,
		Request: write(requestPath, binding), OriginalReturn: "UNKNOWN",
		Checkout: archive.CheckoutIdentity{Path: worktree},
		Lease: admission.MachineSessionLease{ContextID: req.ContextID, Owner: hostMachineGenerationOwner(req.HostDir, req.InstanceID),
			Binding: &admission.MachineSessionBinding{ContextID: req.ContextID, OrderID: req.ReadbackOrderID, SessionID: req.ReadbackSessionID,
				AttemptID: "original-attempt", WorktreePath: worktree}},
	}
	// The checkout is already absent. A retained disposition needs no recreation.
	disposition := write(path, d)
	missing := archiveRecoveryCleanupPlan{Disposition: &disposition, Effects: []string{}}
	if err := planArchiveRecoveryReclaim(t.Context(), &missing); err != nil || missing.Next != nil ||
		missing.WaitFor != "original_archive_recovery_host_handoff" {
		t.Fatalf("missing resident yielded a mutation: %+v %v", missing, err)
	}
	listener, err := net.Listen("unix", filepath.Join(req.HostDir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	matching := archiveRecoveryCleanupPlan{Disposition: &disposition, Effects: []string{}}
	if err = planArchiveRecoveryReclaim(t.Context(), &matching); err != nil || matching.Next == nil || matching.WaitFor != "" ||
		!reflect.DeepEqual(matching.Next.Argv[3:], []string{archiveRecoveryReclaimOperation,
			"--disposition-file", path, "--expected-disposition-sha256", disposition.SHA256}) {
		t.Fatalf("exact current actor has no reclamation continuation: %+v %v", matching, err)
	}
	binding.BinarySHA256 = strings.Repeat("e", 64)
	d.Request = write(requestPath, binding)
	differentBinary := write(path, d)
	blocked := archiveRecoveryCleanupPlan{Disposition: &differentBinary, Effects: []string{}}
	if err = planArchiveRecoveryReclaim(t.Context(), &blocked); err != nil || blocked.Next != nil ||
		blocked.WaitFor != "original_archive_recovery_host_handoff" {
		t.Fatalf("different resident binary was allowed to reclaim: %+v %v", blocked, err)
	}
}
