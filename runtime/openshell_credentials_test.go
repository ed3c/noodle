package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Real mTLS gRPC, with only synthetic runtime credentials and an in-memory
// native mutation receipt store. The server deliberately redacts GetProvider.
type credentialNative struct {
	pb.UnimplementedOpenShellServer
	mu                                                     sync.Mutex
	provider                                               *dm.Provider
	profile                                                *pb.ProviderProfile
	values                                                 map[string]*pb.ProviderCredentialValue
	requests                                               map[string][]byte
	results                                                map[string]proto.Message
	lost                                                   string
	gets, retrievals, readbacks, creates, updates, imports int
	unavailable, sourceLost, refreshing                    bool
	abortRetrievalOnce                                     bool
	retrievalEvents                                        []string
	hiddenCredentialKeys                                   map[string]bool
	afterRetrieval                                         func(*credentialNative)
	blockRetrieval                                         chan struct{}
	releaseRetrieval                                       chan struct{}
	sandbox                                                *pb.Sandbox
	receipt                                                *pb.ProviderMutationReceipt
	installationReady                                      bool
	refreshReadback                                        *pb.GetProviderRefreshStatusResponse
}

func credentialRPC(t *testing.T, server pb.OpenShellServer) (string, OpenShellTLS) {
	t.Helper()
	security, config := openShellTestTLS(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(config)))
	pb.RegisterOpenShellServer(s, server)
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Serve(l) }()
	t.Cleanup(func() { s.Stop(); <-done })
	return "https://" + l.Addr().String(), security
}

func (s *credentialNative) ListSandboxes(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	return &pb.ListSandboxesResponse{}, nil
}
func (s *credentialNative) GetSandbox(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sandbox == nil {
		return nil, status.Error(codes.NotFound, "absent")
	}
	return &pb.SandboxResponse{Sandbox: proto.Clone(s.sandbox).(*pb.Sandbox)}, nil
}

func (s *credentialNative) GetSandboxProviderStatus(_ context.Context, request *pb.GetSandboxProviderStatusRequest) (*pb.GetSandboxProviderStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receipt == nil || request.ReceiptId != s.receipt.ReceiptId {
		return nil, status.Error(codes.NotFound, "absent receipt")
	}
	desired := s.receipt.Desired
	state := pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PENDING
	if s.installationReady {
		state = pb.ProviderReadinessState_PROVIDER_READINESS_STATE_READY
	}
	return &pb.GetSandboxProviderStatusResponse{Status: &pb.ProviderReadinessStatus{Receipt: proto.Clone(s.receipt).(*pb.ProviderMutationReceipt), State: state, NetworkInstanceId: "original-main", Observed: &pb.ProviderReadinessObservation{SessionId: "supervisor", Sequence: 1, AttachmentEpoch: desired.AttachmentEpoch, ProviderEnvRevision: desired.ProviderEnvRevision, ConfigRevision: desired.ConfigRevision, PolicyHash: desired.PolicyHash, CredentialsInstalled: s.installationReady, LaunchEnvironmentInstalled: s.installationReady, PolicyActive: s.installationReady, ProcessInstanceId: "process"}}}, nil
}

