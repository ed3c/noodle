package runtime

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// OpenShellCredentialSource selects the existing operator-authenticated gateway.
// It supplies runtime values only; its refresh authorization never leaves it.
type OpenShellCredentialSource struct {
	Gateway, Workspace string
	TLS                OpenShellTLS
	BindingSHA256      string
}

// OpenShellCredentialDelivery belongs to the original custody state. Payloads
// are authenticated ciphertext, including bootstrap requests and their IDs.
// A lost response retains the exact request; redacted Get is never an ACK.
type OpenShellCredentialDelivery struct {
	Provider, SourceBinding, SourceProviderID, ProfileID, ProviderID string
	RefreshAt                                                        time.Time
	ExpiresAt                                                        time.Time
	SourceReadPending                                                bool
	Pending                                                          *OpenShellCredentialIntent
	MutationID, LastError                                            string
	TargetReceipts                                                   [][]byte
	RetryAt                                                          time.Time
	InstallationObserved                                             bool
}

type OpenShellCredentialIntent struct {
	Kind, RequestID string
	StartedAt       time.Time
	SealedPayload   []byte
}

func cloneOpenShellCredentialDeliveries(rows []OpenShellCredentialDelivery) []OpenShellCredentialDelivery {
	rows = slices.Clone(rows)
	for i := range rows {
		rows[i].TargetReceipts = slices.Clone(rows[i].TargetReceipts)
		for j := range rows[i].TargetReceipts {
			rows[i].TargetReceipts[j] = slices.Clone(rows[i].TargetReceipts[j])
		}
		if rows[i].Pending != nil {
			v := *rows[i].Pending
			v.SealedPayload = slices.Clone(v.SealedPayload)
			rows[i].Pending = &v
		}
	}
	return rows
}

func validateOpenShellCredentialLedger(rows []OpenShellCredentialDelivery) error {
	seen := make(map[string]bool)
	for _, row := range rows {
		binding, err := hex.DecodeString(row.SourceBinding)
		if row.Provider == "" || strings.TrimSpace(row.Provider) != row.Provider || seen[row.Provider] || err != nil || len(binding) != sha256.Size {
			return fmt.Errorf("openshell credential custody identity invalid")
		}
		seen[row.Provider] = true
		if intent := row.Pending; intent != nil {
			if (intent.Kind != "import" && intent.Kind != "create" && intent.Kind != "update") || intent.RequestID == "" || intent.StartedAt.IsZero() || len(intent.SealedPayload) < 28 || row.ProfileID == "" || row.SourceProviderID == "" {
				return fmt.Errorf("openshell credential custody intent invalid")
			}
		}
	}
	return nil
}

// Readback may retain v5 only when its original profile includes the delivery
// source. An ordinary standalone custody client cannot resume these effects.
func openShellCustodySupportsProfile(state OpenShellCustodyState, spec OpenShellGatewaySpec) bool {
	if state.Version == openShellCustodyVersion {
		return len(state.Credentials) == 0
	}
	if state.Version != openShellCredentialCustodyVersion || spec.CredentialSource == nil || spec.CredentialSource.BindingSHA256 == "" {
		return false
	}
	for _, row := range state.Credentials {
		if row.SourceBinding != spec.CredentialSource.BindingSHA256 {
			return false
		}
	}
	return validateOpenShellCredentialLedger(state.Credentials) == nil
}

func validateOpenShellCredentialSource(s OpenShellCredentialSource, target string) error {
	u, err := url.Parse(s.Gateway)
	if err != nil || u == nil {
		return fmt.Errorf("openshell credential source endpoint invalid")
	}
	ip := net.ParseIP(u.Hostname())
	binding, decodeErr := hex.DecodeString(s.BindingSHA256)
	if u.Scheme != "https" || ip == nil || !ip.IsLoopback() || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || s.Gateway == target || s.Workspace == "" || strings.TrimSpace(s.Workspace) != s.Workspace || strings.ContainsRune(s.Workspace, 0) || decodeErr != nil || len(binding) != sha256.Size {
		return fmt.Errorf("openshell credential source binding invalid")
	}
	return nil
}

func (o *OpenShellGateway) credentialTime() time.Time {
	if o.credentialNow != nil {
		return o.credentialNow()
	}
	return time.Now()
}

