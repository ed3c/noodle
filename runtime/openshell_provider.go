package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/filex"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const openShellProviderWait = 30 * time.Second

// OpenShellProviderObservation contains only public readiness identities and
// closed reason categories. The process boundary is distinct from main: native
// network_instance_id, not observed.process_instance_id, binds to canonical main.
type OpenShellProviderObservation struct {
	Identity                                                    OpenShellIdentity
	Provider, ReceiptID, DesiredSHA256                          string
	MainProcessInstanceID, NetworkInstanceID, ProcessInstanceID string
	State, Reason                                               string
	Ready                                                       bool
}

// ObserveProvider performs one readiness query against the original Ready VM.
// An empty receiptID creates a native durable OBSERVE receipt; a lost response
// must fail the attempt, never trigger another empty-receipt request.
func (c *OpenShellCustody) ObserveProvider(ctx context.Context, expected OpenShellIdentity, provider, receiptID string) (OpenShellProviderObservation, error) {
	observed := OpenShellProviderObservation{Identity: expected, Provider: provider}
	if !c.mu.TryLock() {
		return observed, fmt.Errorf("openshell custody operation in progress")
	}
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, openShellProviderWait)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	if err := c.available(); err != nil {
		return observed, err
	}
	if c.ctx == nil || c.client == nil {
		return observed, fmt.Errorf("openshell original provider connection unavailable")
	}
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	var record *OpenShellCustodyRecord
	for i := range c.state.Records {
		if c.state.Records[i].Worker.Identity == expected {
			copy := c.state.Records[i]
			record = &copy
			break
		}
	}
	if record == nil || record.Retained || !record.CreateAcknowledged || record.MainProcessInstanceID == "" || record.Exec != nil || record.StopRequestID != "" || record.Stopped != nil {
		return observed, fmt.Errorf("openshell original provider launch binding is unavailable")
	}
	if expected.Sandbox.Gateway != c.state.Binding.Gateway || expected.Sandbox.Workspace != c.state.Binding.Workspace || expected.ConfigSource != c.state.Binding.ConfigSource || record.Name != expected.SandboxName {
		return observed, fmt.Errorf("openshell provider binding differs from original custody")
	}
	var spec pb.SandboxSpec
	if err := proto.Unmarshal(record.Spec, &spec); err != nil {
		return observed, fmt.Errorf("openshell provider bound specification is invalid")
	}
	if provider == "" || !slices.Contains(spec.Providers, provider) {
		return observed, fmt.Errorf("openshell provider %q is not attached to original specification", provider)
	}
	observed.MainProcessInstanceID = record.MainProcessInstanceID
	if err := c.providerSandboxReady(ctx, *record); err != nil {
		return observed, err
	}
	response, err := c.client.GetSandboxProviderStatus(ctx, &pb.GetSandboxProviderStatusRequest{WorkspaceScope: c.scope(), Sandbox: record.Name, Provider: provider, ReceiptId: receiptID})
	if err != nil {
		if ctx.Err() != nil {
			return observed, ctx.Err()
		}
		return observed, fmt.Errorf("openshell provider %q status unavailable: %s", provider, status.Code(err))
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	readiness := response.GetStatus()
	receipt := readiness.GetReceipt()
	desired := receipt.GetDesired()
	observed.ReceiptID = receipt.GetReceiptId()
	observed.State, observed.Reason = readiness.GetState().String(), readiness.GetReason().String()
	observed.NetworkInstanceID = readiness.GetNetworkInstanceId()
	observed.ProcessInstanceID = readiness.GetObserved().GetProcessInstanceId()
	if receipt.GetReceiptId() == "" || (receiptID != "" && receipt.GetReceiptId() != receiptID) || receipt.GetProvider() != provider || receipt.GetWorkspace() != expected.Sandbox.Workspace || desired.GetSandboxId() != expected.Sandbox.SandboxID || desired.GetSandbox() != record.Name || desired.GetProviderId() == "" {
		return observed, fmt.Errorf("openshell provider %q receipt or desired identity conflicts with original binding", provider)
	}
	desiredBytes, err := openShellBytes(desired)
	if err != nil {
		return observed, fmt.Errorf("openshell provider desired identity is invalid")
	}
	sum := sha256.Sum256(desiredBytes)
	observed.DesiredSHA256 = hex.EncodeToString(sum[:])
	// Bracket the name-based provider query with original-ID/main checks. This
	// is observed continuity, not an atomic native expected-ID precondition.
	if err := c.providerSandboxReady(ctx, *record); err != nil {
		return observed, err
	}
	switch readiness.GetState() {
	case pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PERSISTED, pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PENDING:
		return observed, nil
	case pb.ProviderReadinessState_PROVIDER_READINESS_STATE_READY:
		installed := readiness.GetObserved()
		if observed.NetworkInstanceID != record.MainProcessInstanceID || installed.GetSessionId() == "" || installed.GetSequence() == 0 || installed.GetProcessInstanceId() == "" || !installed.GetCredentialsInstalled() || !installed.GetPolicyActive() || !installed.GetLaunchEnvironmentInstalled() || installed.GetAttachmentEpoch() != desired.GetAttachmentEpoch() || installed.GetProviderEnvRevision() != desired.GetProviderEnvRevision() || installed.GetConfigRevision() != desired.GetConfigRevision() || installed.GetPolicyHash() != desired.GetPolicyHash() {
			return observed, fmt.Errorf("openshell provider %q Ready evidence does not match original installed snapshot", provider)
		}
		observed.Ready = true
		return observed, nil
	default:
		return observed, fmt.Errorf("openshell provider %q readiness %s: %s", provider, observed.State, observed.Reason)
	}
}