func (s *credentialNative) GetProvider(_ context.Context, r *pb.GetProviderRequest) (*pb.ProviderResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.unavailable {
		return nil, status.Error(codes.Unavailable, "synthetic-secret-must-not-leak")
	}
	if s.provider == nil || r.Name != s.provider.GetMetadata().GetName() {
		return nil, status.Error(codes.NotFound, "absent")
	}
	p := proto.Clone(s.provider).(*dm.Provider)
	if s.values != nil {
		p.Credentials = make(map[string]string)
		p.CredentialExpirationTimes = make(map[string]*timestamppb.Timestamp)
		for key, value := range s.values {
			if !s.hiddenCredentialKeys[key] {
				p.Credentials[key] = "<redacted>"
				if value.ExpirationTime != nil {
					p.CredentialExpirationTimes[key] = value.ExpirationTime
				}
			}
		}
	}
	for key := range p.Credentials {
		p.Credentials[key] = "<redacted>"
	}
	return &pb.ProviderResponse{Provider: p}, nil
}
func (s *credentialNative) GetProviderProfile(_ context.Context, r *pb.GetProviderProfileRequest) (*pb.ProviderProfileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.profile == nil || r.Id != s.profile.Id {
		return nil, status.Error(codes.NotFound, "absent")
	}
	return &pb.ProviderProfileResponse{Profile: proto.Clone(s.profile).(*pb.ProviderProfile)}, nil
}
func (s *credentialNative) GetProviderCredentials(ctx context.Context, r *pb.GetProviderCredentialsRequest) (*pb.GetProviderCredentialsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retrievals++
	s.retrievalEvents = append(s.retrievalEvents, "retrieve")
	if s.abortRetrievalOnce {
		s.abortRetrievalOnce = false
		return nil, status.Error(codes.Aborted, "provider changed during credential resolution")
	}
	if s.blockRetrieval != nil {
		close(s.blockRetrieval)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.releaseRetrieval:
		}
		s.blockRetrieval = nil
	}
	if s.sourceLost {
		s.sourceLost = false
		return nil, status.Error(codes.Unavailable, "synthetic-secret-must-not-leak")
	}
	values := make(map[string]*pb.ProviderCredentialValue)
	for _, key := range r.CredentialKeys {
		if s.values[key] == nil {
			return nil, status.Error(codes.InvalidArgument, "unexportable")
		}
		if expiry := s.values[key].ExpirationTime; expiry != nil && !expiry.AsTime().After(time.Now().Add(r.GetMinimumRemainingLifetime().AsDuration())) {
			return nil, status.Error(codes.FailedPrecondition, "credential cannot meet the requested remaining lifetime")
		}
		values[key] = proto.Clone(s.values[key]).(*pb.ProviderCredentialValue)
	}
	if s.afterRetrieval != nil {
		action := s.afterRetrieval
		s.afterRetrieval = nil
		action(s)
	}
	return &pb.GetProviderCredentialsResponse{Credentials: values}, nil
}
func (s *credentialNative) GetProviderRefreshStatus(context.Context, *pb.GetProviderRefreshStatusRequest) (*pb.GetProviderRefreshStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readbacks++
	s.retrievalEvents = append(s.retrievalEvents, "readback")
	if s.refreshReadback != nil {
		return proto.Clone(s.refreshReadback).(*pb.GetProviderRefreshStatusResponse), nil
	}
	state := "refreshed"
	if s.refreshing {
		state = "refresh_in_progress"
	}
	var rows []*pb.ProviderCredentialRefreshStatus
	for _, credential := range s.profile.GetCredentials() {
		if !openShellGatewayMintable(credential.GetRefresh().GetStrategy()) || len(credential.EnvVars) == 0 {
			continue
		}
		key := credential.EnvVars[0]
		for _, candidate := range credential.EnvVars {
			if s.values[candidate] != nil {
				key = candidate
				break
			}
		}
		rows = append(rows, &pb.ProviderCredentialRefreshStatus{Provider: s.provider.GetMetadata().GetName(), ProviderId: s.provider.GetMetadata().GetId(), CredentialKey: key, Strategy: credential.GetRefresh().GetStrategy(), Status: state})
	}
	return &pb.GetProviderRefreshStatusResponse{Credentials: rows}, nil
}
func (s *credentialNative) replay(id string, request proto.Message) (proto.Message, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "missing ID")
	}
	data, _ := openShellBytes(request)
	if previous, ok := s.requests[id]; ok {
		if !bytes.Equal(data, previous) {
			return nil, status.Error(codes.FailedPrecondition, "REQUEST_ID_PAYLOAD_MISMATCH")
		}
		return proto.Clone(s.results[id]), nil
	}
	return nil, nil
}
func (s *credentialNative) ImportProviderProfiles(_ context.Context, r *pb.ImportProviderProfilesRequest) (*pb.ImportProviderProfilesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, err := s.replay(r.RequestId, r); previous != nil || err != nil {
		if err != nil {
			return nil, err
		}
		return previous.(*pb.ImportProviderProfilesResponse), nil
	}
	s.imports++
	s.profile = proto.Clone(r.Profiles[0].Profile).(*pb.ProviderProfile)
	result := &pb.ImportProviderProfilesResponse{Imported: true, Profiles: []*pb.ProviderProfile{proto.Clone(s.profile).(*pb.ProviderProfile)}}
	s.remember(r.RequestId, r, result)
	if s.lost == "import" {
		s.lost = ""
		return nil, status.Error(codes.Unavailable, "synthetic-secret-must-not-leak")
	}
	return result, nil
}
func (s *credentialNative) remember(id string, request, result proto.Message) {
	if s.requests == nil {
		s.requests = map[string][]byte{}
		s.results = map[string]proto.Message{}
	}
	s.requests[id], _ = openShellBytes(request)
	s.results[id] = proto.Clone(result)
}
func (s *credentialNative) CreateProvider(_ context.Context, r *pb.CreateProviderRequest) (*pb.ProviderResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, err := s.replay(r.RequestId, r); previous != nil || err != nil {
		if err != nil {
			return nil, err
		}
		return previous.(*pb.ProviderResponse), nil
	}
	if s.provider != nil {
		return nil, status.Error(codes.AlreadyExists, "exists")
	}
	s.creates++
	s.provider = proto.Clone(r.Provider).(*dm.Provider)
	s.provider.Metadata.Id = "target-provider"
	s.provider.Metadata.Workspace = "owned-workspace"
	s.provider.Metadata.ResourceVersion = 1
	result := &pb.ProviderResponse{Provider: proto.Clone(s.provider).(*dm.Provider)}
	s.remember(r.RequestId, r, result)
	if s.lost == "create" {
		s.lost = ""
		return nil, status.Error(codes.Unavailable, "synthetic-secret-must-not-leak")
	}
	return result, nil
}
func (s *credentialNative) UpdateProvider(_ context.Context, r *pb.UpdateProviderRequest) (*pb.ProviderResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, err := s.replay(r.RequestId, r); previous != nil || err != nil {
		if err != nil {
			return nil, err
		}
		return previous.(*pb.ProviderResponse), nil
	}
	if s.provider == nil || r.Provider.GetMetadata().GetResourceVersion() != s.provider.GetMetadata().GetResourceVersion() {
		return nil, status.Error(codes.Aborted, "CAS")
	}
	// Match pinned Rust: merge Provider expiry first, extend top-level expiry,
	// reject set+clear conflicts, and retain omitted credential/config keys.
	candidate := proto.Clone(s.provider).(*dm.Provider)
	if candidate.CredentialExpirationTimes == nil {
		candidate.CredentialExpirationTimes = make(map[string]*timestamppb.Timestamp)
	}
	for key, value := range r.Provider.GetCredentialExpirationTimes() {
		candidate.CredentialExpirationTimes[key] = value
	}
	for key, value := range r.GetCredentialExpirationTimes() {
		candidate.CredentialExpirationTimes[key] = value
	}
	for _, key := range r.ClearCredentialExpirationKeys {
		if r.Provider.GetCredentialExpirationTimes()[key] != nil || r.GetCredentialExpirationTimes()[key] != nil {
			return nil, status.Error(codes.InvalidArgument, "expiration both set and cleared")
		}
		delete(candidate.CredentialExpirationTimes, key)
	}
	for key, value := range r.Provider.GetCredentials() {
		if value == "" {
			delete(candidate.Credentials, key)
		} else {
			candidate.Credentials[key] = value
		}
	}
	if candidate.Config == nil {
		candidate.Config = make(map[string]string)
	}
	for key, value := range r.Provider.GetConfig() {
		if value == "" {
			delete(candidate.Config, key)
		} else {
			candidate.Config[key] = value
		}
	}
	s.updates++
	version := s.provider.Metadata.ResourceVersion
	s.provider = candidate
	s.provider.Metadata.Id = "target-provider"
	s.provider.Metadata.Workspace = "owned-workspace"
	s.provider.Metadata.ResourceVersion = version + 1
	result := &pb.ProviderResponse{Provider: proto.Clone(s.provider).(*dm.Provider), MutationId: fmt.Sprintf("mutation-%d", s.updates)}
	if s.sandbox != nil {
		s.receipt = &pb.ProviderMutationReceipt{ReceiptId: fmt.Sprintf("receipt-%d", s.updates), MutationId: result.MutationId, Provider: r.Provider.Metadata.Name, Workspace: "owned-workspace", Desired: &pb.ProviderDesiredIdentity{SandboxId: s.sandbox.Metadata.Id, Sandbox: s.sandbox.Metadata.Name, ProviderId: s.provider.Metadata.Id, AttachmentEpoch: "original-attachment", ProviderEnvRevision: s.provider.Metadata.ResourceVersion, ConfigRevision: 1, PolicyHash: "original-policy"}}
		result.TargetReceipts = []*pb.ProviderMutationReceipt{proto.Clone(s.receipt).(*pb.ProviderMutationReceipt)}
	}
	s.remember(r.RequestId, r, result)
	if s.lost == "update" {
		s.lost = ""
		return nil, status.Error(codes.Unavailable, "synthetic-secret-must-not-leak")
	}
	return result, nil
}

