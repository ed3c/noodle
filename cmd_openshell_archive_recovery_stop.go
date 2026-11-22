package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type archiveRecoveryReview struct {
	Schema            int                                     `json:"schema"`
	Status            string                                  `json:"status"`
	OriginalReturn    string                                  `json:"original_return"`
	AttemptID         string                                  `json:"attempt_id"`
	Capture           loopruntime.SessionReadbackFileRef      `json:"capture"`
	Tree              workspaceartifacts.ArchiveCandidateTree `json:"tree"`
	WritePaths        []string                                `json:"write_paths"`
	AuthorizesLanding bool                                    `json:"authorizes_landing"`
}

func newStopOpenShellArchiveRecoveryCmd() *cobra.Command {
	var review, expected string
	cmd := &cobra.Command{Use: "stop-workspace-capture", Short: "Stop the original OpenShell sandbox after the reviewed archive and paused Owner are verified", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		ref, err := runOriginalArchiveRecoveryControl(ctx, loopruntime.SessionReadbackFileRef{Path: review, SHA256: expected}, archiveRecoveryControl{})
		if err != nil {
			return err
		}
		reviewRef := loopruntime.SessionReadbackFileRef{Path: review, SHA256: expected}
		continuation, err := archiveRecoveryOfferAfter(filepath.Dir(filepath.Dir(ref.Path)), reviewRef, "apply", archiveApplyOperation)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Schema            int                                `json:"schema"`
			Status            string                             `json:"status"`
			Receipt           loopruntime.SessionReadbackFileRef `json:"receipt"`
			Review            loopruntime.SessionReadbackFileRef `json:"review"`
			OriginalReturn    string                             `json:"original_return"`
			AuthorizesLanding bool                               `json:"authorizes_landing"`
			archiveRecoveryContinuation
		}{1, "archive_recovery_stop_observed", ref, reviewRef, "UNKNOWN", false, continuation})
	}}
	cmd.Flags().StringVar(&review, "review-file", "", "Original archive recovery review evidence")
	cmd.Flags().StringVar(&expected, "expected-sha256", "", "Exact reviewed FileRef SHA256")
	_ = cmd.MarkFlagRequired("review-file")
	_ = cmd.MarkFlagRequired("expected-sha256")
	return cmd
}

type archiveRecoveryControl struct {
	pause           *archiveRecoveryPauseReadback
	adopt           bool
	plan            *archiveRecoveryPlanReadback
	refresh         *archiveRecoveryRefreshReadback
	cleanup         *archiveRecoveryCleanupPlan
	cleanupExpected string
}

