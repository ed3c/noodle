package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Only the new provider RPC is substituted; identity and lifecycle observations
// continue through the existing in-process native gRPC/mTLS fixture.
type providerTestClient struct {
	pb.OpenShellClient
	query func(context.Context, *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error)
}

func (c providerTestClient) GetSandboxProviderStatus(ctx context.Context, r *pb.GetSandboxProviderStatusRequest, _ ...grpc.CallOption) (*pb.GetSandboxProviderStatusResponse, error) {
	return c.query(ctx, r)
}

func providerFixture(t *testing.T) (*custodyGateway, *OpenShellCustody, OpenShellCustodyRecord) {
	t.Helper()
	g, binding, security, open := custodyInProcessFixture(t)
	c := custodyOpen(t, binding, security, nil, open)
	g.mutate(func() {
		g.afterCreate = func() {
			for _, s := range g.sandboxes {
				s.Status.MainProcessInstanceId = "main-instance"
			}
		}
	})
	_, err := c.Create(t.Context(), OpenShellTask{OrderID: "provider-order", AttemptID: "provider-attempt", SessionID: "provider-session"}, &pb.SandboxSpec{Command: []string{"hold"}, Providers: []string{"provider-one"}})
	if err != nil {
		t.Fatal(err)
	}
	return g, c, c.Snapshot().Records[0]
}

func providerReady(record OpenShellCustodyRecord) *pb.GetSandboxProviderStatusResponse {
	return &pb.GetSandboxProviderStatusResponse{Status: &pb.ProviderReadinessStatus{
		Receipt: &pb.ProviderMutationReceipt{ReceiptId: openShellRequestID, Provider: "provider-one", Workspace: record.Worker.Identity.Sandbox.Workspace, Desired: &pb.ProviderDesiredIdentity{
			SandboxId: record.Worker.Identity.Sandbox.SandboxID, Sandbox: record.Name, ProviderId: "native-provider", AttachmentEpoch: "attachment-epoch", ProviderEnvRevision: 2, ConfigRevision: 3, PolicyHash: "abcd",
		}},
		State: pb.ProviderReadinessState_PROVIDER_READINESS_STATE_READY, NetworkInstanceId: record.MainProcessInstanceID,
		Observed: &pb.ProviderReadinessObservation{
			SessionId: "supervisor-session", Sequence: 1, ProcessInstanceId: "authenticated-process-boundary", AttachmentEpoch: "attachment-epoch", ProviderEnvRevision: 2, ConfigRevision: 3, PolicyHash: "abcd", CredentialsInstalled: true, PolicyActive: true, LaunchEnvironmentInstalled: true,
		},
	}}
}

