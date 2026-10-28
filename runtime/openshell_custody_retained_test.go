package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

func TestOpenShellCustodyRetainedReopenIsIdempotent(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		for _, change := range []string{"same", "reordered", "omitted", "duplicate", "replace", "add", "drop"} {
			t.Run(map[bool]string{false: "pending", true: "initialized"}[initialized]+"/"+change, func(t *testing.T) {
				g, b, security, open := custodyFixture(t)
				refs := []OpenShellRetainedSandbox{{"one", "one-id", b.Workspace}, {"two", "two-id", b.Workspace}}
				g.mutate(func() {
					for _, r := range refs {
						g.sandboxes[r.Name] = custodySandbox(r.Name, r.SandboxID, pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
					}
				})
				conn, err := newOpenShellConnection(b.Gateway, security)
				if err != nil {
					t.Fatal(err)
				}
				c, err := reserveOpenShellCustody(b, conn, refs)
				if err != nil {
					t.Fatal(err)
				}
				if initialized {
					if err = c.initialize(t.Context()); err != nil {
						t.Fatal(err)
					}
					custodyCreate(t, c, "worker")
				}
				before := c.Snapshot()
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				changed := slices.Clone(refs)
				switch change {
				case "reordered":
					slices.Reverse(changed)
				case "omitted":
					changed = nil
				case "duplicate":
					changed[1] = changed[0]
				case "replace":
					changed[1].SandboxID = "replacement"
				case "add":
					changed = append(changed, OpenShellRetainedSandbox{"third", "third-id", b.Workspace})
				case "drop":
					changed = changed[:1]
				}
				creates, stops, execs := g.counts()
				conn, err = newOpenShellConnection(b.Gateway, security)
				if err != nil {
					t.Fatal(err)
				}
				reopened, err := reserveOpenShellCustody(b, conn, changed)
				if reopened != nil {
					defer reopened.Close()
				}
				valid := change == "same" || change == "reordered" || change == "omitted"
				if valid {
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, reopened.Snapshot()) {
						t.Fatal("reopen rewrote authoritative history")
					}
					if err = reopened.Close(); err != nil {
						t.Fatal(err)
					}
					reopened, err = open(t.Context(), b, security, changed)
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close()
					if len(reopened.Snapshot().PendingRetained) != 0 {
						t.Fatal("native readback did not initialize pending receipts")
					}
				} else if err == nil || reopened.Snapshot().Blocked == "" {
					t.Fatal("retained modification was accepted")
				}
				if a, b, c := g.counts(); a != creates || b != stops || c != execs {
					t.Fatal("reopen replayed native mutation")
				}
			})
		}
	}
}

func TestOpenShellCustodySameRetainedDoesNotClearExistingBlock(t *testing.T) {
	_, b, security, _ := custodyFixture(t)
	refs := []OpenShellRetainedSandbox{{"one", "one-id", b.Workspace}}
	conn, err := newOpenShellConnection(b.Gateway, security)
	if err != nil {
		t.Fatal(err)
	}
	c, err := reserveOpenShellCustody(b, conn, refs)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.block("retained initialization supplied for existing custody")
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err = newOpenShellConnection(b.Gateway, security)
	if err != nil {
		t.Fatal(err)
	}
	c, err = reserveOpenShellCustody(b, conn, refs)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	after, err := os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || c.Snapshot().Blocked == "" {
		t.Fatal("reopen cleared existing block")
	}
}