func runOriginalArchiveRecoveryControl(ctx context.Context, reviewRef loopruntime.SessionReadbackFileRef, action archiveRecoveryControl) (loopruntime.SessionReadbackFileRef, error) {
	var empty loopruntime.SessionReadbackFileRef
	var review archiveRecoveryReview
	ref, err := loopruntime.ReadSessionReadbackFile(reviewRef.Path, &review)
	if err != nil {
		return empty, err
	}
	if ref != reviewRef || review.Schema != 1 || review.Status != "archive_recovery_review_evidence" || review.OriginalReturn != "UNKNOWN" || review.AttemptID == "" || review.Tree.CandidateTree == "" || review.AuthorizesLanding {
		return empty, fmt.Errorf("archive recovery original reviewed evidence differs")
	}
	capture, err := loopruntime.ReadOpenShellWorkspaceCapture(review.Capture)
	if err != nil {
		return empty, err
	}
	request := capture.Sources["request"]
	var binding hostRequestBinding
	actual, err := loopruntime.ReadSessionReadbackFile(request.Path, &binding)
	if err != nil {
		return empty, err
	}
	if actual != request || binding.Schema != 1 || binding.Status != "accepted" || binding.Operation != "attach" || filepath.Base(capture.SessionID) != capture.SessionID || capture.SessionID == "." || capture.SessionID == ".." {
		return empty, fmt.Errorf("archive recovery original accepted request differs")
	}
	owner, err := readHostOwnerBinding(request.Path)
	if err != nil {
		return empty, err
	}
	if owner.Request != request || owner.Current.HostInstanceID != binding.RequiredHostInstanceID || owner.Current != owner.Original || owner.Current.ContextState != "active" {
		return empty, fmt.Errorf("archive recovery original Host has no exclusive active context")
	}
	data, err := readWorkspaceCaptureConfig(binding)
	if err != nil {
		return empty, err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return empty, err
	}
	if len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.AdmittedOrders[0] != capture.OrderID {
		return empty, fmt.Errorf("archive recovery original Host has more than the one admitted task")
	}
	profile, err := loadSessionObserverProfile(cfg.Runtime.OpenShell.Profile, capture.Sources["profile"])
	if err != nil {
		return empty, err
	}
	session, err := filepath.EvalSymlinks(filepath.Join(binding.ProjectDir, ".noodle", "sessions", capture.SessionID))
	if err != nil {
		return empty, err
	}
	p := loopruntime.ProgressMaintenance{Request: request, Profile: capture.Sources["profile"], HostDir: owner.HostRoot, HostInstance: owner.Current.HostInstanceID, HostPID: owner.Current.HostPID, ContextID: owner.Current.ContextID, SessionDirectory: session}
	runtimeDir := filepath.Dir(filepath.Dir(session))
	if action.plan == nil && (action.cleanup == nil || action.cleanupExpected != "") {
		lock, err := lockfile.TryLock(filepath.Join(runtimeDir, "control.lock"))
		if err != nil {
			return empty, err
		}
		defer lock.Close()
	}
	var hold loopruntime.ArchiveRecoveryDispatchHold
	if action.pause == nil && action.plan == nil && action.refresh == nil && action.cleanup == nil {
		hold, err = archiveRecoveryPaused(runtimeDir)
		if err != nil {
			return empty, err
		}
	}
	if !action.adopt && action.plan == nil && action.refresh == nil && action.cleanup == nil {
		if err = verifyArchiveRecoveryReview(ctx, review); err != nil {
			return empty, err
		}
	}
	if err = maintenanceOriginalAttempt(p); err != nil {
		return empty, err
	}
	var priorFrame *hostSessionObserverFrame
	verifyOwner := func() error {
		if err := loopruntime.VerifySessionReadbackFiles(owner.Sources); err != nil {
			return err
		}
		frame, err := readSessionObserverFrame(ctx, owner, request.Path, profile.Gateway)
		if err != nil {
			return err
		}
		if priorFrame != nil {
			if err := verifySessionObserverFrames(*priorFrame, frame); err != nil {
				return err
			}
		} else {
			priorFrame = &frame
		}
		return verifyArchiveRecoveryLeases(owner, frame, profile.Gateway, capture.OrderID)
	}
	if action.cleanup != nil {
		return empty, inspectArchiveRecoveryCleanup(ctx, action.cleanup, p, owner, binding, cfg, profile, reviewRef, review, capture, action.cleanupExpected, verifyOwner)
	}
	if action.plan != nil {
		*action.plan, err = inspectArchiveRecoveryPlan(ctx, p, profile, reviewRef, review, capture, verifyOwner)
		return empty, err
	}
	if action.refresh != nil {
		*action.refresh, err = refreshOriginalArchiveRecovery(ctx, p, profile, reviewRef, review, capture, verifyOwner)
		return empty, err
	}
	if action.pause != nil {
		if err = verifyOwner(); err != nil {
			return empty, err
		}
		*action.pause, err = prepareArchiveRecoveryPauseLocked(p, reviewRef, review.Capture, review.AttemptID)
		return action.pause.Plan, err
	}
	checkOwner := func() error {
		latest, err := archiveRecoveryPaused(runtimeDir)
		if err != nil {
			return err
		}
		if latest != hold {
			return fmt.Errorf("archive recovery original native pause changed")
		}
		return verifyOwner()
	}
	if action.adopt {
		subject, err := readArchiveRecoveryScopeBinding(ctx, binding.ProjectDir, cfg, capture)
		if err != nil {
			return empty, err
		}
		if subject.Attempt.AttemptID != review.AttemptID {
			return empty, fmt.Errorf("archive recovery original canonical attempt differs")
		}
		owner.Sources["canonical_snapshot"], owner.Sources["integration_binding"] = subject.Snapshot, subject.Integration
		stop, err := loopruntime.ReadSessionReadbackFile(filepath.Join(session, "archive-recovery-stop", "archive-recovery-stop.json"), nil)
		if err != nil {
			return empty, fmt.Errorf("archive recovery requires the original Stop receipt before Apply: %w", err)
		}
		input := loopruntime.OpenShellArchiveRecoveryApplyInput{Review: reviewRef, Capture: review.Capture, Stop: stop, DispatchHold: hold, AttemptID: subject.Attempt.AttemptID, Tree: review.Tree, Scope: subject.Scope, Target: subject.Custody.Physical}
		return loopruntime.ApplyOpenShellArchiveRecovery(ctx, p, profile, input, checkOwner)
	}
	return loopruntime.StopOpenShellArchiveRecovery(ctx, p, profile, ref, review.Capture, hold, filepath.Join(session, "archive-recovery-stop"), checkOwner)
}

func verifyArchiveRecoveryReview(ctx context.Context, review archiveRecoveryReview) error {
	fresh, err := inspectOriginalWorkspaceCapture(ctx, review.Capture)
	if err != nil {
		return err
	}
	if fresh["attempt_id"] != review.AttemptID || !reflect.DeepEqual(fresh["tree"], review.Tree) {
		return fmt.Errorf("archive recovery reviewed original attempt or candidate tree changed")
	}
	return nil
}

