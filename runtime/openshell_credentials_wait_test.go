package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Only provider CRUD is substituted. The managed child and original custody
// still own all actual fixture-process, VM Create/Get/Stop and lease evidence.
type credentialProviderClient struct {
	pb.OpenShellClient
	provider pb.OpenShellClient
}

func (c credentialProviderClient) GetProvider(ctx context.Context, r *pb.GetProviderRequest, opts ...grpc.CallOption) (*pb.ProviderResponse, error) {
	return c.provider.GetProvider(ctx, r, opts...)
}
func (c credentialProviderClient) GetProviderProfile(ctx context.Context, r *pb.GetProviderProfileRequest, opts ...grpc.CallOption) (*pb.ProviderProfileResponse, error) {
	return c.provider.GetProviderProfile(ctx, r, opts...)
}
func (c credentialProviderClient) ImportProviderProfiles(ctx context.Context, r *pb.ImportProviderProfilesRequest, opts ...grpc.CallOption) (*pb.ImportProviderProfilesResponse, error) {
	return c.provider.ImportProviderProfiles(ctx, r, opts...)
}
func (c credentialProviderClient) CreateProvider(ctx context.Context, r *pb.CreateProviderRequest, opts ...grpc.CallOption) (*pb.ProviderResponse, error) {
	return c.provider.CreateProvider(ctx, r, opts...)
}
func (c credentialProviderClient) UpdateProvider(ctx context.Context, r *pb.UpdateProviderRequest, opts ...grpc.CallOption) (*pb.ProviderResponse, error) {
	return c.provider.UpdateProvider(ctx, r, opts...)
}