// EnsureCredentials prepares only the selected route. The registration covers
// the Create window; persisted native obligations independently keep delivery
// alive after a local stream or session ends. No model call is made here.
func (o *OpenShellGateway) EnsureCredentials(ctx context.Context, providers []string) (func(), error) {
	if o.spec.CredentialSource == nil {
		return func() {}, nil
	}
	c := o.custody
	c.mu.Lock()
	if err := c.mutable(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	err := o.startCredentials()
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	providers = slices.Compact(slices.Sorted(slices.Values(providers)))
	for _, provider := range providers {
		if provider == "" {
			return nil, fmt.Errorf("openshell credential provider selection empty")
		}
		if err := o.deliverCredential(ctx, provider); err != nil {
			return nil, err
		}
	}
	o.credentialMu.Lock()
	if o.credentialActive == nil {
		o.credentialActive = make(map[string]int)
	}
	for _, provider := range providers {
		o.credentialActive[provider]++
	}
	o.credentialMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			o.credentialMu.Lock()
			defer o.credentialMu.Unlock()
			for _, provider := range providers {
				o.credentialActive[provider]--
				if o.credentialActive[provider] == 0 {
					delete(o.credentialActive, provider)
				}
			}
		})
	}, nil
}

// Gateway readback and Ensure call this under custody.mu; no native RPC runs here.
func (o *OpenShellGateway) startCredentials() error {
	o.credentialLifetimeMu.Lock()
	defer o.credentialLifetimeMu.Unlock()
	if o.spec.CredentialSource == nil {
		return nil
	}
	if o.credentialClient == nil {
		conn, err := newOpenShellConnection(o.spec.CredentialSource.Gateway, o.spec.CredentialSource.TLS)
		if err != nil {
			return fmt.Errorf("openshell credential source transport unavailable")
		}
		o.credentialConnection, o.credentialClient = conn, pb.NewOpenShellClient(conn)
	}
	if o.credentialCancel == nil {
		loopCtx, cancel := context.WithCancel(o.custody.ctx)
		o.credentialCancel, o.credentialDone = cancel, make(chan struct{})
		go o.runCredentials(loopCtx, o.credentialDone)
	}
	return nil
}

func (o *OpenShellGateway) runCredentials(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.credentialTick(ctx)
		}
	}
}

func (o *OpenShellGateway) credentialTick(ctx context.Context) {
	o.credentialMu.Lock()
	providers := maps.Clone(o.credentialActive)
	o.credentialMu.Unlock()
	if providers == nil {
		providers = make(map[string]int)
	}
	c := o.custody
	c.mu.Lock()
	if c.mutable() != nil {
		c.mu.Unlock()
		return
	}
	state := c.snapshotLocked()
	c.mu.Unlock()
	for _, record := range state.Records {
		// A local stream ending does not settle native effects.
		settled := record.Stopped != nil && (record.Retained || record.CreateAcknowledged) && !record.Worker.StopPending && (record.StopRequestID == "" || record.StopAcknowledged) && (record.Exec == nil || record.Exec.Result.ExitObserved) && (record.ProgressStop == nil || record.ProgressStop.Result.ExitObserved)
		if settled {
			continue
		}
		var spec pb.SandboxSpec
		if proto.Unmarshal(record.Spec, &spec) != nil {
			continue
		}
		for _, provider := range spec.Providers {
			providers[provider]++
		}
	}
	for _, row := range state.Credentials {
		if row.Pending != nil || row.SourceReadPending {
			providers[row.Provider] += 0
		}
	}
	var batch sync.WaitGroup
	for provider := range providers {
		var row *OpenShellCredentialDelivery
		for i := range state.Credentials {
			if state.Credentials[i].Provider == provider {
				row = &state.Credentials[i]
				break
			}
		}
		if row != nil && (o.credentialTime().Before(row.RetryAt) || (row.Pending == nil && !row.SourceReadPending && row.InstallationObserved && o.credentialTime().Before(row.RefreshAt))) {
			continue
		}
		batch.Go(func() {
			bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			// The original provider row records any error and its pending intent.
			_ = o.advanceCredential(bounded, provider, providers[provider] > 0)
		})
	}
	batch.Wait()
}

