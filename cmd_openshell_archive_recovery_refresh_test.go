package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
)

func TestArchiveRecoveryReservedRefreshRequiresOriginalReadback(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "project", ".noodle", "sessions", "original")
	captureDir := filepath.Join(root, "archives", "old")
	profile := loopruntime.OpenShellProfile{}
	profile.Gateway.Binding.StateRoot = filepath.Join(root, "custody")
	p := loopruntime.ProgressMaintenance{SessionDirectory: session, HostDir: filepath.Join(root, "host"), HostInstance: "original-host", ContextID: "original-context"}
	for _, dir := range []string{session, captureDir, profile.Gateway.Binding.StateRoot, p.HostDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	oldCustody := loopruntime.SessionReadbackFileRef{Path: filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json"), SHA256: strings.Repeat("a", 64)}
	currentCustody := loopruntime.SessionReadbackFileRef{Path: oldCustody.Path, SHA256: strings.Repeat("b", 64)}
	old := loopruntime.OpenShellWorkspaceCapture{Sources: map[string]loopruntime.SessionReadbackFileRef{"custody": oldCustody}}
	capture := loopruntime.SessionReadbackFileRef{Path: filepath.Join(captureDir, "workspace-capture.json"), SHA256: strings.Repeat("c", 64)}
	review := archiveRecoveryReview{Capture: capture, AttemptID: "original-attempt"}
	reviewRef := loopruntime.SessionReadbackFileRef{Path: filepath.Join(captureDir, "original-review.json"), SHA256: strings.Repeat("d", 64)}
	output, err := archiveRecoveryRefreshOutput(p, profile, capture)
	if err != nil || output != filepath.Join(root, "archives", "archive-recovery-refresh-"+capture.SHA256) {
		t.Fatalf("wrong private output: %s %v", output, err)
	}
	expected := archiveRecoveryPausePlan{Schema: 1, Review: reviewRef, Capture: capture, Host: p.HostInstance, ContextID: p.ContextID, AttemptID: review.AttemptID}
	intent := archiveRecoveryRefreshIntent{archiveRecoveryPausePlan: expected, Custody: currentCustody, Output: output}
	path := archiveRecoveryRefreshIntentPath(session)
	intentRef, err := reserveArchiveRecoveryRefresh(path, intent)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := refreshOriginalArchiveRecovery(t.Context(), p, profile, reviewRef, review, old, func() error {
			t.Fatal("an uncertain native capture was offered again")
			return nil
		})
		if err != nil || result.Intent == nil || *result.Intent != intentRef || result.Next != nil || len(result.Effects) != 0 || result.WaitFor != archiveRefreshWait {
			t.Fatalf("reserved refresh was not correctly held: %+v %v", result, err)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("reserved refresh created a new archive: %v", err)
		}
		observed, handled, err := inspectExistingArchiveRecoveryRefresh(t.Context(), archiveRecoveryPlanReadback{}, p, profile, expected)
		if err != nil || !handled || observed.Next != nil || observed.WaitFor != archiveRefreshWait {
			t.Fatalf("planner did not preserve uncertain capture: %+v %v", observed, err)
		}
	}
}

func TestArchiveRecoveryFreshCaptureMustBindOriginalAndCurrentEvidence(t *testing.T) {
	oldCustody := loopruntime.SessionReadbackFileRef{Path: "/original/owner.json", SHA256: strings.Repeat("a", 64)}
	currentCustody := loopruntime.SessionReadbackFileRef{Path: oldCustody.Path, SHA256: strings.Repeat("b", 64)}
	request := loopruntime.SessionReadbackFileRef{Path: "/original/request.json", SHA256: strings.Repeat("c", 64)}
	old := loopruntime.OpenShellWorkspaceCapture{OrderID: "order", SessionID: "session", NativeAttemptID: "attempt", SandboxID: "vm", MainProcessInstanceID: "instance", BaselineHead: "base", BaselineIndexSHA256: "index", CaptureExecRequestID: "prior-tar", Sources: map[string]loopruntime.SessionReadbackFileRef{"custody": oldCustody, "request": request}}
	fresh := old
	fresh.CaptureExecRequestID = "fresh-tar"
	fresh.Sources = map[string]loopruntime.SessionReadbackFileRef{"custody": currentCustody, "request": request}
	intent := archiveRecoveryRefreshIntent{archiveRecoveryPausePlan: archiveRecoveryPausePlan{Request: request}, Custody: currentCustody}
	if err := validateArchiveRecoveryFreshCapture(old, fresh, intent); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*loopruntime.OpenShellWorkspaceCapture)
	}{
		{"replacement VM", func(c *loopruntime.OpenShellWorkspaceCapture) { c.SandboxID = "replacement" }},
		{"replacement instance", func(c *loopruntime.OpenShellWorkspaceCapture) { c.MainProcessInstanceID = "replacement" }},
		{"different attempt", func(c *loopruntime.OpenShellWorkspaceCapture) { c.NativeAttemptID = "new-attempt" }},
		{"same tar receipt", func(c *loopruntime.OpenShellWorkspaceCapture) { c.CaptureExecRequestID = old.CaptureExecRequestID }},
		{"stale custody", func(c *loopruntime.OpenShellWorkspaceCapture) { c.Sources = old.Sources }},
		{"changed original request", func(c *loopruntime.OpenShellWorkspaceCapture) {
			c.Sources = map[string]loopruntime.SessionReadbackFileRef{"custody": currentCustody}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := fresh
			test.change(&changed)
			if err := validateArchiveRecoveryFreshCapture(old, changed, intent); err == nil {
				t.Fatal("unrelated evidence qualified for Pause")
			}
		})
	}
}
