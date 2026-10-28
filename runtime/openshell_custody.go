package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const openShellCustodyVersion = 3

// OpenShellCustodyBinding identifies one dedicated gateway deployment. The
// operator must assign exactly one state root per gateway. Its advisory local
// lock cannot exclude external CLI clients or make name-based RPCs atomic.
// ConfigFingerprint is the caller's fingerprint of the explicit configuration;
// no credentials or user configuration files are read by this package.
type OpenShellCustodyBinding struct {
	StateRoot         string
	Gateway           string
	Workspace         string
	Owner             string
	ConfigSource      string
	ConfigFingerprint string
}

type OpenShellRetainedSandbox struct{ Name, SandboxID, Workspace string }
type OpenShellTask struct{ OrderID, AttemptID, SessionID string }

type OpenShellCustodyExec struct {
	AttemptID string
	Command   OpenShellCommand
	Result    OpenShellExecResult
}

// OpenShellStoppedObservation is historical native Get evidence, independent
// of a Stop acknowledgement. It never certifies a new worker's fresh readback.
// The optional driver receipt may disappear during normal stop cleanup.
type OpenShellStoppedObservation struct {
	ComputeInstanceID string
}

// Intent is retained even when the RPC was never sent. Unknown effects and all
// names/UUIDs are permanent history; this API provides no reset, adopt or delete.
type OpenShellCustodyRecord struct {
	Task                  OpenShellTask
	Name                  string
	Retained              bool
	CreateRequestID       string
	CreateIntent          []byte
	CreateAcknowledged    bool
	Spec                  []byte
	MainProcessInstanceID string // gateway-owned nonempty generation; preserved across driver snapshots
	Worker                OpenShellWorkerRecord
	Exec                  *OpenShellCustodyExec
	StopRequestID         string
	StopAcknowledged      bool
	Stopped               *OpenShellStoppedObservation
}

type OpenShellCustodyState struct {
	Version         int
	Binding         OpenShellCustodyBinding
	Blocked         string
	Records         []OpenShellCustodyRecord
	PendingRetained []OpenShellRetainedSandbox
}

// OpenShellCustody serializes local admission and native lifecycle operations.
// Close releases only local streams/lock, and never stops remote compute.
type OpenShellCustody struct {
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	state           OpenShellCustodyState
	lock            *lockfile.Lock
	conn            *grpc.ClientConn
	client          pb.OpenShellClient
	fault           error
	closed          bool
	quiesced        bool
	initialized     bool
	managed         bool // immutable after construction; only gateway owner may close
	retryReadback   bool
	residentRunning func() error
}

// OpenOpenShellCustody returns a non-nil owner on an inventory/binding block.
// Callers must Close it even on error; Snapshot remains available for diagnosis.
// Existing state is read back, never replayed. Repeated retained receipts must
// identify exactly the already-bound history; they never initialize it again.
func OpenOpenShellCustody(ctx context.Context, binding OpenShellCustodyBinding, security OpenShellTLS, retained []OpenShellRetainedSandbox) (*OpenShellCustody, error) {
	for _, value := range []string{binding.StateRoot, binding.Gateway, binding.Workspace, binding.Owner, binding.ConfigSource, binding.ConfigFingerprint} {
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("openshell custody binding missing or invalid")
		}
	}
	conn, err := newOpenShellConnection(binding.Gateway, security)
	if err != nil {
		return nil, err
	}
	return openOpenShellCustody(ctx, binding, conn, retained)
}

