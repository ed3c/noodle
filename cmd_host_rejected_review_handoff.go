package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type hostRejectedReviewHandoff struct {
	observer                 *hostSessionObserver
	Schema                   int                                           `json:"schema"`
	Owner                    string                                        `json:"owner"`
	Status                   string                                        `json:"status"`
	Error                    string                                        `json:"error,omitempty"`
	Request                  loopruntime.SessionReadbackFileRef            `json:"request"`
	RequestID                string                                        `json:"request_id"`
	HostInstanceID           string                                        `json:"host_instance_id"`
	ContextID                string                                        `json:"context_id"`
	OrderID                  string                                        `json:"order_id"`
	Attempts                 []hostRejectedReviewAttempt                   `json:"attempts"`
	Sources                  map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Retention                string                                        `json:"retention,omitempty"`
	Effects                  []string                                      `json:"effects"`
	AuthorizesDispatch       bool                                          `json:"authorizes_dispatch"`
	WorkerLeaseReadbackError string                                        `json:"worker_lease_readback_error,omitempty"`
	Next                     *hostRejectedReviewNext                       `json:"next"`
}

type hostRejectedReviewAttempt struct {
	StageIndex                     int                                  `json:"stage_index"`
	AttemptID                      string                               `json:"attempt_id"`
	SessionID                      string                               `json:"session_id"`
	CandidateHead                  string                               `json:"candidate_head"`
	NativeOpenShellStoppedReadback loopruntime.CompletedSessionReadback `json:"native_openshell_stopped_readback"`
	WorkerLeaseState               string                               `json:"worker_lease_state"`
	WorkerLease                    *admission.MachineSessionLease       `json:"worker_lease,omitempty"`
}

type hostRejectedReviewNext struct {
	Kind     string   `json:"kind"`
	Owner    string   `json:"owner"`
	Required []string `json:"required"`
}

func newHostRejectedReviewHandoffCmd(app *App) *cobra.Command {
	var request, order, mode, expectedPlan string
	cmd := &cobra.Command{
		Use: "rejected-review-handoff", Short: "Observe the original failed review attempts for cross-version handoff", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if mode != "readback" {
				result, err := runRejectedReviewRoundRelease(cmd.Context(), app.projectDir, request, order, mode, expectedPlan)
				if err != nil {
					result.Error = err.Error()
				}
				return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(result))
			}
			if expectedPlan != "" {
				return errors.New("expected-plan requires an exact rejected review release continuation")
			}
			report, err := readHostRejectedReviewHandoff(cmd.Context(), app.projectDir, request, order)
			if err != nil {
				report.Error = err.Error()
			}
			return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(report))
		},
	}
	cmd.Flags().StringVar(&request, "request-file", "", "Original accepted native admission request")
	cmd.Flags().StringVar(&order, "order-id", "", "Original canonical failed order")
	cmd.Flags().StringVar(&mode, "mode", "readback", "Native handoff boundary: readback, plan, release, retire, finish, reconcile or rollback")
	cmd.Flags().StringVar(&expectedPlan, "expected-plan", "", "Original rejected review release plan digest")
	_ = cmd.MarkFlagRequired("request-file")
	_ = cmd.MarkFlagRequired("order-id")
	return cmd
}