func credentialFixture(t *testing.T) (*OpenShellGateway, *credentialNative, *credentialNative, *time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	source := &credentialNative{provider: &dm.Provider{Metadata: &dm.ObjectMeta{Name: "chosen", Id: "source-provider", Workspace: "source-workspace"}, Type: "original", ProfileWorkspace: "source-workspace"}, profile: &pb.ProviderProfile{Id: "original", Credentials: []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"CODEX_AUTH_ACCESS_TOKEN"}, Required: true, Refresh: &pb.ProviderCredentialRefresh{Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN, Material: []*pb.ProviderCredentialRefreshMaterial{{Name: "refresh_token", Secret: true}}}}}}, values: map[string]*pb.ProviderCredentialValue{"CODEX_AUTH_ACCESS_TOKEN": {Value: "synthetic-initial-access", ExpirationTime: timestamppb.New(now.Add(time.Hour))}}}
	target := &credentialNative{}
	sourceEndpoint, sourceTLS := credentialRPC(t, source)
	targetEndpoint, targetTLS := credentialRPC(t, target)
	binding := custodyBinding(t, targetEndpoint)
	c, err := OpenOpenShellCustody(t.Context(), binding, targetTLS, nil, filepath.Dir(binding.StateRoot))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := newOpenShellConnection(sourceEndpoint, sourceTLS)
	if err != nil {
		t.Fatal(err)
	}
	o := &OpenShellGateway{spec: OpenShellGatewaySpec{Binding: binding, TLS: targetTLS, CredentialSource: &OpenShellCredentialSource{Gateway: sourceEndpoint, Workspace: "source-workspace", TLS: sourceTLS, BindingSHA256: strings.Repeat("a", 64)}}, custody: c, credentialClient: pb.NewOpenShellClient(conn), credentialConnection: conn, credentialNow: func() time.Time { return now }}
	c.credentialSourceBinding = o.spec.CredentialSource.BindingSHA256
	t.Cleanup(func() { o.stopCredentials(); _ = o.custody.Close() })
	return o, source, target, &now
}
func credentialDeliver(t *testing.T, o *OpenShellGateway) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return o.deliverCredential(ctx, "chosen")
}
func credentialNoLeaks(t *testing.T, o *OpenShellGateway) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(o.custody.state.Binding.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic-initial-access", "synthetic-next-access", "synthetic-secret-must-not-leak", "refresh_token"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("credential material leaked into owner.json: %q", secret)
		}
	}
}