// The native connection is already authenticated and belongs to this owner.
func openOpenShellCustody(ctx context.Context, binding OpenShellCustodyBinding, conn *grpc.ClientConn, retained []OpenShellRetainedSandbox) (*OpenShellCustody, error) {
	c, err := reserveOpenShellCustody(binding, conn, retained)
	if err != nil {
		return c, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c, c.initialize(ctx)
}

// Reserve local control and the retained source before any native RPC.
func reserveOpenShellCustody(binding OpenShellCustodyBinding, conn *grpc.ClientConn, retained []OpenShellRetainedSandbox) (*OpenShellCustody, error) {
	fail := func(err error) (*OpenShellCustody, error) { _ = conn.Close(); return nil, err }
	if err := os.MkdirAll(binding.StateRoot, 0700); err != nil {
		return fail(err)
	}
	root, err := filepath.EvalSymlinks(binding.StateRoot)
	if err != nil {
		return fail(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return fail(err)
	}
	binding.StateRoot = root
	if err := filex.SyncDir(filepath.Dir(root)); err != nil {
		return fail(err)
	}
	lock, err := lockfile.TryLock(filepath.Join(root, "owner.lock"))
	if err != nil {
		return fail(err)
	}
	ownerCtx, cancel := context.WithCancel(context.Background())
	c := &OpenShellCustody{lock: lock, conn: conn, client: pb.NewOpenShellClient(conn), ctx: ownerCtx, cancel: cancel}
	data, err := os.ReadFile(filepath.Join(root, "owner.json"))
	if err == nil {
		if err := json.Unmarshal(data, &c.state); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("decode openshell custody: %w", err)
		}
		if c.state.Version != openShellCustodyVersion {
			_ = c.Close()
			return nil, fmt.Errorf("unsupported openshell custody version")
		}
		if c.state.Binding != binding {
			// Always persist through the admitted root, never a path supplied by disk.
			c.state.Binding.StateRoot = root
			return c, c.block("openshell custody configuration/owner binding changed")
		}
		if len(retained) != 0 && !sameOpenShellRetained(c.state, retained) {
			return c, c.block("retained sandbox receipt set changed")
		}
	} else if os.IsNotExist(err) {
		c.state = OpenShellCustodyState{Version: openShellCustodyVersion, Binding: binding, PendingRetained: slices.Clone(retained)}
		// Bind the owner before any inventory failure, including an empty inventory.
		if err := c.save(); err != nil {
			return c, err
		}
	} else {
		_ = c.Close()
		return nil, err
	}
	if err := c.validateLedger(); err != nil {
		return c, c.block(err.Error())
	}
	return c, nil
}

// The persisted pending/initialized history is authoritative. Input order does
// not matter, but duplicates, replacements, additions and removals do.
func sameOpenShellRetained(state OpenShellCustodyState, retained []OpenShellRetainedSandbox) bool {
	expected := make(map[OpenShellRetainedSandbox]bool)
	for _, ref := range state.PendingRetained {
		if expected[ref] {
			return false
		}
		expected[ref] = true
	}
	for _, record := range state.Records {
		if !record.Retained {
			continue
		}
		ref := OpenShellRetainedSandbox{record.Name, record.Worker.Identity.Sandbox.SandboxID, record.Worker.Identity.Sandbox.Workspace}
		if expected[ref] {
			return false
		}
		expected[ref] = true
	}
	if len(expected) != len(retained) {
		return false
	}
	for _, ref := range retained {
		if !expected[ref] {
			return false
		}
		delete(expected, ref)
	}
	return len(expected) == 0
}

// Validate only local ledger structure here; native identity is audited after
// the original child starts. Corrupt receipts never justify a service spawn.
func (c *OpenShellCustody) validateLedger() error {
	names, ids := map[string]bool{}, map[string]bool{}
	for _, r := range c.state.Records {
		if r.Name == "" || names[r.Name] {
			return fmt.Errorf("invalid or duplicate custody ledger name")
		}
		names[r.Name] = true
		if r.Retained || r.CreateAcknowledged {
			id := r.Worker.Identity.Sandbox
			if id.SandboxID == "" || ids[id.SandboxID] || id.Gateway != c.state.Binding.Gateway || id.Workspace != c.state.Binding.Workspace || r.Worker.Identity.SandboxName != r.Name || r.Worker.Identity.ConfigSource != c.state.Binding.ConfigSource {
				return fmt.Errorf("custody ledger native identity/binding invalid")
			}
			ids[id.SandboxID] = true
			if err := proto.Unmarshal(r.Spec, &pb.SandboxSpec{}); err != nil {
				return fmt.Errorf("custody ledger specification invalid: %w", err)
			}
		}
		if !r.Retained {
			var intent pb.CreateSandboxRequest
			if err := proto.Unmarshal(r.CreateIntent, &intent); err != nil {
				return fmt.Errorf("custody ledger create intent invalid: %w", err)
			}
			if r.CreateRequestID == "" || intent.GetName() != r.Name || intent.GetRequestId() != r.CreateRequestID || intent.GetWorkspaceScope().GetWorkspace() != c.state.Binding.Workspace {
				return fmt.Errorf("custody ledger create intent binding invalid")
			}
		}
	}
	for _, ref := range c.state.PendingRetained {
		if ref.Name == "" || ref.SandboxID == "" || ref.Workspace != c.state.Binding.Workspace || names[ref.Name] || ids[ref.SandboxID] {
			return fmt.Errorf("invalid retained sandbox receipt")
		}
		names[ref.Name], ids[ref.SandboxID] = true, true
	}
	return nil
}

// Called under mu. Initialization never replays VM effects. A partial retained
// read keeps its durable source, so a later explicit read cannot accept empty state.
func (c *OpenShellCustody) initialize(ctx context.Context) (err error) {
	c.initialized = true
	defer func() {
		if err != nil {
			c.initialized = false
		}
	}()
	if err = c.available(); err != nil {
		return err
	}
	records := slices.Clone(c.state.Records)
	for _, ref := range c.state.PendingRetained {
		response, readErr := c.client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: ref.Name, WorkspaceScope: c.scope()})
		if readErr != nil {
			return c.readFailure(fmt.Sprintf("read retained sandbox %s", ref.Name), readErr)
		}
		s := response.GetSandbox()
		if !sameOpenShellSandbox(s, ref.Name, ref.SandboxID, c.state.Binding.Workspace) || s.GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED || s.GetSpec() == nil {
			return c.block("retained sandbox identity/Stopped/spec evidence conflicts")
		}
		spec, err := openShellBytes(s.GetSpec())
		if err != nil {
			return c.block(err.Error())
		}
		records = append(records, OpenShellCustodyRecord{Name: ref.Name, Retained: true, Spec: spec, MainProcessInstanceID: s.GetStatus().GetMainProcessInstanceId(), Stopped: openShellStoppedObservation(s), Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{Sandbox: c.ref(ref.SandboxID), SandboxName: ref.Name, ConfigSource: c.state.Binding.ConfigSource}}})
	}
	if len(c.state.PendingRetained) != 0 {
		c.state.Records = records
		c.state.PendingRetained = nil
		if err := c.save(); err != nil {
			return err
		}
	}
	if err := c.audit(ctx); err != nil {
		return err
	}
	// List is not fresh canonical identity evidence for historical records.
	if c.managed {
		for i := range c.state.Records {
			if _, err := c.read(ctx, &c.state.Records[i]); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (c *OpenShellCustody) readFailure(reason string, cause error) error {
	if c.retryReadback {
		// Unknown transport failures cannot prove external drift. They do revoke
		// usable custody until explicit initialization succeeds. Native NotFound
		// confirms a missing bound identity and remains a permanent conflict.
		c.initialized = false
		if status.Code(cause) != codes.NotFound {
			return fmt.Errorf("openshell readback unavailable: %s: %w", reason, cause)
		}
	}
	return c.block(fmt.Sprintf("%s: %v", reason, cause))
}

func (c *OpenShellCustody) scope() *dm.WorkspaceSelector {
	return &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: c.state.Binding.Workspace}}
}
func (c *OpenShellCustody) ref(id string) OpenShellSandboxRef {
	return OpenShellSandboxRef{Gateway: c.state.Binding.Gateway, Workspace: c.state.Binding.Workspace, SandboxID: id}
}
func openShellBytes(m proto.Message) ([]byte, error) {
	return (proto.MarshalOptions{Deterministic: true}).Marshal(m)
}
func sameOpenShellSandbox(s *pb.Sandbox, name, id, workspace string) bool {
	m := s.GetMetadata()
	return id != "" && m.GetId() == id && m.GetName() == name && m.GetWorkspace() == workspace
}