func readHostRejectedReviewHandoff(ctx context.Context, project, requestFile, order string) (hostRejectedReviewHandoff, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	result := hostRejectedReviewHandoff{Schema: 1, Owner: "Noodle", Status: "refused",
		OrderID: order, Effects: []string{}, Attempts: []hostRejectedReviewAttempt{}}
	owner, err := readHostOwnerBinding(requestFile)
	if err != nil {
		return result, err
	}
	var request hostRequestBinding
	requestRef, err := loopruntime.ReadSessionReadbackFile(requestFile, &request)
	if err != nil || requestRef != owner.Request || request.Status != "accepted" {
		return result, fmt.Errorf("original accepted Host request differs: %v", err)
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil || project != owner.Current.ProjectDir {
		return result, fmt.Errorf("original failed review project differs: %v", err)
	}
	result.Request, result.RequestID = owner.Request, owner.Current.RequestID
	result.HostInstanceID, result.ContextID = owner.Current.HostInstanceID, owner.Current.ContextID
	exe, err := os.Executable()
	if err != nil {
		return result, err
	}
	binary, err := hostReadbackBinaryRef(exe)
	if err != nil {
		return result, err
	}
	history, err := loop.ReadRejectedReviewHistory(filepath.Join(project, ".noodle"), order)
	if err != nil {
		return result, err
	}
	attempts := make([]hostRejectedReviewAttempt, 0, len(history.Attempts))
	bindings := make([]admission.MachineSessionBinding, 0, len(history.Attempts))
	observed, err := observeBoundHostSessions(ctx, project, requestFile, order, owner, binary, readSessionObserverFrame,
		func(ctx context.Context, project string, profile loopruntime.OpenShellProfile) (func() error, error) {
			nativeIDs := map[string]bool{}
			for i, attempt := range history.Attempts {
				read := loop.ReadOpenShellSessionEvidence
				latest := i == len(history.Attempts)-1
				if !latest {
					read = loop.ReadRetainedOpenShellSessionEvidence
				}
				proof, err := read(ctx, project, filepath.Join(project, ".noodle"), result.ContextID, profile, order, attempt.SessionID)
				if err != nil {
					return nil, fmt.Errorf("original rejected session %s: %w", attempt.SessionID, err)
				}
				execution := proof.Execution
				if proof.AttemptID != attempt.AttemptID || proof.StageIndex != attempt.StageIndex || proof.SessionID != attempt.SessionID ||
					execution.Sources["canonical_snapshot"] != history.Sources["canonical_snapshot"] {
					return nil, fmt.Errorf("original rejected session %s proof binding differs", attempt.SessionID)
				}
				if nativeIDs[execution.NativeAttemptID] || execution.NativeAttemptID == "" || execution.NativePhase != "Stopped" ||
					!execution.StopAcknowledged || execution.NativeExecExitCode != 0 || execution.CandidateHead == "" ||
					execution.CustodyProjection == nil || latest && execution.CandidateHead != history.Head {
					return nil, fmt.Errorf("original rejected session %s stopped execution differs", attempt.SessionID)
				}
				nativeIDs[execution.NativeAttemptID] = true
				path, err := filepath.EvalSymlinks(proof.WorktreePath)
				if err != nil {
					return nil, err
				}
				bindings = append(bindings, admission.MachineSessionBinding{ContextID: result.ContextID, ProjectDir: project, OrderID: order,
					StageIndex: attempt.StageIndex, RetryCount: i, AttemptID: attempt.AttemptID, SessionID: attempt.SessionID, Runtime: "openshell", WorktreePath: path})
				attempts = append(attempts, hostRejectedReviewAttempt{StageIndex: attempt.StageIndex, AttemptID: attempt.AttemptID,
					SessionID: attempt.SessionID, CandidateHead: execution.CandidateHead,
					NativeOpenShellStoppedReadback: execution, WorkerLeaseState: "UNKNOWN"})
			}
			return func() error {
				for _, attempt := range attempts {
					if err := loopruntime.VerifyCompletedSessionReadback(attempt.NativeOpenShellStoppedReadback, profile, result.ContextID, order, attempt.SessionID); err != nil {
						return err
					}
				}
				return nil
			}, nil
		})
	if err != nil {
		return result, err
	}
	sources := maps.Clone(observed.Sources)
	maps.Copy(sources, history.Sources)
	if err = loopruntime.VerifySessionReadbackFiles(sources); err != nil {
		return result, err
	}
	if err = observeRejectedReviewMachineLeases(owner, bindings, attempts, sources); err != nil {
		result.WorkerLeaseReadbackError = err.Error()
	}
	result.Status, result.Attempts, result.Sources = "observed", attempts, sources
	result.observer = observed.Observer
	result.Retention = reducer.TaskDataRetention
	result.Next = &hostRejectedReviewNext{"input", "Noodle", []string{"original_native_host_transfer_readback"}}
	return result, nil
}

func observeRejectedReviewMachineLeases(owner *hostOwnerBinding, bindings []admission.MachineSessionBinding,
	attempts []hostRejectedReviewAttempt, sources map[string]loopruntime.SessionReadbackFileRef) error {
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return err
	}
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(directory, "capacity.json"), nil)
	if err != nil {
		return err
	}
	rows, err := admission.ReadMachineSessionLeases(directory, hostMachineGenerationOwner(owner.HostRoot, owner.Current.HostInstanceID),
		owner.Current.ContextID, owner.Current.ProjectDir)
	if err != nil {
		return err
	}
	matched := make(map[admission.MachineSessionBinding]admission.MachineSessionLease, len(rows))
	for _, row := range rows {
		if row.Binding == nil {
			return fmt.Errorf("original machine lease %s has no session binding", row.ID)
		}
		if _, exists := matched[*row.Binding]; exists {
			return fmt.Errorf("original machine lease for session %s is ambiguous", row.Binding.SessionID)
		}
		matched[*row.Binding] = row
	}
	if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"machine_capacity": ref}); err != nil {
		return err
	}
	sources["machine_capacity"] = ref
	for i, binding := range bindings {
		if row, ok := matched[binding]; ok {
			attempts[i].WorkerLease, attempts[i].WorkerLeaseState = &row, row.State
		}
	}
	return nil
}