func TestOpenShellCredentialBootstrapAndRefresh(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	if source.retrievals != 1 || target.creates != 1 || target.imports != 1 || target.updates != 0 {
		t.Fatal("fresh provisioning effects differ")
	}
	if !target.profile.Credentials[0].StablePlaceholder || target.profile.Credentials[0].Refresh != nil {
		t.Fatal("grant or revision placeholder reached target")
	}
	*now = now.Add(55 * time.Minute)
	source.values["CODEX_AUTH_ACCESS_TOKEN"] = &pb.ProviderCredentialValue{Value: "synthetic-next-access", ExpirationTime: timestamppb.New(now.Add(time.Hour))}
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	if target.creates != 1 || target.updates != 1 || target.provider.Credentials["CODEX_AUTH_ACCESS_TOKEN"] != "synthetic-next-access" {
		t.Fatal("refresh recreated provider or retained old token")
	}
	if target.provider.CredentialExpirationTimes["CODEX_AUTH_ACCESS_TOKEN"].AsTime() != now.Add(time.Hour) {
		t.Fatal("native expiration lost")
	}
	credentialNoLeaks(t, o)
}

func TestOpenShellCredentialLostMutationRestart(t *testing.T) {
	for _, kind := range []string{"import", "create", "update"} {
		t.Run(kind, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			if kind == "update" {
				if err := credentialDeliver(t, o); err != nil {
					t.Fatal(err)
				}
				*now = now.Add(55 * time.Minute)
				source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
			}
			target.lost = kind
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("lost mutation reply accepted")
			}
			pending := o.custody.Snapshot().Credentials[0].Pending
			if pending == nil || pending.Kind != kind {
				t.Fatal("original operation lost")
			}
			credentialNoLeaks(t, o)
			// Simulate a different process reading the original custody bytes. Same
			// target key, original request ID and ciphertext must recover the ACK.
			binding := o.custody.state.Binding
			security := o.spec.TLS
			if err := o.custody.Close(); err != nil {
				t.Fatal(err)
			}
			connection, err := newOpenShellConnection(binding.Gateway, security)
			if err != nil {
				t.Fatal(err)
			}
			c, err := reserveOpenShellCustodyWithCredentials(binding, connection, nil, filepath.Dir(binding.StateRoot), o.spec.CredentialSource)
			if err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			err = c.initialize(t.Context())
			c.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			o.custody = c
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			if target.imports != 1 || target.creates != 1 || (kind == "update" && target.updates != 1) {
				t.Fatal("unknown operation was reminted or replayed with a new ID")
			}
			if o.custody.state.Credentials[0].Pending != nil {
				t.Fatal("acknowledged operation stayed unknown")
			}
		})
	}
}

