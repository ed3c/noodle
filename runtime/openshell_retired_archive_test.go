package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func retiredArchiveFixture(t *testing.T) (*custodyGateway, *OpenShellCustody, OpenShellCustodyBinding, OpenShellIdentity, OpenShellTLS, custodyOpener, SessionReadbackFileRef) {
	t.Helper()
	return retiredArchiveExecutionFixture(t, true)
}

func retiredArchiveExecutionFixture(t *testing.T, executed bool) (*custodyGateway, *OpenShellCustody, OpenShellCustodyBinding, OpenShellIdentity, OpenShellTLS, custodyOpener, SessionReadbackFileRef) {
	t.Helper()
	g, c, b, id, security, open := completedObservationFactoryFixture(t, custodyFixture)
	closure := closureFixture(t, id)
	if !executed {
		c.state.Records[0].Exec = nil
		c.state.Records[0].Worker.ExecResult = OpenShellExecResult{}
		var value OpenShellClosure
		if _, err := ReadSessionReadbackFile(closure.Path, &value); err != nil {
			t.Fatal(err)
		}
		value.Outcome, value.ExecutionBasis = StatusFailed, openShellNotStarted
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(closure.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		closure, err = ReadSessionReadbackFile(closure.Path, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	c.client = retiringClient{OpenShellClient: c.client, delete: func(_ context.Context, _ *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
		g.mutate(func() { delete(g.sandboxes, id.SandboxName) })
		return &pb.DeleteSandboxResponse{SandboxId: id.Sandbox.SandboxID, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
	}}
	if err := c.Retire(t.Context(), id, closure); err != nil {
		t.Fatal(err)
	}
	return g, c, b, id, security, open, closure
}

func TestOpenShellCompletedRetirementReopensWithoutArchivedClosure(t *testing.T) {
	_, c, b, _, security, open, closure := retiredArchiveFixture(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(closure.Path); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(t.Context(), b, security, nil)
	if reopened != nil {
		defer reopened.Close()
	}
	if err != nil {
		t.Fatalf("completed retirement blocked independent work after archive removal: %v", err)
	}
	if _, err := reopened.Create(t.Context(), OpenShellTask{OrderID: "next", AttemptID: "next", SessionID: "next"}, &pb.SandboxSpec{Command: []string{"hold"}}); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Snapshot().Records) != 2 {
		t.Fatal("retired history was discarded")
	}
}

func TestOpenShellMissingRetiredArchiveBlockRecoversAtOriginalOpen(t *testing.T) {
	_, c, b, _, security, open, closure := retiredArchiveFixture(t)
	if err := os.Remove(closure.Path); err != nil {
		t.Fatal(err)
	}
	_, missing := os.Lstat(closure.Path)
	if missing == nil {
		t.Fatal("missing archive not reproduced")
	}
	_ = c.block(missing.Error())
	before, err := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(t.Context(), b, security, nil)
	if reopened != nil {
		defer reopened.Close()
	}
	if err != nil {
		t.Fatalf("exact completed-archive block did not recover through owner: %v", err)
	}
	if reopened.Snapshot().Blocked != "" {
		t.Fatal("old false block survived verified native absence")
	}
	files, err := filepath.Glob(filepath.Join(b.StateRoot, "retired-archive-recovery-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("missing durable recovery history: %v %v", files, err)
	}
	var receipt struct{ Before []byte }
	data, err := os.ReadFile(files[0])
	if err != nil || json.Unmarshal(data, &receipt) != nil || string(receipt.Before) != string(before) {
		t.Fatal("original blocked owner bytes were not retained")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := open(t.Context(), b, security, nil)
	if again != nil {
		defer again.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	filesAfter, _ := filepath.Glob(filepath.Join(b.StateRoot, "retired-archive-recovery-*.json"))
	if !reflect.DeepEqual(files, filesAfter) {
		t.Fatal("reopen created another recovery history")
	}
}

func TestOpenShellRetiredArchiveRecoveryRefusesUnresolvedOrReappearedHistory(t *testing.T) {
	for _, scenario := range []string{"pending", "unknown-ack", "invalid-completion", "unrelated-block", "same-id-reappeared", "foreign-id-reappeared"} {
		t.Run(scenario, func(t *testing.T) {
			g, c, b, id, security, open, closure := retiredArchiveFixture(t)
			if err := os.Remove(closure.Path); err != nil {
				t.Fatal(err)
			}
			_, missing := os.Lstat(closure.Path)
			c.state.Blocked = missing.Error()
			switch scenario {
			case "pending":
				c.state.Records[0].Retirement.Completed = false
			case "unknown-ack":
				c.state.Records[0].Retirement.Completed = false
				c.state.Records[0].Retirement.Acknowledged = false
			case "invalid-completion":
				c.state.Records[0].Retirement.Acknowledged = false
			case "unrelated-block":
				c.state.Blocked = "original native identity changed"
			case "same-id-reappeared", "foreign-id-reappeared":
				g.mutate(func() {
					identifier := id.Sandbox.SandboxID
					if strings.HasPrefix(scenario, "foreign") {
						identifier = "replacement"
					}
					g.sandboxes[id.SandboxName] = custodySandbox(id.SandboxName, identifier, pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
				})
			}
			if err := c.save(); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := open(t.Context(), b, security, nil)
			if reopened != nil {
				defer reopened.Close()
			}
			if err == nil {
				t.Fatal("unresolved or reappeared history admitted new work")
			}
			after, _ := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
			if string(before) != string(after) {
				t.Fatal("failed recovery changed historical owner")
			}
			files, _ := filepath.Glob(filepath.Join(b.StateRoot, "retired-archive-recovery-*.json"))
			if len(files) != 0 {
				t.Fatal("failed recovery published successful recovery history")
			}
		})
	}
}

func TestOpenShellRetiredArchiveRecoveryCannotClearBeforeDurableHistory(t *testing.T) {
	_, c, b, _, security, open, closure := retiredArchiveFixture(t)
	if err := os.Remove(closure.Path); err != nil {
		t.Fatal(err)
	}
	_, missing := os.Lstat(closure.Path)
	_ = c.block(missing.Error())
	before, err := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.StateRoot, fmt.Sprintf("retired-archive-recovery-%x.json", sha256.Sum256(before)))
	if err := os.WriteFile(path, []byte("conflicting original history"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(t.Context(), b, security, nil)
	if reopened != nil {
		defer reopened.Close()
	}
	if err == nil {
		t.Fatal("conflicting recovery proof admitted work")
	}
	after, _ := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	if string(before) != string(after) {
		t.Fatal("blocked owner changed before recovery proof was durable")
	}
}

func TestOpenShellGatewayReservesArchivedRetirementForNativeRecoveryOnly(t *testing.T) {
	for _, recoverable := range []bool{true, false} {
		t.Run(fmt.Sprint(recoverable), func(t *testing.T) {
			g, c, b, _, security, _, closure := retiredArchiveFixture(t)
			if err := os.Remove(closure.Path); err != nil {
				t.Fatal(err)
			}
			_, missing := os.Lstat(closure.Path)
			c.state.Blocked = missing.Error()
			if !recoverable {
				c.state.Blocked = "native identity changed"
			}
			if err := c.save(); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			residents, err := admission.OpenResidents(root)
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			g.mu.Lock()
			beforeGets, beforeLists := g.gets, g.lists
			g.mu.Unlock()
			owner, err := NewOpenShellGateway(residents, admission.NewBudget(1), OpenShellGatewaySpec{
				Binding: b, TLS: security, Version: "test-version", ComputeDriver: "test-driver", ReadinessTimeout: time.Second,
				Resident: admission.ResidentSpec{Name: "gateway", Argv: []string{executable}, Directory: root, Sharing: "shared", Transport: "network"},
			})
			if owner != nil {
				defer owner.Close()
			}
			if (err == nil) != recoverable {
				t.Fatalf("gateway reservation recovery=%v: %v", recoverable, err)
			}
			if recoverable {
				if _, err := owner.custody.Create(t.Context(), OpenShellTask{"new", "new", "new"}, &pb.SandboxSpec{}); err == nil {
					t.Fatal("reservation admitted a worker before native recovery")
				}
			}
			after, _ := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
			if string(before) != string(after) {
				t.Fatal("pre-start reservation cleared native audit block")
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.gets != beforeGets || g.lists != beforeLists {
				t.Fatal("pre-start reservation contacted native gateway")
			}
		})
	}
}

func TestOpenShellRetiredArchiveRecoveryPreservesBlockOnUnknownNativeAbsence(t *testing.T) {
	_, c, b, _, _, _, closure := retiredArchiveFixture(t)
	if err := os.Remove(closure.Path); err != nil {
		t.Fatal(err)
	}
	_, missing := os.Lstat(closure.Path)
	_ = c.block(missing.Error())
	before, _ := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	c.client = retirementDeadlineClient{OpenShellClient: c.client, get: func(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
		return nil, status.Error(codes.Unavailable, "original gateway unavailable")
	}}
	if err := c.initialize(t.Context()); err == nil {
		t.Fatal("unknown native absence admitted work")
	}
	after, _ := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	if string(before) != string(after) {
		t.Fatal("unknown native absence cleared the original block")
	}
	files, _ := filepath.Glob(filepath.Join(b.StateRoot, "retired-archive-recovery-*.json"))
	if len(files) != 0 {
		t.Fatal("unknown native absence published a recovery proof")
	}
}

func TestOpenShellNotStartedRetirementRecoversRemovedArchive(t *testing.T) {
	for _, completed := range []bool{true, false} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			_, c, b, _, security, open, closure := retiredArchiveExecutionFixture(t, false)
			c.state.Records[0].Retirement.Completed = completed
			if err := os.Remove(closure.Path); err != nil {
				t.Fatal(err)
			}
			_, missing := os.Lstat(closure.Path)
			_ = c.block(missing.Error())
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := open(t.Context(), b, security, nil)
			if reopened != nil {
				defer reopened.Close()
			}
			if !completed {
				if err == nil {
					t.Fatal("pending diagnostic retirement lost its original archive requirement")
				}
				return
			}
			if err != nil {
				t.Fatalf("completed not-started retirement depends on removed archive: %v", err)
			}
			if _, err := reopened.Create(t.Context(), OpenShellTask{"next", "next", "next"}, &pb.SandboxSpec{}); err != nil {
				t.Fatal(err)
			}
			if reopened.Snapshot().Records[0].Exec != nil {
				t.Fatal("recovery invented execution")
			}
		})
	}
}