func (c *OpenShellCustody) providerSandboxReady(ctx context.Context, record OpenShellCustodyRecord) error {
	response, err := c.client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: c.scope()})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("openshell provider VM observation unavailable: %s", status.Code(err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.available(); err != nil {
		return err
	}
	native := response.GetSandbox()
	if !sameOpenShellSandbox(native, record.Name, record.Worker.Identity.Sandbox.SandboxID, c.state.Binding.Workspace) || native.GetStatus().GetMainProcessInstanceId() != record.MainProcessInstanceID || native.GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_READY {
		return fmt.Errorf("openshell provider original VM identity or Ready generation changed")
	}
	spec, err := openShellBytes(native.GetSpec())
	if err != nil || native.GetSpec() == nil || !bytes.Equal(spec, record.Spec) {
		return fmt.Errorf("openshell provider original VM specification changed")
	}
	return nil
}

type openShellProviderEvidence struct {
	Phase, Error string
	Identity     OpenShellIdentity
	Providers    []string
	Observations map[string]OpenShellProviderObservation
}

func (s *openShellSession) waitProviders(providers []string) error {
	if err := s.phase("providers"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), openShellProviderWait)
	defer cancel()
	go func() {
		select {
		case <-s.stopRequested:
			cancel()
		case <-ctx.Done():
		}
	}()
	identity := s.worker.Record().Identity
	evidence := openShellProviderEvidence{Phase: "querying", Identity: identity, Providers: slices.Clone(providers), Observations: make(map[string]OpenShellProviderObservation)}
	persist := func() error {
		data, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		return filex.WriteFileAtomicDurable(filepath.Join(s.dir, "openshell-providers.json"), data)
	}
	if err := persist(); err != nil {
		return err
	}
	err := waitOpenShellProviders(ctx, s.custody, identity, providers, func(observed OpenShellProviderObservation) error {
		if observed.State != "" {
			evidence.Observations[observed.Provider] = observed
		}
		return persist()
	})
	evidence.Phase = "ready"
	if err != nil {
		evidence.Phase = "failed"
		evidence.Error = err.Error()
	}
	return errors.Join(err, persist())
}

func waitOpenShellProviders(ctx context.Context, custody *OpenShellCustody, identity OpenShellIdentity, providers []string, retain func(OpenShellProviderObservation) error) error {
	pinned := make(map[string]OpenShellProviderObservation)
	seen := make(map[string]bool)
	for _, provider := range providers {
		if strings.TrimSpace(provider) == "" || seen[provider] {
			return fmt.Errorf("openshell provider selection is empty or duplicated")
		}
		seen[provider] = true
	}
	for {
		allReady := true
		for _, provider := range providers {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("openshell provider readiness deadline or cancellation: %w", err)
			}
			prior := pinned[provider]
			observed, err := custody.ObserveProvider(ctx, identity, provider, prior.ReceiptID)
			if retained := retain(observed); retained != nil {
				return errors.Join(err, retained)
			}
			if err != nil {
				return err
			}
			if prior.ReceiptID != "" && (observed.ReceiptID != prior.ReceiptID || observed.DesiredSHA256 != prior.DesiredSHA256 || observed.MainProcessInstanceID != prior.MainProcessInstanceID) {
				return fmt.Errorf("openshell provider %q receipt identity changed during readiness wait", provider)
			}
			if prior.ReceiptID == "" {
				pinned[provider] = observed
			}
			allReady = allReady && observed.Ready
		}
		if allReady {
			return nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("openshell provider readiness deadline or cancellation: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