func TestOpenShellCredentialUnknownSourceReadback(t *testing.T) {
	o, source, target, _ := credentialFixture(t)
	source.sourceLost = true
	if err := credentialDeliver(t, o); err == nil || !o.custody.state.Credentials[0].SourceReadPending {
		t.Fatal("lost retrieval was not retained")
	}
	source.refreshing = true
	if err := credentialDeliver(t, o); err == nil {
		t.Fatal("uncertain source refresh retried")
	}
	if source.retrievals != 1 || source.readbacks != 2 || target.creates != 0 {
		t.Fatal("readback was skipped")
	}
	source.refreshing = false
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	credentialNoLeaks(t, o)
}

func TestOpenShellCredentialReplayTamperAndExpiry(t *testing.T) {
	for _, mode := range []string{"ciphertext", "request-id", "expiry", "target-key", "payload-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			o, _, target, now := credentialFixture(t)
			target.lost = "create"
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("lost response expected")
			}
			r := &o.custody.state.Credentials[0]
			intent := r.Pending
			switch mode {
			case "ciphertext":
				intent.SealedPayload[len(intent.SealedPayload)-1] ^= 1
			case "request-id":
				intent.RequestID = "changed"
			case "expiry":
				*now = now.Add(24 * time.Hour)
			case "target-key":
				security, _ := openShellTestTLS(t)
				o.spec.TLS = security
			case "payload-mismatch":
				target.requests[intent.RequestID] = []byte("another payload")
			}
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("unsafe replay accepted")
			}
			if target.creates != 1 || target.updates != 0 || r.Pending == nil {
				t.Fatal("unsafe replay lost uncertainty or sent new operation")
			}
			credentialNoLeaks(t, o)
		})
	}
}