func archiveRecoveryPaused(runtimeDir string) (loopruntime.ArchiveRecoveryDispatchHold, error) {
	var hold loopruntime.ArchiveRecoveryDispatchHold
	var status struct {
		LoopState string `json:"loop_state"`
	}
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(runtimeDir, "status.json"), &status); err != nil {
		return hold, err
	}
	if status.LoopState != "paused" {
		return hold, fmt.Errorf("archive recovery requires the original paused Loop before Stop")
	}
	ackPath := filepath.Join(runtimeDir, "control-ack.ndjson")
	ref, err := loopruntime.ReadSessionReadbackFile(ackPath, nil)
	if err != nil {
		return hold, fmt.Errorf("archive recovery original native pause ACK unavailable: %w", err)
	}
	raw, err := os.ReadFile(ref.Path)
	if err != nil || hostDigest(raw) != ref.SHA256 {
		return hold, fmt.Errorf("archive recovery native control ACK source changed")
	}
	pauseID, err := archiveRecoveryPauseACK(raw)
	if err != nil {
		return hold, err
	}
	for _, name := range []string{"control.ndjson", "orders-next.json"} {
		data, err := os.ReadFile(filepath.Join(runtimeDir, name))
		if err != nil && !os.IsNotExist(err) {
			return hold, err
		}
		if len(data) != 0 {
			return hold, fmt.Errorf("archive recovery original Loop has pending %s", name)
		}
	}
	return loopruntime.ArchiveRecoveryDispatchHold{Acknowledgements: ref, PauseCommandID: pauseID}, nil
}

func archiveRecoveryPauseACK(raw []byte) (string, error) {
	seen := map[string]string{}
	pauseID := ""
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ack loop.ControlAck
		if err := json.Unmarshal(line, &ack); err != nil || ack.ID == "" || ack.At.IsZero() {
			return "", fmt.Errorf("archive recovery native control ACK history is invalid")
		}
		action := strings.ToLower(strings.TrimSpace(ack.Action))
		if previous, exists := seen[ack.ID]; exists {
			if previous != action {
				return "", fmt.Errorf("archive recovery replayed control ID changed action")
			}
			continue
		}
		seen[ack.ID] = action
		if ack.DispatchState == loop.StateRunning || ack.DispatchState == loop.StateDraining || (ack.Status == "ok" && (action == "resume" || action == "drain" || action == "pause")) {
			pauseID = ""
		}
		if action == "pause" && ack.Status == "ok" && ack.DispatchState == loop.StatePaused {
			pauseID = ack.ID
		}
	}
	if pauseID == "" {
		return "", fmt.Errorf("archive recovery requires a current native pause ACK with no later resume")
	}
	return pauseID, nil
}

func verifyArchiveRecoveryLeases(owner *hostOwnerBinding, frame hostSessionObserverFrame, spec loopruntime.OpenShellGatewaySpec, order string) error {
	var quota sessionObserverQuota
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(owner.HostRoot, "quota", "quota.json"), &quota); err != nil {
		return err
	}
	if err := validateSessionObserverQuota(owner, quota, frame, spec); err != nil {
		return err
	}
	if len(quota.Leases) != 2 || len(frame.Resident.Contexts) != 1 || frame.Resident.Contexts[0] != owner.Current.ContextID {
		return fmt.Errorf("archive recovery original worker and resident are not exclusive")
	}
	for _, row := range quota.Leases {
		if row.Owner != owner.Current.HostInstanceID || row.Context != owner.Current.ContextID {
			return fmt.Errorf("archive recovery quota has a foreign original context")
		}
	}
	dir, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return err
	}
	return verifyArchiveRecoveryMachineLeases(filepath.Join(dir, "capacity.json"), owner, order)
}

func verifyArchiveRecoveryMachineLeases(path string, owner *hostOwnerBinding, order string) error {
	var machine struct {
		Policy string `json:"policy"`
		Leases map[string]struct {
			Owner           string          `json:"owner"`
			OrderID         string          `json:"order_id"`
			ContextID       string          `json:"context_id"`
			EffectsPossible bool            `json:"effects_possible"`
			Settlement      json.RawMessage `json:"settlement"`
		} `json:"leases"`
	}
	if _, err := loopruntime.ReadSessionReadbackFile(path, &machine); err != nil {
		return err
	}
	workers, residents := 0, 0
	workerOwner := hostMachineGenerationOwner(owner.HostRoot, owner.Current.HostInstanceID)
	for _, lease := range machine.Leases {
		switch lease.Owner {
		case workerOwner:
			if lease.OrderID != order || lease.ContextID != owner.Current.ContextID || !lease.EffectsPossible || (len(lease.Settlement) != 0 && string(lease.Settlement) != "null") {
				return fmt.Errorf("archive recovery original machine worker lease differs")
			}
			workers++
		case owner.HostRoot + "/resident":
			if !lease.EffectsPossible || (len(lease.Settlement) != 0 && string(lease.Settlement) != "null") {
				return fmt.Errorf("archive recovery original machine resident lease differs")
			}
			residents++
		}
	}
	if machine.Policy != admission.MachineAdmissionPolicy || workers != 1 || residents != 1 {
		return fmt.Errorf("archive recovery original machine leases are incomplete")
	}
	return nil
}
