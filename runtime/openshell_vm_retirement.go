package runtime

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"reflect"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

const CleanupSessionRetired = "retired"

type CleanupSessionRetirement struct {
	OutcomeStatus         SessionStatus `json:"outcome_status"`
	SessionID             string        `json:"session_id"`
	NativeAttemptID       string        `json:"native_attempt_id"`
	SandboxID             string        `json:"sandbox_id"`
	State                 string        `json:"state"`
	NativeDeletionOutcome string        `json:"native_deletion_outcome"`
}

type openShellVMRetirement struct {
	ExpectedResourceVersion uint64
	RequestID               string
	Acknowledged            bool
	Outcome                 int32
	Failure                 string // Historical failure is retained after effect readback.
	Retired                 bool
	Storage                 openShellVMStorage
	Sources                 map[string]SessionReadbackFileRef
}

func validateVMRetirement(record OpenShellCustodyRecord) error {
	retirement := record.Retirement
	if retirement == nil {
		return nil
	}
	if err := validateProgressRecord(record, record.Worker.Identity); err != nil {
		return err
	}
	if retirement.ExpectedResourceVersion == 0 || retirement.RequestID == "" || retirement.Storage.Root == "" || retirement.Storage.Config.Path != record.Worker.Identity.ConfigSource || retirement.Storage.Config.SHA256 == "" || len(retirement.Sources) == 0 {
		return fmt.Errorf("original VM retirement intent is incomplete")
	}
	if _, exists := retirement.Sources["native_custody"]; exists {
		return fmt.Errorf("VM retirement cannot bind its own mutable custody source")
	}
	if retirement.Acknowledged && retirement.Outcome != int32(pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED) && retirement.Outcome != int32(pb.DeletionOutcome_DELETION_OUTCOME_ACCEPTED) {
		return fmt.Errorf("original VM deletion acknowledgement is invalid")
	}
	return nil
}

func retirementResult(record OpenShellCustodyRecord) CleanupSessionRetirement {
	result := CleanupSessionRetirement{SessionID: record.Task.SessionID, NativeAttemptID: record.Task.AttemptID, SandboxID: record.Worker.Identity.Sandbox.SandboxID, State: "pending", NativeDeletionOutcome: "unknown"}
	if record.Retirement.Acknowledged {
		result.NativeDeletionOutcome = pb.DeletionOutcome(record.Retirement.Outcome).String()
	}
	if record.Retirement.Retired {
		result.State = CleanupSessionRetired
	}
	return result
}