func (c *OpenShellCustody) save() error {
	data, err := json.Marshal(c.state)
	if err == nil {
		err = filex.WriteFileAtomicDurable(filepath.Join(c.state.Binding.StateRoot, "owner.json"), data)
	}
	if err != nil {
		c.fault = fmt.Errorf("persist openshell custody; effects unresolved: %w", err)
		return c.fault
	}
	return nil
}
func (c *OpenShellCustody) block(reason string) error {
	if c.state.Blocked == "" {
		c.state.Blocked = reason
	}
	return errors.Join(fmt.Errorf("openshell custody blocked: %s", c.state.Blocked), c.save())
}
func (c *OpenShellCustody) available() error {
	if c.closed {
		return fmt.Errorf("openshell custody closed")
	}
	if c.managed {
		if c.residentRunning == nil {
			return fmt.Errorf("openshell original resident generation unavailable")
		}
		if err := c.residentRunning(); err != nil {
			return err
		}
	}
	if !c.initialized {
		return fmt.Errorf("openshell custody native initialization incomplete")
	}
	if c.fault != nil {
		return c.fault
	}
	if c.state.Blocked != "" {
		return fmt.Errorf("openshell custody blocked: %s", c.state.Blocked)
	}
	return nil
}

// Snapshot is a detached copy, including command maps and protocol evidence.
func (c *OpenShellCustody) Snapshot() OpenShellCustodyState {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state
	state.Records = slices.Clone(state.Records)
	state.PendingRetained = slices.Clone(state.PendingRetained)
	for i := range state.Records {
		r := &state.Records[i]
		r.Spec = slices.Clone(r.Spec)
		r.CreateIntent = slices.Clone(r.CreateIntent)
		if r.Stopped != nil {
			stopped := *r.Stopped
			r.Stopped = &stopped
		}
		if r.Exec != nil {
			saved := *r.Exec
			saved.Command.Argv = slices.Clone(saved.Command.Argv)
			saved.Command.Env = maps.Clone(saved.Command.Env)
			r.Exec = &saved
		}
	}
	return state
}
func (c *OpenShellCustody) Close() error {
	if c.managed {
		return fmt.Errorf("openshell custody belongs to gateway owner; close original owner")
	}
	return c.closeLocal()
}