// Called after custody quiescence or service closure, while gateway.mu is held.
// Cancellation interrupts local RPCs, and never controls the source gateway.
func (o *OpenShellGateway) stopCredentials() {
	// Do not wait for the delivery mutex before cancelling: an in-flight RPC
	// holds it and must be interrupted before local shutdown waits for custody.
	o.credentialLifetimeMu.Lock()
	if o.credentialCancel != nil {
		o.credentialCancel()
	}
	conn := o.credentialConnection
	o.credentialLifetimeMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// saveCredentialRow merges one detached provider result into the original
// custody. Other providers and VM records can advance while native RPCs wait.
func (o *OpenShellGateway) saveCredentialRow(row *OpenShellCredentialDelivery) error {
	c := o.custody
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("openshell credential custody closed; original intent retained")
	}
	for i := range c.state.Credentials {
		if c.state.Credentials[i].Provider == row.Provider {
			c.state.Credentials[i] = cloneOpenShellCredentialDeliveries([]OpenShellCredentialDelivery{*row})[0]
			return c.save()
		}
	}
	c.state.Credentials = append(c.state.Credentials, cloneOpenShellCredentialDeliveries([]OpenShellCredentialDelivery{*row})[0])
	return c.save()
}

type openShellCredentialWaiting struct{ message string }

func (e *openShellCredentialWaiting) Error() string { return e.message }

func credentialWaiting(message string) error { return &openShellCredentialWaiting{message: message} }

func credentialFailure(operation string, err error) error {
	// Native errors may contain provider-controlled prose or request values.
	code := status.Code(err)
	reason := ""
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			switch info.Reason {
			case "REQUEST_OUTCOME_UNCERTAIN", "REQUEST_ID_PAYLOAD_MISMATCH", "REQUEST_REPLAY_UNAVAILABLE":
				reason = info.Reason
			}
		}
	}
	message := fmt.Sprintf("openshell credential %s unavailable: %s", operation, code)
	if reason != "" {
		message += ": " + reason
	}
	transient := code == codes.Unavailable || code == codes.DeadlineExceeded || code == codes.ResourceExhausted || reason == "REQUEST_OUTCOME_UNCERTAIN"
	if strings.HasPrefix(operation, "source") && (code == codes.Aborted || code == codes.Unauthenticated || code == codes.PermissionDenied || code == codes.FailedPrecondition) {
		transient = true
	}
	if transient {
		return credentialWaiting(message)
	}
	return errors.New(message)
}

func (o *OpenShellGateway) deliverCredential(ctx context.Context, provider string) error {
	return o.advanceCredential(ctx, provider, true)
}