// The original cleanup actor has already accepted canonical completion and
// verified restoration. This method rechecks the original runtime/evidence; it
// accepts no independently serialized Main authority or caller-selected VM.
func (r *OpenShellRuntime) RetireCleanupSession(ctx context.Context, b CompletedSessionBinding, repository string, expected SessionStatus) (CleanupSessionRetirement, error) {
	readback, err := r.ReadCleanupSession(ctx, b, repository, expected)
	if err != nil {
		return CleanupSessionRetirement{}, err
	}
	if !r.mu.TryLock() {
		return CleanupSessionRetirement{}, fmt.Errorf("original runtime operation in progress")
	}
	defer r.mu.Unlock()
	if r.active[b.SessionID] != nil {
		return CleanupSessionRetirement{}, fmt.Errorf("original cleanup session became active")
	}
	storage, err := openShellVMStorageForProfile(r.config.Profile)
	if err != nil {
		return CleanupSessionRetirement{}, err
	}
	gateway := r.config.Gateway
	if !gateway.mu.TryLock() {
		return CleanupSessionRetirement{}, fmt.Errorf("original gateway operation in progress")
	}
	defer gateway.mu.Unlock()
	custody, _, err := gateway.completionOwner(r.config.ContextID)
	if err != nil {
		return CleanupSessionRetirement{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := custody.retireCleanupSession(ctx, OpenShellTask{b.OrderID, readback.Execution.NativeAttemptID, b.SessionID}, storage, readback.Execution.Sources)
	result.OutcomeStatus = expected
	return result, err
}

func (c *OpenShellCustody) retireCleanupSession(ctx context.Context, task OpenShellTask, storage openShellVMStorage, sources map[string]SessionReadbackFileRef) (CleanupSessionRetirement, error) {
	if !c.mu.TryLock() {
		return CleanupSessionRetirement{}, fmt.Errorf("original custody operation in progress")
	}
	defer c.mu.Unlock()
	if err := c.mutable(); err != nil {
		return CleanupSessionRetirement{}, err
	}
	var record *OpenShellCustodyRecord
	for i := range c.state.Records {
		if c.state.Records[i].Task == task {
			if record != nil {
				return CleanupSessionRetirement{}, fmt.Errorf("original cleanup task is ambiguous")
			}
			record = &c.state.Records[i]
		}
	}
	if record == nil {
		return CleanupSessionRetirement{}, fmt.Errorf("original cleanup task is missing")
	}
	if err := validateProgressRecord(*record, record.Worker.Identity); err != nil {
		return CleanupSessionRetirement{}, err
	}
	if err := VerifySessionReadbackFiles(sources); err != nil {
		return CleanupSessionRetirement{}, err
	}
	if record.Retirement == nil {
		// Recheck the same stopped identity immediately before storing the
		// one-shot intent. The native conditional RPC closes the name race.
		response, err := c.client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: c.scope()})
		if err != nil {
			return CleanupSessionRetirement{}, err
		}
		if _, err := settledNativeObservation(*record, record.Worker.Identity, response.GetSandbox(), savedObservation); err != nil {
			return CleanupSessionRetirement{}, err
		}
		expectedVersion := response.GetSandbox().GetMetadata().GetResourceVersion()
		if expectedVersion == 0 {
			return CleanupSessionRetirement{}, fmt.Errorf("original Stopped VM resource version missing")
		}
		if _, err := storage.sandboxPresent(record.Worker.Identity.Sandbox.SandboxID); err != nil {
			return CleanupSessionRetirement{}, err
		}
		requestID, err := openShellUUID()
		if err != nil {
			return CleanupSessionRetirement{}, err
		}
		retained := maps.Clone(sources)
		delete(retained, "native_custody")
		switch c.state.Version {
		case openShellCustodyVersion:
			c.state.Version = openShellRetirementCustodyVersion
		case openShellCredentialCustodyVersion:
			c.state.Version = openShellCredentialRetirementCustodyVersion
		case openShellRetirementCustodyVersion, openShellCredentialRetirementCustodyVersion:
		default:
			return CleanupSessionRetirement{}, fmt.Errorf("original custody schema cannot retain VM retirement")
		}
		record.Retirement = &openShellVMRetirement{ExpectedResourceVersion: expectedVersion, RequestID: requestID, Storage: storage, Sources: retained}
		if err := c.save(); err != nil {
			return retirementResult(*record), err
		}
		responseDelete, deleteErr := c.client.DeleteStoppedSandboxIfMatches(ctx, &pb.DeleteStoppedSandboxIfMatchesRequest{WorkspaceScope: c.scope(), Name: record.Name, ExpectedSandboxId: record.Worker.Identity.Sandbox.SandboxID, ExpectedResourceVersion: expectedVersion, RequestId: requestID})
		if deleteErr != nil {
			record.Retirement.Failure = deleteErr.Error()
		} else if responseDelete.GetSandboxId() != record.Worker.Identity.Sandbox.SandboxID || (responseDelete.GetOutcome() != pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED && responseDelete.GetOutcome() != pb.DeletionOutcome_DELETION_OUTCOME_ACCEPTED) {
			record.Retirement.Failure = "native deletion response does not identify the original VM outcome"
		} else {
			record.Retirement.Acknowledged = true
			record.Retirement.Outcome = int32(responseDelete.GetOutcome())
		}
		if err := c.save(); err != nil {
			return retirementResult(*record), err
		}
	} else if !reflect.DeepEqual(record.Retirement.Storage, storage) {
		return retirementResult(*record), fmt.Errorf("original VM storage binding changed")
	}
	// An existing intent is never resent, including a crash before sending and
	// a lost ACK. Only the original native effect and disk are read back.
	present, disk, err := c.readVMRetirement(ctx, *record)
	if err != nil {
		return retirementResult(*record), err
	}
	if !present && !disk && !record.Retirement.Retired {
		record.Retirement.Retired = true
		if err := c.save(); err != nil {
			return retirementResult(*record), err
		}
	}
	result := retirementResult(*record)
	if result.State != CleanupSessionRetired {
		return result, fmt.Errorf("original VM retirement is pending; native_present=%t disk_present=%t; prior_failure=%s", present, disk, record.Retirement.Failure)
	}
	return result, nil
}

