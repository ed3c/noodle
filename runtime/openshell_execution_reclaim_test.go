package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type reclamationClient struct {
	pb.OpenShellClient
	deleted bool
	deletes int
	id      string
}

func (c *reclamationClient) DeleteSandbox(_ context.Context, r *pb.DeleteSandboxRequest, _ ...grpc.CallOption) (*pb.DeleteSandboxResponse, error) {
	c.deleted = true
	c.deletes++
	return &pb.DeleteSandboxResponse{SandboxId: c.id, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
}
func (c *reclamationClient) GetSandbox(ctx context.Context, r *pb.GetSandboxRequest, opts ...grpc.CallOption) (*pb.SandboxResponse, error) {
	if c.deleted {
		return nil, status.Error(codes.NotFound, "retired")
	}
	return c.OpenShellClient.GetSandbox(ctx, r, opts...)
}
func (c *reclamationClient) ListSandboxes(ctx context.Context, r *pb.ListSandboxesRequest, opts ...grpc.CallOption) (*pb.ListSandboxesResponse, error) {
	if c.deleted {
		return &pb.ListSandboxesResponse{}, nil
	}
	return c.OpenShellClient.ListSandboxes(ctx, r, opts...)
}

func TestOpenShellReclaimKeepsAuditablePublicationAfterCopiesRemoved(t *testing.T) {
	r, b, _ := completedReadbackFixture(t)
	project := filepath.Join(t.TempDir(), "retained-project")
	if out, err := exec.Command("git", "clone", "--quiet", "--no-hardlinks", b.WorktreePath, project).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	custody := r.config.Gateway.custody
	record := custody.Snapshot().Records[0]
	native := &reclamationClient{OpenShellClient: custody.client, id: record.Worker.Identity.Sandbox.SandboxID}
	custody.client = native
	before, err := r.ReadCleanupSession(t.Context(), b, project, StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := filepath.Join(t.TempDir(), "acceptance-evidence.json")
	writeReadbackJSON(t, diagnostic, map[string]bool{"original_integrated_acceptance": true})
	diagnosticRef, err := ReadSessionReadbackFile(diagnostic, nil)
	if err != nil {
		t.Fatal(err)
	}
	authority := originalTaskAuthorityFixture(t, b.OrderID, b.SessionID, before.PublishedHead, diagnosticRef)
	got, err := r.ReclaimCleanupSession(t.Context(), b, project, StatusCompleted, "original owner accepted task", authority)
	if err != nil {
		t.Fatal(err)
	}
	if got.PublishedHead != before.PublishedHead || native.deletes != 1 {
		t.Fatal("reclamation changed publication or repeated delete")
	}
	if before.Retirement != nil || got.Retirement == nil || got.Retirement.ObservationBasis != "historical_stopped_with_fresh_native_absence" || !got.Retirement.DeleteAcknowledged || !got.Retirement.AbsenceObserved || got.Retirement.Closure.Path == "" {
		t.Fatal("retired observation was not distinguished from live completion")
	}
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	for _, name := range []string{"repo", "returned", "delivery/baseline", "delivery/returned", "delivery/patch-repository"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("execution copy retained: %s %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "delivery/compaction.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReclaimCleanupSession(t.Context(), b, project, StatusCompleted, "original owner accepted task", authority); err != nil {
		t.Fatal(err)
	}
	if native.deletes != 1 {
		t.Fatal("native deletion replayed")
	}
	if err := os.RemoveAll(b.WorktreePath); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadCleanupSession(t.Context(), b, project, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	retained, err := RetainReclamationCustody(got, filepath.Join(t.TempDir(), "native-custody.json"))
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(t.TempDir(), "reclaimed.json")
	writeReadbackJSON(t, receiptPath, ExecutionReclamationReceipt{Schema: 1, OrderID: b.OrderID, Authority: authority, Sessions: []ExecutionReclaimedSession{{OrderID: b.OrderID, SessionID: b.SessionID, Readback: retained}}, Worktrees: map[string]string{"original-worker": got.PublishedHead}})
	if _, _, err := ReadExecutionReclamationReceipt(receiptPath, b.OrderID, authority); err != nil {
		t.Fatalf("original retirement could not be read after worktree removal: %v", err)
	}
	// The original raw archives and owner resources are still required.
	archives, err := filepath.Glob(filepath.Join(dir, "progress-archive-*", "*.tar"))
	if err != nil || len(archives) != 2 {
		t.Fatal("raw archives lost")
	}
	saved, err := os.ReadFile(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archives[0], []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadCleanupSession(t.Context(), b, project, StatusCompleted); err == nil {
		t.Fatal("changed evidence accepted after retirement")
	}
	if _, _, err := ReadExecutionReclamationReceipt(receiptPath, b.OrderID, authority); err == nil {
		t.Fatal("original reclamation receipt ignored changed archive evidence")
	}
	if err := os.WriteFile(archives[0], saved, 0600); err != nil {
		t.Fatal(err)
	}
}