func (o *OpenShellGateway) advanceCredential(ctx context.Context, provider string, allowFresh bool) (result error) {
	// This is only an in-memory same-provider fence. The custody intent remains
	// the durable authority across process loss and lost native replies.
	o.credentialMu.Lock()
	if o.credentialFlights == nil {
		o.credentialFlights = make(map[string]bool)
	}
	if o.credentialFlights[provider] {
		o.credentialMu.Unlock()
		return credentialWaiting("openshell credential provider operation in progress")
	}
	o.credentialFlights[provider] = true
	o.credentialMu.Unlock()
	defer func() { o.credentialMu.Lock(); delete(o.credentialFlights, provider); o.credentialMu.Unlock() }()
	c := o.custody
	c.mu.Lock()
	if err := c.mutable(); err != nil {
		c.mu.Unlock()
		return err
	}
	if c.credentialSourceBinding == "" || c.credentialSourceBinding != o.spec.CredentialSource.BindingSHA256 {
		c.mu.Unlock()
		return fmt.Errorf("openshell credential source-capable custody owner unavailable")
	}
	if c.state.Version != openShellCredentialCustodyVersion {
		if c.state.Version != openShellCustodyVersion || len(c.state.Credentials) != 0 {
			c.mu.Unlock()
			return fmt.Errorf("openshell credential custody upgrade source invalid")
		}
		c.state.Version = openShellCredentialCustodyVersion
		if err := c.save(); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	r := &OpenShellCredentialDelivery{Provider: provider, SourceBinding: o.spec.CredentialSource.BindingSHA256}
	for _, existing := range c.state.Credentials {
		if existing.Provider == provider {
			copy := cloneOpenShellCredentialDeliveries([]OpenShellCredentialDelivery{existing})[0]
			r = &copy
			break
		}
	}
	c.credentialInFlight++
	c.mu.Unlock()
	defer func() {
		if result != nil {
			r.LastError = result.Error()
			r.RetryAt = o.credentialTime().Add(10 * time.Second)
		} else {
			r.LastError = ""
			r.RetryAt = time.Time{}
		}
		if err := o.saveCredentialRow(r); err != nil {
			result = err
		}
		c.mu.Lock()
		c.credentialInFlight--
		c.mu.Unlock()
	}()
	if !allowFresh {
		return o.recoverCredentialRow(ctx, r)
	}
	return o.deliverCredentialRow(ctx, r)
}

func (o *OpenShellGateway) deliverCredentialRow(ctx context.Context, r *OpenShellCredentialDelivery) error {
	provider := r.Provider
	if r.SourceBinding != o.spec.CredentialSource.BindingSHA256 {
		return fmt.Errorf("openshell credential source identity changed")
	}
	if r.Pending != nil {
		if err := o.replayCredential(ctx, r); err != nil {
			return err
		}
	}
	// An acknowledged update whose credential has since expired may need a
	// fresh update to repair installation. Its old receipt must not prevent
	// renewal; the native ACK still has to be recovered before reaching here.
	if !r.InstallationObserved && o.credentialTime().Before(r.RefreshAt) {
		if err := o.readCredentialReceipts(ctx, r); err != nil {
			return err
		}
	}
	if o.credentialTime().Before(r.RefreshAt) {
		return nil
	}
	source := o.spec.CredentialSource
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: source.Workspace}}
	response, err := o.credentialClient.GetProvider(ctx, &pb.GetProviderRequest{WorkspaceScope: scope, Name: provider})
	if err != nil {
		return credentialFailure("source provider", err)
	}
	p := response.GetProvider()
	if p.GetMetadata().GetId() == "" || p.GetMetadata().GetName() != provider || p.GetMetadata().GetWorkspace() != source.Workspace {
		return fmt.Errorf("openshell credential source provider identity invalid")
	}
	if r.SourceProviderID != "" && r.SourceProviderID != p.GetMetadata().GetId() {
		return fmt.Errorf("openshell credential source provider replaced")
	}
	r.SourceProviderID = p.GetMetadata().GetId()
	profileScope := (*dm.WorkspaceSelector)(nil)
	if p.GetProfileWorkspace() != "" {
		profileScope = &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: p.GetProfileWorkspace()}}
	}
	profileResponse, err := o.credentialClient.GetProviderProfile(ctx, &pb.GetProviderProfileRequest{WorkspaceScope: profileScope, Id: p.GetType()})
	if err != nil {
		return credentialFailure("source profile", err)
	}
	profile, _, err := externalOpenShellProfile(profileResponse.GetProfile())
	if err != nil {
		return err
	}
	if r.ProfileID != "" && r.ProfileID != profile.Id {
		return fmt.Errorf("openshell credential source profile changed")
	}
	var refreshReadback *pb.GetProviderRefreshStatusResponse
	if r.SourceReadPending || credentialProfileHasRefresh(profileResponse.GetProfile()) {
		refreshReadback, err = o.credentialClient.GetProviderRefreshStatus(ctx, &pb.GetProviderRefreshStatusRequest{WorkspaceScope: scope, Provider: provider})
		if err != nil {
			return credentialFailure("source refresh readback", err)
		}
	}
	keys, ownedKeys, err := credentialSourceKeys(profileResponse.GetProfile(), p, refreshReadback)
	if err != nil {
		return err
	}
	if r.SourceReadPending {
		if err := credentialSourceReadbackSettled(profileResponse.GetProfile(), keys, provider, r.SourceProviderID, refreshReadback, o.credentialTime()); err != nil {
			return err
		}
	}
	r.ProfileID = profile.Id
	if err := o.ensureCredentialProfile(ctx, r, profile); err != nil {
		return err
	}
	r.SourceReadPending = true
	if err := o.saveCredentialRow(r); err != nil {
		return err
	}
	values, err := o.credentialClient.GetProviderCredentials(ctx, &pb.GetProviderCredentialsRequest{WorkspaceScope: scope, Name: provider, CredentialKeys: keys, MinimumRemainingLifetime: durationpb.New(5 * time.Minute)})
	if err != nil {
		return credentialFailure("source retrieval", err)
	}
	credentials := make(map[string]string)
	expiration := make(map[string]*timestamppb.Timestamp)
	var expires time.Time
	for _, key := range keys {
		value := values.GetCredentials()[key]
		if value == nil || value.GetValue() == "" {
			return fmt.Errorf("openshell credential source response incomplete")
		}
		credentials[key] = value.GetValue()
		if ts := value.GetExpirationTime(); ts != nil {
			if ts.CheckValid() != nil || !ts.AsTime().After(o.credentialTime().Add(4*time.Minute)) {
				return fmt.Errorf("openshell credential source expiration insufficient")
			}
			expiration[key] = ts
			if expires.IsZero() || ts.AsTime().Before(expires) {
				expires = ts.AsTime()
			}
		}
	}
	// Retrieval has no expected-provider-ID input. Bracket it with the original
	// non-secret identity/config so a same-name replacement cannot be relabeled.
	readback, err := o.credentialClient.GetProvider(ctx, &pb.GetProviderRequest{WorkspaceScope: scope, Name: provider})
	if err != nil {
		return credentialFailure("source identity readback", err)
	}
	current := readback.GetProvider()
	if current.GetMetadata().GetId() != r.SourceProviderID || current.GetType() != p.GetType() || current.GetProfileWorkspace() != p.GetProfileWorkspace() || !maps.Equal(current.GetConfig(), p.GetConfig()) {
		return fmt.Errorf("openshell credential source changed during retrieval")
	}
	if !credentialSourceKeySnapshotMatches(p, current, ownedKeys) {
		return credentialWaiting("openshell credential source key selection changed during retrieval")
	}
	target, err := o.custody.client.GetProvider(ctx, &pb.GetProviderRequest{WorkspaceScope: o.custody.scope(), Name: provider})
	create := status.Code(err) == codes.NotFound
	if err != nil && !create {
		return credentialFailure("target provider", err)
	}
	id, err := openShellUUID()
	if err != nil {
		return err
	}
	material := &dm.Provider{Metadata: &dm.ObjectMeta{Name: provider}, Type: profile.Id, ProfileWorkspace: o.custody.state.Binding.Workspace, Credentials: credentials, Config: maps.Clone(p.GetConfig()), CredentialExpirationTimes: expiration}
	var request proto.Message
	kind := "create"
	if create {
		if r.ProviderID != "" {
			return fmt.Errorf("openshell credential target provider disappeared")
		}
		request = &pb.CreateProviderRequest{WorkspaceScope: o.custody.scope(), Provider: material, RequestId: id}
	} else {
		native := target.GetProvider()
		if r.ProviderID == "" || native.GetMetadata().GetId() != r.ProviderID || native.GetType() != profile.Id || native.GetProfileWorkspace() != o.custody.state.Binding.Workspace {
			return fmt.Errorf("openshell credential target provider ownership changed")
		}
		// Native Update merges config and retains omitted keys. Express source
		// deletions with its existing empty-string marker in this same CAS intent.
		if material.Config == nil {
			material.Config = make(map[string]string)
		}
		for key := range native.GetConfig() {
			if _, exists := p.GetConfig()[key]; !exists {
				material.Config[key] = ""
			}
		}
		material.Metadata.ResourceVersion = native.GetMetadata().GetResourceVersion()
		kind = "update"
		var clearExpiration []string
		for key := range native.GetCredentials() {
			if _, exists := credentials[key]; !exists {
				material.Credentials[key] = ""
				if native.GetCredentialExpirationTimes()[key] != nil {
					clearExpiration = append(clearExpiration, key)
				}
			}
		}
		for _, key := range keys {
			if native.GetCredentialExpirationTimes()[key] != nil && expiration[key] == nil {
				clearExpiration = append(clearExpiration, key)
			}
		}
		material.CredentialExpirationTimes = nil
		request = &pb.UpdateProviderRequest{WorkspaceScope: o.custody.scope(), Provider: material, RequestId: id, CredentialExpirationTimes: expiration, ClearCredentialExpirationKeys: clearExpiration}
	}
	r.ExpiresAt = expires
	r.RefreshAt = o.credentialTime().Add(15 * time.Minute)
	if !expires.IsZero() {
		r.RefreshAt = expires.Add(-6 * time.Minute)
		if !r.RefreshAt.After(o.credentialTime()) {
			r.RefreshAt = o.credentialTime().Add(30 * time.Second)
		}
	}
	r.SourceReadPending = false
	if err := o.saveCredentialIntent(r, kind, id, request); err != nil {
		return err
	}
	return o.replayCredential(ctx, r)
}