func TestOpenShellCredentialUnavailableAndUnusedProvider(t *testing.T) {
	o, source, target, _ := credentialFixture(t)
	source.unavailable = true
	if err := credentialDeliver(t, o); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("source failure missing or leaked")
	}
	if target.creates != 0 {
		t.Fatal("source failure created target")
	}
	source.unavailable = false
	release, err := o.EnsureCredentials(t.Context(), []string{"chosen"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	o.stopCredentials()
	if o.credentialDone != nil {
		<-o.credentialDone
	}
	if source.retrievals != 1 || len(o.custody.state.Credentials) != 1 {
		t.Fatal("unused route requested credentials")
	}
	credentialNoLeaks(t, o)
}

func TestOpenShellCredentialNativeUnknownKeepsDelivery(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	data, _ := openShellBytes(&pb.SandboxSpec{Providers: []string{"chosen"}})
	o.custody.state.Records = []OpenShellCustodyRecord{{Name: "original", Spec: data, CreateAcknowledged: true}}
	*now = now.Add(55 * time.Minute)
	source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
	o.credentialTick(t.Context())
	if target.updates != 1 {
		t.Fatal("local completion stopped native-unknown token delivery")
	}
	o.custody.state.Records[0].Stopped = &OpenShellStoppedObservation{}
	*now = now.Add(55 * time.Minute)
	source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
	o.credentialTick(t.Context())
	if target.updates != 1 {
		t.Fatal("settled VM kept requesting credentials")
	}
}

func TestOpenShellCredentialShutdownCancelsBlockedDelivery(t *testing.T) {
	o, source, _, now := credentialFixture(t)
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(55 * time.Minute)
	started := make(chan struct{})
	source.blockRetrieval = started
	o.credentialActive = map[string]int{"chosen": 1}
	ctx, cancel := context.WithCancel(t.Context())
	o.credentialCancel = cancel
	done := make(chan struct{})
	go func() { defer close(done); o.credentialTick(ctx) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("source retrieval not reached")
	}
	stopped := make(chan struct{})
	go func() { o.stopCredentials(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for delivery mutex before cancelling RPC")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delivery survived local shutdown")
	}
	if !o.custody.state.Credentials[0].SourceReadPending {
		t.Fatal("cancelled source refresh uncertainty lost")
	}
	credentialNoLeaks(t, o)
}

func TestOpenShellCredentialRunningVMInstallationAndReplacement(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			target.sandbox = custodySandbox("original-vm", "native-original", pb.SandboxPhase_SANDBOX_PHASE_READY)
			target.sandbox.Spec.Providers = []string{"chosen"}
			target.sandbox.Status.MainProcessInstanceId = "original-main"
			data, _ := openShellBytes(target.sandbox.Spec)
			o.custody.state.Records = []OpenShellCustodyRecord{{Name: "original-vm", Spec: data, CreateAcknowledged: true, MainProcessInstanceID: "original-main", Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{SandboxName: "original-vm", ConfigSource: o.custody.state.Binding.ConfigSource, Sandbox: OpenShellSandboxRef{Gateway: o.custody.state.Binding.Gateway, Workspace: "owned-workspace", SandboxID: "native-original"}}}}}
			*now = now.Add(55 * time.Minute)
			source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			if len(o.custody.state.Credentials[0].TargetReceipts) != 1 || o.custody.state.Credentials[0].InstallationObserved {
				t.Fatal("mutation ACK became installation proof")
			}
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("pending supervisor installation accepted")
			}
			target.installationReady = true
			if replacement {
				target.sandbox.Metadata.Id = "replacement-vm"
			}
			err := credentialDeliver(t, o)
			if replacement {
				if err == nil {
					t.Fatal("same-name replacement accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !o.custody.state.Credentials[0].InstallationObserved || target.updates != 1 {
				t.Fatal("same VM did not acknowledge original update")
			}
			credentialNoLeaks(t, o)
		})
	}
}

func TestOpenShellCredentialCustodySchemaFence(t *testing.T) {
	for _, mode := range []string{"standalone", "wrong-source", "downgrade"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, _ := credentialFixture(t)
			target.lost = "create"
			if err := credentialDeliver(t, o); err == nil {
				t.Fatal("lost response expected")
			}
			if o.custody.state.Version != 5 {
				t.Fatal("credential effect did not fence legacy reader")
			}
			binding := o.custody.state.Binding
			if err := o.custody.Close(); err != nil {
				t.Fatal(err)
			}
			owner := filepath.Join(binding.StateRoot, "owner.json")
			if mode == "downgrade" {
				data, err := os.ReadFile(owner)
				if err != nil {
					t.Fatal(err)
				}
				var state OpenShellCustodyState
				if err := json.Unmarshal(data, &state); err != nil {
					t.Fatal(err)
				}
				state.Version = 4
				data, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(owner, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(owner)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "standalone" {
				c, err := OpenOpenShellCustody(t.Context(), binding, o.spec.TLS, nil, filepath.Dir(binding.StateRoot))
				if c != nil {
					_ = c.Close()
				}
				if err == nil {
					t.Fatal("standalone reader acquired credential custody")
				}
			} else {
				selection := *o.spec.CredentialSource
				if mode == "wrong-source" {
					selection.BindingSHA256 = strings.Repeat("b", 64)
				}
				conn, err := newOpenShellConnection(binding.Gateway, o.spec.TLS)
				if err != nil {
					t.Fatal(err)
				}
				c, err := reserveOpenShellCustodyWithCredentials(binding, conn, nil, filepath.Dir(binding.StateRoot), &selection)
				if c != nil {
					_ = c.Close()
				}
				if err == nil {
					t.Fatal("incompatible credential custody reader admitted")
				}
			}
			after, err := os.ReadFile(owner)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || target.creates != 1 || source.retrievals != 1 {
				t.Fatal("refused reader mutated state or native effects")
			}
		})
	}
}