func providerClient(c *OpenShellCustody, query func(context.Context, *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = providerTestClient{OpenShellClient: c.client, query: query}
}

func TestOpenShellProviderReadyPinsCanonicalNetworkInstance(t *testing.T) {
	g, c, record := providerFixture(t)
	before := c.Snapshot()
	providerClient(c, func(_ context.Context, r *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
		if r.Sandbox != record.Name || r.WorkspaceScope.GetWorkspace() != record.Worker.Identity.Sandbox.Workspace || r.Provider != "provider-one" || r.ReceiptId != "" {
			t.Fatalf("wrong original request: %v", r)
		}
		return providerReady(record), nil
	})
	observed, err := c.ObserveProvider(t.Context(), record.Worker.Identity, "provider-one", "")
	if err != nil || !observed.Ready || observed.ReceiptID != openShellRequestID || observed.DesiredSHA256 == "" {
		t.Fatalf("ready observation: %+v %v", observed, err)
	}
	if observed.ProcessInstanceID == observed.MainProcessInstanceID || observed.NetworkInstanceID != observed.MainProcessInstanceID {
		t.Fatal("authenticated process boundary conflated with canonical main instance")
	}
	if !reflect.DeepEqual(before, c.Snapshot()) || !reflect.DeepEqual(before, custodyDisk(t, before.Binding)) {
		t.Fatal("provider observation changed local custody ledger")
	}
	if creates, stops, execs := g.counts(); creates != 1 || stops != 0 || execs != 0 {
		t.Fatalf("provider observation mutated lifecycle: %d %d %d", creates, stops, execs)
	}
}

func TestOpenShellProviderPendingPinsOneReceipt(t *testing.T) {
	_, c, record := providerFixture(t)
	calls := 0
	providerClient(c, func(_ context.Context, r *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
		calls++
		want := openShellRequestID
		if calls == 1 {
			want = ""
		}
		if r.ReceiptId != want {
			t.Fatalf("receipt pin: got %q want %q", r.ReceiptId, want)
		}
		response := providerReady(record)
		if calls == 1 {
			response.Status.State = pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PENDING
			response.Status.Observed = nil
			response.Status.NetworkInstanceId = ""
		}
		return response, nil
	})
	var retained []OpenShellProviderObservation
	err := waitOpenShellProviders(t.Context(), c, record.Worker.Identity, []string{"provider-one"}, func(observed OpenShellProviderObservation) error {
		retained = append(retained, observed)
		return nil
	})
	if err != nil || calls != 2 || len(retained) != 2 || retained[0].Ready || !retained[1].Ready {
		t.Fatalf("pending then ready: calls=%d evidence=%+v err=%v", calls, retained, err)
	}
}

func TestOpenShellProviderRejectsUnsafeReadiness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pb.ProviderReadinessStatus)
		want   string
	}{
		{"withheld", func(s *pb.ProviderReadinessStatus) {
			s.State = pb.ProviderReadinessState_PROVIDER_READINESS_STATE_WITHHELD
			s.Reason = pb.ProviderReadinessReason_PROVIDER_READINESS_REASON_CREDENTIAL_EXPIRED
		}, "CREDENTIAL_EXPIRED"},
		{"failed", func(s *pb.ProviderReadinessStatus) {
			s.State = pb.ProviderReadinessState_PROVIDER_READINESS_STATE_FAILED
		}, "FAILED"},
		{"missing-receipt", func(s *pb.ProviderReadinessStatus) { s.Receipt = nil }, "receipt"},
		{"wrong-native-id", func(s *pb.ProviderReadinessStatus) { s.Receipt.Desired.SandboxId = "replacement" }, "identity"},
		{"wrong-network", func(s *pb.ProviderReadinessStatus) { s.NetworkInstanceId = "replacement-main" }, "snapshot"},
		{"missing-process", func(s *pb.ProviderReadinessStatus) { s.Observed.ProcessInstanceId = "" }, "snapshot"},
		{"missing-environment", func(s *pb.ProviderReadinessStatus) { s.Observed.LaunchEnvironmentInstalled = false }, "snapshot"},
		{"stale-policy", func(s *pb.ProviderReadinessStatus) { s.Observed.PolicyHash = "different" }, "snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, record := providerFixture(t)
			before := c.Snapshot()
			calls := 0
			providerClient(c, func(context.Context, *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
				calls++
				response := providerReady(record)
				tc.change(response.Status)
				return response, nil
			})
			err := waitOpenShellProviders(t.Context(), c, record.Worker.Identity, []string{"provider-one"}, func(OpenShellProviderObservation) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) || calls != 1 {
				t.Fatalf("rejected readiness: calls=%d err=%v", calls, err)
			}
			if !reflect.DeepEqual(before, c.Snapshot()) {
				t.Fatal("provider failure poisoned custody needed for original Stop")
			}
		})
	}
}