func externalOpenShellProfile(original *pb.ProviderProfile) (*pb.ProviderProfile, []string, error) {
	if original == nil || original.GetId() == "" {
		return nil, nil, fmt.Errorf("openshell source credential profile absent")
	}
	p := proto.Clone(original).(*pb.ProviderProfile)
	p.ResourceVersion, p.Source, p.Scope = 0, "", ""
	p.Annotations = nil
	var keys []string
	for _, credential := range p.Credentials {
		if len(credential.EnvVars) == 0 || credential.TokenGrant != nil {
			return nil, nil, fmt.Errorf("openshell source profile credential is not runtime-exportable")
		}
		for _, key := range credential.EnvVars {
			if strings.Contains(strings.ToUpper(key), "REFRESH_TOKEN") {
				return nil, nil, fmt.Errorf("openshell refresh authorization export refused")
			}
			keys = append(keys, key)
		}
		credential.Refresh, credential.TokenGrant = nil, nil
		credential.StablePlaceholder = true
	}
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("openshell runtime credential selection invalid")
	}
	bytes, err := openShellBytes(p)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(bytes)
	p.Id = "noodle-external-" + hex.EncodeToString(sum[:16])
	return p, keys, nil
}

func (o *OpenShellGateway) ensureCredentialProfile(ctx context.Context, r *OpenShellCredentialDelivery, expected *pb.ProviderProfile) error {
	o.credentialMu.Lock()
	key := "profile:" + expected.Id
	if o.credentialFlights[key] {
		o.credentialMu.Unlock()
		return credentialWaiting("openshell external credential profile preparation in progress")
	}
	o.credentialFlights[key] = true
	o.credentialMu.Unlock()
	defer func() { o.credentialMu.Lock(); delete(o.credentialFlights, key); o.credentialMu.Unlock() }()
	response, err := o.custody.client.GetProviderProfile(ctx, &pb.GetProviderProfileRequest{WorkspaceScope: o.custody.scope(), Id: expected.Id})
	if err == nil {
		if response.GetProfile() == nil {
			return fmt.Errorf("openshell target credential profile absent")
		}
		observed := proto.Clone(response.GetProfile()).(*pb.ProviderProfile)
		observed.ResourceVersion, observed.Source, observed.Scope = 0, "", ""
		if !proto.Equal(observed, expected) {
			return fmt.Errorf("openshell external credential profile drifted")
		}
		return nil
	}
	if status.Code(err) != codes.NotFound {
		return credentialFailure("target profile", err)
	}
	id, err := openShellUUID()
	if err != nil {
		return err
	}
	request := &pb.ImportProviderProfilesRequest{WorkspaceScope: o.custody.scope(), RequestId: id, Profiles: []*pb.ProviderProfileImportItem{{Profile: expected}}}
	if err := o.saveCredentialIntent(r, "import", id, request); err != nil {
		return err
	}
	return o.replayCredential(ctx, r)
}