func TestOpenShellCredentialSlowSourceDoesNotBlockOtherAcquire(t *testing.T) {
	fixture, source, _, now := credentialFixture(t)
	f := newManagedFixture(t, true)
	f.spec.CredentialSource = fixture.spec.CredentialSource
	o := f.open(t)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	o.credentialNow = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	c, err := o.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.client = credentialProviderClient{OpenShellClient: c.client, provider: fixture.custody.client}
	c.mu.Unlock()
	release, err := o.EnsureCredentials(t.Context(), []string{"chosen"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	source.mu.Lock()
	source.blockRetrieval = started
	source.releaseRetrieval = make(chan struct{})
	source.mu.Unlock()
	clock.Add(int64(55 * time.Minute))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("original background delivery did not reach blocked source")
	}
	acquired := make(chan error, 1)
	go func() { _, err := o.Acquire(t.Context(), "b"); acquired <- err }()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("unrelated route rejected by credential RPC: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated acquire waited for credential RPC")
	}
	// A provider-free route also traverses Ensure without the slow provider lock.
	other, err := o.EnsureCredentials(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	other()
	close(source.releaseRetrieval)
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := c.Snapshot()
		if len(state.Credentials) == 1 && !state.Credentials[0].SourceReadPending && state.Credentials[0].Pending == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("original source RPC did not finish its saved intent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	o.stopCredentials()
	select {
	case <-o.credentialDone:
	case <-time.After(time.Second):
		t.Fatal("original background delivery did not cancel")
	}
	release()
	if _, err := o.Release("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellCredentialSessionWaitsThenCreatesOrCancels(t *testing.T) {
	for _, mode := range []string{"recovered", "cancelled", "source-aborted", "source-not-found"} {
		cancelled := mode == "cancelled"
		t.Run(mode, func(t *testing.T) {
			fixture, source, _, _ := credentialFixture(t)
			source.unavailable = mode != "source-aborted" && mode != "source-not-found"
			source.abortRetrievalOnce = mode == "source-aborted"
			source.retrievalNotFoundOnce = mode == "source-not-found"
			r, f, target := openShellRuntimeFixture(t, func(f *managedFixture) {
				f.spec.CredentialSource = fixture.spec.CredentialSource
				managedMarker(t, f.root, "provider-ready")
			})
			o := f.owner
			c, err := o.Acquire(t.Context(), "a")
			if err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			c.client = credentialProviderClient{OpenShellClient: c.client, provider: fixture.custody.client}
			c.mu.Unlock()
			route := r.config.Profile.Routes["codex"]
			route.Spec = proto.Clone(route.Spec).(*pb.SandboxSpec)
			route.Spec.Providers = []string{"chosen"}
			r.config.Profile.Routes["codex"] = route
			h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "original-credential-wait", Provider: "codex", Model: "fixture", Prompt: "fixture", SystemPrompt: "fixture", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true})
			if err != nil {
				t.Fatal(err)
			}
			s := h.(*openShellSession)
			deadline := time.Now().Add(5 * time.Second)
			for {
				s.mu.Lock()
				phase, diagnostic := s.receipt.Phase, s.receipt.Error
				s.mu.Unlock()
				if phase == "credential_waiting" {
					expected := "Unavailable"
					if mode == "source-aborted" {
						expected = "Aborted"
					}
					if mode == "source-not-found" {
						expected = "NotFound"
					}
					if !strings.Contains(diagnostic, expected) {
						t.Fatal("native safe error missing")
					}
					break
				}
				select {
				case <-h.Done():
					t.Fatalf("temporary credential outage consumed task: %+v", h.Outcome())
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("credential waiting phase not reached")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(c.Snapshot().Records) != 0 {
				t.Fatal("VM created before credential readiness")
			}
			originalSession := h.ID()
			if cancelled {
				if err := h.Terminate(); err != nil {
					t.Fatal(err)
				}
			} else {
				source.mu.Lock()
				source.unavailable = false
				source.mu.Unlock()
			}
			select {
			case <-h.Done():
			case <-time.After(8 * time.Second):
				t.Fatal("original waiting session did not settle")
			}
			records := c.Snapshot().Records
			if cancelled {
				if len(records) != 0 || h.Outcome().Status != StatusCancelled {
					t.Fatal("cancelled credential wait created or replayed VM")
				}
			} else {
				if len(records) != 1 || records[0].Task.SessionID != originalSession || records[0].Stopped == nil {
					t.Fatal("recovery did not use the original session and one original stopped VM")
				}
				// This scope ends at Create and stop. The fixture has no provider-readiness
				// implementation for the later model launch; it must not claim model PASS.
				s.mu.Lock()
				prior := s.receipt.CredentialWaitError
				s.mu.Unlock()
				expected := "Unavailable"
				if mode == "source-aborted" {
					expected = "Aborted"
				}
				if mode == "source-not-found" {
					expected = "NotFound"
				}
				if !strings.Contains(prior, expected) {
					t.Fatal("recovered native error history lost")
				}
				if mode == "source-aborted" || mode == "source-not-found" {
					source.mu.Lock()
					events := append([]string(nil), source.retrievalEvents...)
					source.mu.Unlock()
					first := slices.Index(events, "retrieve")
					if first < 0 || len(events) < first+3 || events[first+1] != "readback" || events[first+2] != "retrieve" {
						t.Fatalf("source retrieval retried without original readback: %v", events)
					}
				}
			}
			data, err := os.ReadFile(filepath.Join(s.dir, "openshell-dispatch.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved openShellDispatchReceipt
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "synthetic-secret") || saved.SessionID != originalSession {
				t.Fatal("waiting receipt leaked or changed session")
			}
			if _, err := o.Release("a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenShellCredentialExpiryCarrierMatchesNativeMerge(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(55 * time.Minute)
	source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	for id, result := range target.results {
		response, ok := result.(*pb.ProviderResponse)
		if !ok || response.MutationId == "" {
			continue
		}
		var request pb.UpdateProviderRequest
		if err := proto.Unmarshal(target.requests[id], &request); err != nil {
			t.Fatal(err)
		}
		if request.GetCredentialExpirationTimes()["CODEX_AUTH_ACCESS_TOKEN"] == nil || len(request.Provider.GetCredentialExpirationTimes()) != 0 {
			t.Fatal("Update did not use canonical native top-level expiration carrier")
		}
	}
	*now = now.Add(55 * time.Minute)
	source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = nil
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	if len(target.provider.CredentialExpirationTimes) != 0 {
		t.Fatal("omitted expiry was not explicitly cleared")
	}
	// Pinned Rust accepts inner values and lets top-level values override them.
	native := proto.Clone(target.provider).(*dm.Provider)
	native.CredentialExpirationTimes = map[string]*timestamppb.Timestamp{"CODEX_AUTH_ACCESS_TOKEN": timestamppb.New(now.Add(time.Hour))}
	id, _ := openShellUUID()
	want := timestamppb.New(now.Add(2 * time.Hour))
	response, err := o.custody.client.UpdateProvider(t.Context(), &pb.UpdateProviderRequest{WorkspaceScope: o.custody.scope(), Provider: native, CredentialExpirationTimes: map[string]*timestamppb.Timestamp{"CODEX_AUTH_ACCESS_TOKEN": want}, RequestId: id})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(response.Provider.CredentialExpirationTimes["CODEX_AUTH_ACCESS_TOKEN"], want) {
		t.Fatal("top-level native expiry did not override inner expiry")
	}
	native.Metadata.ResourceVersion = response.Provider.Metadata.ResourceVersion
	id, _ = openShellUUID()
	_, err = o.custody.client.UpdateProvider(t.Context(), &pb.UpdateProviderRequest{WorkspaceScope: o.custody.scope(), Provider: native, ClearCredentialExpirationKeys: []string{"CODEX_AUTH_ACCESS_TOKEN"}, RequestId: id})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal("set+clear native conflict was accepted")
	}
}

func TestOpenShellCredentialPermanentFailureDoesNotWait(t *testing.T) {
	var waiting *openShellCredentialWaiting
	if errors.As(credentialFailure("target mutation", status.Error(codes.Aborted, "CAS conflict")), &waiting) {
		t.Fatal("target CAS conflict was changed to transient waiting")
	}
	for _, reason := range []string{"REQUEST_ID_PAYLOAD_MISMATCH", "REQUEST_REPLAY_UNAVAILABLE"} {
		native, _ := status.New(codes.FailedPrecondition, "private payload must not leak").WithDetails(&errdetails.ErrorInfo{Reason: reason})
		err := credentialFailure("target mutation", native.Err())
		var waiting *openShellCredentialWaiting
		if errors.As(err, &waiting) || !strings.Contains(err.Error(), reason) || strings.Contains(err.Error(), "private payload") {
			t.Fatal("permanent native refusal was retried or leaked")
		}
	}
}