func TestOpenShellProviderRejectsReceiptDrift(t *testing.T) {
	for _, field := range []string{"receipt", "desired"} {
		t.Run(field, func(t *testing.T) {
			_, c, record := providerFixture(t)
			calls := 0
			providerClient(c, func(context.Context, *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
				calls++
				response := providerReady(record)
				if calls == 1 {
					response.Status.State = pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PENDING
				} else if field == "receipt" {
					response.Status.Receipt.ReceiptId = "another-receipt"
				} else {
					response.Status.Receipt.Desired.ProviderResourceVersion++
				}
				return response, nil
			})
			err := waitOpenShellProviders(t.Context(), c, record.Worker.Identity, []string{"provider-one"}, func(OpenShellProviderObservation) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "receipt") || calls != 2 {
				t.Fatalf("receipt drift: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestOpenShellProviderRejectsReplacementAroundStatus(t *testing.T) {
	for _, when := range []string{"before", "during"} {
		t.Run(when, func(t *testing.T) {
			g, c, record := providerFixture(t)
			replace := func() { g.mutate(func() { g.sandboxes[record.Name].Metadata.Id = "replacement" }) }
			calls := 0
			providerClient(c, func(context.Context, *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
				calls++
				replace()
				return providerReady(record), nil
			})
			if when == "before" {
				replace()
			}
			observed, err := c.ObserveProvider(t.Context(), record.Worker.Identity, "provider-one", "")
			if err == nil || observed.Ready || !strings.Contains(err.Error(), "original VM identity") {
				t.Fatalf("replacement accepted: %+v %v", observed, err)
			}
			if when == "before" && calls != 0 || when == "during" && calls != 1 {
				t.Fatalf("unexpected status request count: %d", calls)
			}
		})
	}
}

func TestOpenShellProviderLostReceiptResponseNeverRetries(t *testing.T) {
	_, c, record := providerFixture(t)
	calls := 0
	providerClient(c, func(context.Context, *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
		calls++
		return nil, status.Error(codes.Unavailable, "private-credential-value")
	})
	err := waitOpenShellProviders(t.Context(), c, record.Worker.Identity, []string{"provider-one"}, func(OpenShellProviderObservation) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Unavailable") || strings.Contains(err.Error(), "private-credential-value") || calls != 1 {
		t.Fatalf("lost response retried or leaked error: calls=%d err=%v", calls, err)
	}
}

func TestOpenShellProviderTimeoutAndWithheldStopOriginal(t *testing.T) {
	for _, outcome := range []string{"timeout", "withheld"} {
		t.Run(outcome, func(t *testing.T) {
			g, c, record := providerFixture(t)
			worker, err := c.RecoverWorker(t.Context(), record.Worker.Identity.Sandbox)
			if err != nil {
				t.Fatal(err)
			}
			s := newOpenShellSession(&OpenShellRuntime{active: make(map[string]*openShellSession)}, "provider-session", t.TempDir())
			s.worker, s.custody = worker, c
			providerClient(c, func(ctx context.Context, _ *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
				if outcome == "timeout" {
					// A native request deadline is propagated immediately; the
					// original VM Stop uses its own fresh context.
					return nil, status.Error(codes.DeadlineExceeded, "private-detail")
				}
				response := providerReady(record)
				response.Status.State = pb.ProviderReadinessState_PROVIDER_READINESS_STATE_WITHHELD
				response.Status.Reason = pb.ProviderReadinessReason_PROVIDER_READINESS_REASON_CREDENTIAL_EXPIRED
				return response, nil
			})
			err = s.waitProviders([]string{"provider-one"})
			if err == nil {
				t.Fatal("failed provider accepted")
			}
			s.fail(err)
			s.finishStopped()
			if s.boundaryError != nil || worker.Record().Boundary != OpenShellBoundaryStopped || s.Outcome().Status != StatusFailed {
				t.Fatalf("provider failure did not settle original: %v %+v", s.boundaryError, s.Outcome())
			}
			if creates, stops, execs := g.counts(); creates != 1 || stops != 1 || execs != 0 {
				t.Fatalf("unexpected model or lifecycle effect: %d %d %d", creates, stops, execs)
			}
			data, err := os.ReadFile(filepath.Join(s.dir, "openshell-providers.json"))
			var evidence openShellProviderEvidence
			if err != nil || json.Unmarshal(data, &evidence) != nil || evidence.Phase != "failed" || evidence.Error == "" || strings.Contains(string(data), "private-detail") {
				t.Fatalf("failure evidence lost: %s %v", data, err)
			}
			if outcome == "withheld" && evidence.Observations["provider-one"].ReceiptID != openShellRequestID {
				t.Fatal("withheld receipt evidence lost")
			}
		})
	}
}

func TestOpenShellProviderPendingWaitHonorsDeadline(t *testing.T) {
	_, c, record := providerFixture(t)
	providerClient(c, func(context.Context, *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
		response := providerReady(record)
		response.Status.State = pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PENDING
		return response, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	err := waitOpenShellProviders(ctx, c, record.Worker.Identity, []string{"provider-one"}, func(OpenShellProviderObservation) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending wait ignored deadline: %v", err)
	}
}

func TestOpenShellProviderRuntimeGateRejectsMissingCanonicalInstance(t *testing.T) {
	r, f, target := openShellRuntimeFixture(t)
	c, err := f.owner.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	r.config.Profile.Routes["codex"].Spec.Providers = []string{"provider-one"}
	providerClient(c, func(context.Context, *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
		t.Error("missing canonical main instance reached provider query")
		return nil, status.Error(codes.FailedPrecondition, "missing original")
	})
	handle, err := r.Dispatch(t.Context(), DispatchRequest{Name: "provider-gate", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handle.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("provider rejection did not settle: %+v", handle.Outcome())
	}
	s := handle.(*openShellSession)
	if s.Outcome().Status != StatusFailed || !strings.Contains(s.Outcome().Reason, "original provider launch binding") || s.worker.Record().Boundary != OpenShellBoundaryStopped || s.receipt.CandidateHead != "" {
		t.Fatalf("runtime provider gate failed: %+v", s.receipt)
	}
	records := c.Snapshot().Records
	if len(records) != 1 || records[0].Exec != nil || !records[0].StopAcknowledged || records[0].Stopped == nil {
		t.Fatalf("provider failure started model or lost original stop evidence: %+v", records)
	}
}

// The positive runtime integration uses the original managed child and real
// native gRPC. The marker enables readiness only for this fixture's provider.
func (s *managedGatewayServer) GetSandboxProviderStatus(ctx context.Context, request *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
	if _, err := os.Stat(filepath.Join(s.root, "provider-ready")); err != nil {
		return nil, status.Error(codes.Unimplemented, "provider fixture not enabled")
	}
	if request.Provider != "provider-one" || (request.ReceiptId != "" && request.ReceiptId != openShellRequestID) {
		return nil, status.Error(codes.InvalidArgument, "provider fixture binding changed")
	}
	response, err := s.GetSandbox(ctx, &pb.GetSandboxRequest{Name: request.Sandbox, WorkspaceScope: request.WorkspaceScope})
	if err != nil {
		return nil, err
	}
	native := response.GetSandbox()
	s.event("provider-ready " + native.GetMetadata().GetId())
	return providerReady(OpenShellCustodyRecord{
		Name: native.GetMetadata().GetName(), MainProcessInstanceID: native.GetStatus().GetMainProcessInstanceId(),
		Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{Sandbox: OpenShellSandboxRef{SandboxID: native.GetMetadata().GetId(), Workspace: native.GetMetadata().GetWorkspace()}}},
	}), nil
}

func TestOpenShellProviderRuntimeReadyExecutesPublishesAndStops(t *testing.T) {
	r, f, target := openShellRuntimeFixture(t)
	managedMarker(t, f.root, "provider-ready")
	r.config.Profile.Routes["codex"].Spec.Providers = []string{"provider-one"}
	handle, err := r.Dispatch(t.Context(), DispatchRequest{Name: "provider-ready", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handle.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("ready provider did not settle: %+v", handle.Outcome())
	}
	s := handle.(*openShellSession)
	if s.Outcome().Status != StatusCompleted || s.receipt.CandidateHead == "" || s.receipt.CandidateHead == s.receipt.Baseline.Head || s.worker.Record().Boundary != OpenShellBoundaryStopped {
		t.Fatalf("provider-ready runtime result: %+v", s.receipt)
	}
	data, err := os.ReadFile(filepath.Join(target, "result.txt"))
	if err != nil || string(data) != "accepted output\n" {
		t.Fatalf("published artifact missing: %s %v", data, err)
	}
	records := s.custody.Snapshot().Records
	if len(records) != 1 || records[0].Exec == nil || !records[0].Exec.Result.ExitObserved || !records[0].StopAcknowledged || records[0].Stopped == nil {
		t.Fatalf("native Exec/Stop evidence incomplete: %+v", records)
	}
	data, err = os.ReadFile(filepath.Join(s.dir, "openshell-providers.json"))
	var evidence openShellProviderEvidence
	if err != nil || json.Unmarshal(data, &evidence) != nil || evidence.Phase != "ready" {
		t.Fatalf("provider-ready evidence missing: %s %v", data, err)
	}
	observed := evidence.Observations["provider-one"]
	if !observed.Ready || observed.Identity != records[0].Worker.Identity || observed.NetworkInstanceID != records[0].MainProcessInstanceID || observed.ProcessInstanceID == observed.NetworkInstanceID {
		t.Fatalf("provider evidence detached from original runtime instance: %+v", observed)
	}
}