func TestOpenShellCredentialLegacyCustodyUnchanged(t *testing.T) {
	o, source, target, _ := credentialFixture(t)
	binding := o.custody.state.Binding
	path := filepath.Join(binding.StateRoot, "owner.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte("Credentials")) || o.custody.state.Version != 4 {
		t.Fatal("legacy custody changed before credential use")
	}
	if err := o.custody.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := OpenOpenShellCustody(t.Context(), binding, o.spec.TLS, nil, filepath.Dir(binding.StateRoot))
	if err != nil {
		t.Fatal(err)
	}
	o.custody = c
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || source.retrievals != 0 || target.creates != 0 {
		t.Fatal("legacy ordinary read changed state or requested credentials")
	}
	if err := credentialDeliver(t, o); err == nil {
		t.Fatal("standalone custody performed credential effect")
	}
}

func TestOpenShellCredentialRenewalRemovesDeletedSourceConfig(t *testing.T) {
	for _, mode := range []string{"delete-one", "delete-all", "keep-and-add"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			source.provider.Config = map[string]string{"retired": "old-value", "retained": "stable-value"}
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "delete-one":
				delete(source.provider.Config, "retired")
			case "delete-all":
				source.provider.Config = nil
			case "keep-and-add":
				source.provider.Config["added"] = "new-value"
			}
			*now = now.Add(55 * time.Minute)
			source.values["CODEX_AUTH_ACCESS_TOKEN"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
			if err := credentialDeliver(t, o); err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(target.provider.GetConfig(), source.provider.GetConfig()) {
				t.Fatalf("renewal retained deleted config or removed its valid neighbor: target=%v source=%v", target.provider.GetConfig(), source.provider.GetConfig())
			}
			if target.creates != 1 || target.updates != 1 {
				t.Fatal("config synchronization recreated provider or repeated mutation")
			}
			credentialNoLeaks(t, o)
		})
	}
}

func TestOpenShellCredentialExportUsesActualAliasAndOptionalKeys(t *testing.T) {
	for _, mode := range []string{"first-alias", "second-alias", "optional-absent", "required-none"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			source.profile = &pb.ProviderProfile{Id: "alias-profile", Credentials: []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"ACCESS_A", "ACCESS_B"}, Required: true}, {Name: "optional_id", EnvVars: []string{"OPTIONAL_ID"}}}}
			source.provider.Type = source.profile.Id
			source.values = map[string]*pb.ProviderCredentialValue{}
			selected := "ACCESS_A"
			if mode == "second-alias" {
				selected = "ACCESS_B"
			}
			if mode != "required-none" {
				source.values[selected] = &pb.ProviderCredentialValue{Value: "synthetic-alias-value", ExpirationTime: timestamppb.New(now.Add(time.Hour))}
			}
			err := credentialDeliver(t, o)
			if mode == "required-none" {
				var waiting *openShellCredentialWaiting
				if !errors.As(err, &waiting) || source.retrievals != 0 || target.creates != 0 {
					t.Fatalf("missing required alias group was retrieved or not waiting: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(target.provider.Credentials) != 1 || target.provider.Credentials[selected] != "synthetic-alias-value" || len(target.profile.Credentials[0].EnvVars) != 2 || !target.profile.Credentials[0].Required || target.profile.Credentials[1].Required {
				t.Fatal("actual aliases/optional absence changed original group contract")
			}
		})
	}
}

func TestOpenShellCredentialRemovedOptionalKeyClearsValueAndExpiry(t *testing.T) {
	o, source, target, now := credentialFixture(t)
	source.profile = &pb.ProviderProfile{Id: "optional-profile", Credentials: []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"ACCESS"}, Required: true}, {Name: "optional_id", EnvVars: []string{"OPTIONAL_ID"}}}}
	source.provider.Type = source.profile.Id
	source.values = map[string]*pb.ProviderCredentialValue{"ACCESS": {Value: "synthetic-access", ExpirationTime: timestamppb.New(now.Add(time.Hour))}, "OPTIONAL_ID": {Value: "synthetic-optional", ExpirationTime: timestamppb.New(now.Add(time.Hour))}}
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	profileID := target.profile.Id
	delete(source.values, "OPTIONAL_ID")
	*now = now.Add(55 * time.Minute)
	source.values["ACCESS"].ExpirationTime = timestamppb.New(now.Add(time.Hour))
	if err := credentialDeliver(t, o); err != nil {
		t.Fatal(err)
	}
	if _, exists := target.provider.Credentials["OPTIONAL_ID"]; exists {
		t.Fatal("removed source optional credential retained at target")
	}
	if _, exists := target.provider.CredentialExpirationTimes["OPTIONAL_ID"]; exists {
		t.Fatal("removed credential retained expiry")
	}
	if target.profile.Id != profileID || target.creates != 1 || target.updates != 1 || target.provider.Credentials["ACCESS"] != "synthetic-access" {
		t.Fatal("optional removal changed profile identity or required neighbor")
	}
}