func (c *OpenShellCustody) closeLocal() error {
	// Cancel before taking mu: an outstanding native call may hold it. Closing
	// the connection also interrupts unary RPCs; cancellation releases output
	// backpressure on exec streams without implying a remote stop.
	c.cancel()
	connErr := c.conn.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return errors.Join(connErr, c.lock.Close())
}

// audit requests ALL workspaces and every page. A List/Get followed by a
// mutation is observational only: invisible changes between reads are possible.
func (c *OpenShellCustody) audit(ctx context.Context) error {
	if err := c.available(); err != nil {
		return err
	}
	byName := map[string]*OpenShellCustodyRecord{}
	for i := range c.state.Records {
		r := &c.state.Records[i]
		byName[r.Name] = r
	}
	seen := map[string]bool{}
	tokens := map[string]bool{}
	token := ""
	for {
		response, err := c.client.ListSandboxes(ctx, &pb.ListSandboxesRequest{WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_AllWorkspaces{AllWorkspaces: &dm.AllWorkspaces{}}}, PageSize: 100, PageToken: token})
		if err != nil {
			return c.readFailure("inventory unavailable", err)
		}
		for _, s := range response.GetSandboxes() {
			name := s.GetMetadata().GetName()
			r := byName[name]
			if r == nil || seen[name] {
				return c.block(fmt.Sprintf("unregistered or duplicate inventory sandbox %q", name))
			}
			if err := c.matches(r, s); err != nil {
				return err
			}
			seen[name] = true
		}
		token = response.GetNextPageToken()
		if token == "" {
			break
		}
		if tokens[token] {
			return c.block("inventory pagination token repeated")
		}
		tokens[token] = true
	}
	for i := range c.state.Records {
		r := &c.state.Records[i]
		if !r.Retained && !r.CreateAcknowledged {
			return c.block(fmt.Sprintf("create %s acknowledgement unresolved; replay prohibited", r.Name))
		}
		if !seen[r.Name] {
			return c.block(fmt.Sprintf("inventory sandbox %s missing", r.Name))
		}
	}
	return nil
}
func (c *OpenShellCustody) matches(r *OpenShellCustodyRecord, s *pb.Sandbox) error {
	if !sameOpenShellSandbox(s, r.Name, r.Worker.Identity.Sandbox.SandboxID, c.state.Binding.Workspace) {
		return c.block(fmt.Sprintf("sandbox %s native identity changed or unresolved", r.Name))
	}
	spec, err := openShellBytes(s.GetSpec())
	if err != nil || s.GetSpec() == nil || !bytes.Equal(spec, r.Spec) {
		return c.block(fmt.Sprintf("sandbox %s bound specification changed", r.Name))
	}
	status := s.GetStatus()
	if r.MainProcessInstanceID != "" && status.GetMainProcessInstanceId() != r.MainProcessInstanceID {
		return c.block(fmt.Sprintf("sandbox %s native main process instance changed or missing", r.Name))
	}
	if (r.Retained || r.Stopped != nil) && status.GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
		return c.block(fmt.Sprintf("sandbox %s lifecycle changed after observed Stopped: %s", r.Name, status.GetPhase()))
	}
	// A stopped driver may clear its receipt. Keep the old receipt, and reject
	// a different nonempty one; FD and resource-version changes are not identity.
	if r.Stopped != nil && r.Stopped.ComputeInstanceID != "" && status.GetAgentPod() != "" && r.Stopped.ComputeInstanceID != status.GetAgentPod() {
		return c.block(fmt.Sprintf("sandbox %s compute instance changed after observed Stopped", r.Name))
	}
	if r.MainProcessInstanceID == "" {
		if instance := s.GetStatus().GetMainProcessInstanceId(); instance != "" {
			r.MainProcessInstanceID = instance
			return c.save()
		}
	}
	return nil
}
func (c *OpenShellCustody) read(ctx context.Context, r *OpenShellCustodyRecord) (*pb.Sandbox, error) {
	response, err := c.client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: r.Name, WorkspaceScope: c.scope()})
	if err != nil {
		return nil, c.readFailure(fmt.Sprintf("read sandbox %s", r.Name), err)
	}
	s := response.GetSandbox()
	if err := c.matches(r, s); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Only Get supplies new terminal history; neither List nor the Stop RPC ACK
	// substitutes for the worker's fresh Get. An unknown Stop remains pending.
	if s.GetStatus().GetPhase() == pb.SandboxPhase_SANDBOX_PHASE_STOPPED && r.Stopped == nil {
		r.Stopped = openShellStoppedObservation(s)
		if err := c.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func openShellStoppedObservation(s *pb.Sandbox) *OpenShellStoppedObservation {
	status := s.GetStatus()
	return &OpenShellStoppedObservation{ComputeInstanceID: status.GetAgentPod()}
}

func openShellUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