func (c *OpenShellCustody) readVMRetirement(ctx context.Context, record OpenShellCustodyRecord) (nativePresent, diskPresent bool, err error) {
	if err := validateVMRetirement(record); err != nil {
		return false, false, err
	}
	if err := VerifySessionReadbackFiles(record.Retirement.Sources); err != nil {
		return false, false, err
	}
	token := ""
	seen := map[string]bool{}
	for {
		response, err := c.client.ListSandboxes(ctx, &pb.ListSandboxesRequest{WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_AllWorkspaces{AllWorkspaces: &dm.AllWorkspaces{}}}, PageSize: 100, PageToken: token})
		if err != nil {
			return false, false, err
		}
		for _, native := range response.GetSandboxes() {
			meta := native.GetMetadata()
			if meta.GetId() != record.Worker.Identity.Sandbox.SandboxID && meta.GetName() != record.Name {
				continue
			}
			if !sameOpenShellSandbox(native, record.Name, record.Worker.Identity.Sandbox.SandboxID, c.state.Binding.Workspace) {
				return false, false, fmt.Errorf("VM retirement encountered a foreign identity or name replacement")
			}
			if err := retiringNativeMatches(record, native); err != nil {
				return false, false, err
			}
			if nativePresent {
				return false, false, fmt.Errorf("VM retirement inventory contains duplicate original identity")
			}
			nativePresent = true
		}
		token = response.GetNextPageToken()
		if token == "" {
			break
		}
		if seen[token] {
			return false, false, fmt.Errorf("VM retirement inventory pagination repeated")
		}
		seen[token] = true
	}
	diskPresent, err = record.Retirement.Storage.sandboxPresent(record.Worker.Identity.Sandbox.SandboxID)
	return nativePresent, diskPresent, err
}

func retiringNativeMatches(record OpenShellCustodyRecord, native *pb.Sandbox) error {
	identity := record.Worker.Identity
	if !sameOpenShellSandbox(native, record.Name, identity.Sandbox.SandboxID, identity.Sandbox.Workspace) {
		return fmt.Errorf("original retiring VM identity differs")
	}
	spec, err := openShellBytes(native.GetSpec())
	if err != nil || native.GetSpec() == nil || !bytes.Equal(spec, record.Spec) {
		return fmt.Errorf("original retiring VM specification differs")
	}
	status := native.GetStatus()
	if status.GetMainProcessInstanceId() != record.MainProcessInstanceID || (status.GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED && status.GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_DELETING) {
		return fmt.Errorf("original retiring VM generation or terminal phase differs")
	}
	if record.Stopped.ComputeInstanceID != "" && status.GetAgentPod() != "" && record.Stopped.ComputeInstanceID != status.GetAgentPod() {
		return fmt.Errorf("original retiring VM compute identity differs")
	}
	return nil
}

func (c *OpenShellCustody) observeRetiring(ctx context.Context, record OpenShellCustodyRecord) (OpenShellCompletedObservation, error) {
	present, disk, err := c.readVMRetirement(ctx, record)
	if err != nil {
		return OpenShellCompletedObservation{}, err
	}
	if record.Retirement.Retired && (present || disk) {
		return OpenShellCompletedObservation{}, fmt.Errorf("original retired VM resource reappeared")
	}
	observed, err := savedSettledObservation(record)
	if err != nil {
		return OpenShellCompletedObservation{}, err
	}
	result := retirementResult(record)
	observed.Phase = "Retiring"
	if result.State == CleanupSessionRetired {
		observed.Phase = "Retired"
	}
	observed.Retirement = &result
	return observed, nil
}