func TestOpenShellCredentialSourceKeyBracketAndRefreshOutputs(t *testing.T) {
	for _, mode := range []string{"unowned-added", "unowned-removed", "value-only", "owned-output"} {
		t.Run(mode, func(t *testing.T) {
			o, source, target, now := credentialFixture(t)
			source.profile = &pb.ProviderProfile{Id: "key-bracket", Credentials: []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"ACCESS"}, Required: true}, {Name: "optional", EnvVars: []string{"OPTIONAL"}}}}
			source.provider.Type = source.profile.Id
			source.values = map[string]*pb.ProviderCredentialValue{"ACCESS": {Value: "synthetic-access", ExpirationTime: timestamppb.New(now.Add(time.Hour))}}
			switch mode {
			case "unowned-added":
				source.afterRetrieval = func(s *credentialNative) {
					s.values["OPTIONAL"] = &pb.ProviderCredentialValue{Value: "synthetic-added"}
				}
			case "unowned-removed":
				source.values["OPTIONAL"] = &pb.ProviderCredentialValue{Value: "synthetic-old"}
				source.afterRetrieval = func(s *credentialNative) { delete(s.values, "OPTIONAL") }
			case "value-only":
				source.afterRetrieval = func(s *credentialNative) { s.values["ACCESS"].Value = "synthetic-new-value" }
			case "owned-output":
				source.profile = &pb.ProviderProfile{Id: "aws-output-profile", Credentials: []*pb.ProviderProfileCredential{
					{Name: "access", EnvVars: []string{"AWS_ACCESS_KEY_ID"}, Required: true, Refresh: &pb.ProviderCredentialRefresh{Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_AWS_STS_ASSUME_ROLE, AdditionalOutputs: []*pb.ProviderCredentialRefreshOutput{{Output: "secret_access_key", Credential: "secret"}, {Output: "session_token", Credential: "session"}}}},
					{Name: "secret", EnvVars: []string{"AWS_SECRET_ACCESS_KEY"}, Required: true}, {Name: "session", EnvVars: []string{"AWS_SESSION_TOKEN"}, Required: true},
				}}
				source.provider.Type = source.profile.Id
				source.values = map[string]*pb.ProviderCredentialValue{"AWS_ACCESS_KEY_ID": {Value: "synthetic-aws-access"}, "AWS_SECRET_ACCESS_KEY": {Value: "synthetic-aws-secret"}, "AWS_SESSION_TOKEN": {Value: "synthetic-minted-output"}}
				source.hiddenCredentialKeys = map[string]bool{"AWS_ACCESS_KEY_ID": true, "AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true}
				source.refreshReadback = &pb.GetProviderRefreshStatusResponse{Credentials: []*pb.ProviderCredentialRefreshStatus{{Provider: "chosen", ProviderId: "source-provider", CredentialKey: "AWS_ACCESS_KEY_ID", Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_AWS_STS_ASSUME_ROLE, Status: "configured"}}}
				source.afterRetrieval = func(s *credentialNative) { s.hiddenCredentialKeys = nil }
			}
			err := credentialDeliver(t, o)
			if mode == "unowned-added" || mode == "unowned-removed" {
				var waiting *openShellCredentialWaiting
				if !errors.As(err, &waiting) || target.creates != 0 {
					t.Fatalf("keyset drift was exported or not waiting: %v", err)
				}
				if err := credentialDeliver(t, o); err != nil {
					t.Fatal(err)
				}
				if target.creates != 1 {
					t.Fatal("fresh original source readback did not resume")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "owned-output" && (len(target.provider.Credentials) != 3 || target.provider.Credentials["AWS_SESSION_TOKEN"] != "synthetic-minted-output") {
				t.Fatal("native refresh-owned output was not selected")
			}
			if mode == "value-only" && target.creates != 1 {
				t.Fatal("normal credential value refresh became key drift")
			}
		})
	}
}