func (o *OpenShellGateway) credentialCipher() (cipher.AEAD, error) {
	private, err := x509.MarshalPKCS8PrivateKey(o.spec.TLS.Certificate.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("openshell credential replay key unavailable")
	}
	key, err := hkdf.Key(sha256.New, private, nil, "noodle/openshell/custody-credential-replay/v1", 32)
	clear(private)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (o *OpenShellGateway) credentialAAD(r *OpenShellCredentialDelivery, intent *OpenShellCredentialIntent) []byte {
	data, _ := json.Marshal(struct {
		Binding                           OpenShellCustodyBinding
		Source, Provider, Kind, RequestID string
		StartedAt, ExpiresAt              time.Time
	}{o.custody.state.Binding, r.SourceBinding, r.Provider, intent.Kind, intent.RequestID, intent.StartedAt, r.ExpiresAt})
	return data
}

func (o *OpenShellGateway) saveCredentialIntent(r *OpenShellCredentialDelivery, kind, id string, request proto.Message) error {
	if r.Pending != nil {
		return fmt.Errorf("openshell credential operation unresolved")
	}
	plain, err := openShellBytes(request)
	if err != nil {
		return err
	}
	defer clear(plain)
	aead, err := o.credentialCipher()
	if err != nil {
		return err
	}
	intent := &OpenShellCredentialIntent{Kind: kind, RequestID: id, StartedAt: o.credentialTime()}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	intent.SealedPayload = aead.Seal(nonce, nonce, plain, o.credentialAAD(r, intent))
	r.Pending = intent
	return o.saveCredentialRow(r)
}

func (o *OpenShellGateway) replayCredential(ctx context.Context, r *OpenShellCredentialDelivery) error {
	intent := r.Pending
	if intent == nil {
		return nil
	}
	if age := o.credentialTime().Sub(intent.StartedAt); age < 0 || age >= 24*time.Hour {
		return fmt.Errorf("openshell credential operation outside native replay window; original owner reconciliation required")
	}
	aead, err := o.credentialCipher()
	if err != nil {
		return err
	}
	if len(intent.SealedPayload) < aead.NonceSize() {
		return fmt.Errorf("openshell credential replay payload invalid")
	}
	plain, err := aead.Open(nil, intent.SealedPayload[:aead.NonceSize()], intent.SealedPayload[aead.NonceSize():], o.credentialAAD(r, intent))
	if err != nil {
		return fmt.Errorf("openshell credential replay authentication failed")
	}
	defer clear(plain)
	var response *pb.ProviderResponse
	switch intent.Kind {
	case "import":
		var request pb.ImportProviderProfilesRequest
		if proto.Unmarshal(plain, &request) != nil || request.RequestId != intent.RequestID || request.WorkspaceScope.GetWorkspace() != o.custody.state.Binding.Workspace || len(request.Profiles) != 1 || request.Profiles[0].GetProfile().GetId() != r.ProfileID {
			return fmt.Errorf("openshell credential import intent invalid")
		}
		result, callErr := o.custody.client.ImportProviderProfiles(ctx, &request)
		if callErr != nil {
			return credentialFailure("profile import", callErr)
		}
		if !result.GetImported() {
			return fmt.Errorf("openshell credential profile import rejected")
		}
		if len(result.GetProfiles()) != 1 || result.GetProfiles()[0] == nil {
			return fmt.Errorf("openshell credential profile import receipt incomplete")
		}
		observed := proto.Clone(result.Profiles[0]).(*pb.ProviderProfile)
		observed.ResourceVersion, observed.Source, observed.Scope = 0, "", ""
		if !proto.Equal(observed, request.Profiles[0].Profile) {
			return fmt.Errorf("openshell credential profile import receipt differs")
		}
	case "create":
		var request pb.CreateProviderRequest
		if proto.Unmarshal(plain, &request) != nil || request.RequestId != intent.RequestID || request.WorkspaceScope.GetWorkspace() != o.custody.state.Binding.Workspace || request.Provider.GetMetadata().GetName() != r.Provider || request.Provider.GetType() != r.ProfileID {
			return fmt.Errorf("openshell credential create intent invalid")
		}
		response, err = o.custody.client.CreateProvider(ctx, &request)
	case "update":
		var request pb.UpdateProviderRequest
		if proto.Unmarshal(plain, &request) != nil || request.RequestId != intent.RequestID || request.WorkspaceScope.GetWorkspace() != o.custody.state.Binding.Workspace || request.Provider.GetMetadata().GetName() != r.Provider || request.Provider.GetType() != r.ProfileID {
			return fmt.Errorf("openshell credential update intent invalid")
		}
		response, err = o.custody.client.UpdateProvider(ctx, &request)
	default:
		return fmt.Errorf("openshell credential intent kind invalid")
	}
	if err != nil {
		return credentialFailure("target mutation", err)
	}
	if response != nil {
		p := response.GetProvider()
		if p.GetMetadata().GetId() == "" || p.GetMetadata().GetName() != r.Provider || p.GetType() != r.ProfileID || p.GetMetadata().GetWorkspace() != o.custody.state.Binding.Workspace || (r.ProviderID != "" && r.ProviderID != p.GetMetadata().GetId()) {
			return fmt.Errorf("openshell credential mutation identity conflicts")
		}
		r.ProviderID = p.GetMetadata().GetId()
		if intent.Kind == "update" && response.GetMutationId() == "" {
			return fmt.Errorf("openshell credential update mutation receipt absent")
		}
		r.MutationID = response.GetMutationId()
		r.TargetReceipts = nil
		r.InstallationObserved = false
		for _, receipt := range response.GetTargetReceipts() {
			if receipt.GetReceiptId() == "" || receipt.GetProvider() != r.Provider || receipt.GetWorkspace() != o.custody.state.Binding.Workspace || receipt.GetMutationId() != r.MutationID || receipt.GetDesired().GetProviderId() != r.ProviderID {
				return fmt.Errorf("openshell credential update target receipt invalid")
			}
			data, err := openShellBytes(receipt)
			if err != nil {
				return err
			}
			r.TargetReceipts = append(r.TargetReceipts, data)
		}
	}
	r.Pending = nil
	return o.saveCredentialRow(r)
}

func (o *OpenShellGateway) readCredentialReceipts(ctx context.Context, r *OpenShellCredentialDelivery) error {
	for _, data := range r.TargetReceipts {
		var receipt pb.ProviderMutationReceipt
		if proto.Unmarshal(data, &receipt) != nil {
			return fmt.Errorf("openshell credential saved receipt invalid")
		}
		desired := receipt.GetDesired()
		original, err := o.credentialSandboxReadback(ctx, desired)
		if err != nil {
			return err
		}
		if original.Stopped != nil && !original.Worker.StopPending && (original.StopRequestID == "" || original.StopAcknowledged) {
			continue
		}
		response, err := o.custody.client.GetSandboxProviderStatus(ctx, &pb.GetSandboxProviderStatusRequest{WorkspaceScope: o.custody.scope(), Sandbox: original.Name, Provider: r.Provider, ReceiptId: receipt.ReceiptId})
		if err != nil {
			return credentialFailure("target installation", err)
		}
		observed := response.GetStatus()
		if !proto.Equal(observed.GetReceipt(), &receipt) {
			return fmt.Errorf("openshell credential target receipt changed")
		}
		if _, err := o.credentialSandboxReadback(ctx, desired); err != nil {
			return err
		}
		if observed.GetNetworkInstanceId() != "" && observed.GetNetworkInstanceId() != original.MainProcessInstanceID {
			return fmt.Errorf("openshell credential target network identity changed")
		}
		if observed.GetState() == pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PERSISTED || observed.GetState() == pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PENDING {
			return credentialWaiting("openshell credential target installation pending on original receipt")
		}
		installed := observed.GetObserved()
		if observed.GetState() != pb.ProviderReadinessState_PROVIDER_READINESS_STATE_READY || observed.GetNetworkInstanceId() != original.MainProcessInstanceID || original.MainProcessInstanceID == "" || installed.GetSessionId() == "" || installed.GetSequence() == 0 || installed.GetProcessInstanceId() == "" || !installed.GetCredentialsInstalled() || !installed.GetLaunchEnvironmentInstalled() || !installed.GetPolicyActive() || installed.GetAttachmentEpoch() != desired.GetAttachmentEpoch() || installed.GetProviderEnvRevision() != desired.GetProviderEnvRevision() || installed.GetConfigRevision() != desired.GetConfigRevision() || installed.GetPolicyHash() != desired.GetPolicyHash() {
			return fmt.Errorf("openshell credential target installation not ready")
		}
	}
	r.InstallationObserved = true
	return o.saveCredentialRow(r)
}

// Network observation runs outside custody.mu; only validation and any newly
// observed native identity/Stopped receipt use its brief original-owner lock.
func (o *OpenShellGateway) credentialSandboxReadback(ctx context.Context, desired *pb.ProviderDesiredIdentity) (OpenShellCustodyRecord, error) {
	c := o.custody
	response, err := c.client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: desired.GetSandbox(), WorkspaceScope: c.scope()})
	if err != nil {
		return OpenShellCustodyRecord{}, credentialFailure("target sandbox readback", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.available(); err != nil {
		return OpenShellCustodyRecord{}, err
	}
	for i := range c.state.Records {
		r := &c.state.Records[i]
		if r.Worker.Identity.Sandbox.SandboxID != desired.GetSandboxId() || r.Name != desired.GetSandbox() {
			continue
		}
		if err := c.matches(r, response.GetSandbox()); err != nil {
			return OpenShellCustodyRecord{}, err
		}
		if response.GetSandbox().GetStatus().GetPhase() == pb.SandboxPhase_SANDBOX_PHASE_STOPPED && r.Stopped == nil {
			r.Stopped = openShellStoppedObservation(response.GetSandbox())
			if err := c.save(); err != nil {
				return OpenShellCustodyRecord{}, err
			}
		}
		return *r, nil
	}
	return OpenShellCustodyRecord{}, fmt.Errorf("openshell credential receipt names unowned sandbox")
}
