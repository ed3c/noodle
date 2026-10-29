package dispatcher

import "testing"

func TestNativeProcessObservationKeepsDescendantsAndRejectsLiveExit(t *testing.T) {
	root := nativeProcessIdentity{PID: 10, Parent: 1, Started: "original"}
	rows := map[int]nativeProcessIdentity{10: root, 11: {PID: 11, Parent: 10, Started: "child"}, 12: {PID: 12, Parent: 11, Started: "grandchild"}, 13: {PID: 13, Parent: 1, Started: "neighbor"}}
	children, err := nativeProcessDescendants(rows, root)
	if err != nil || len(children) != 2 {
		t.Fatalf("descendant identity lost: %+v %v", children, err)
	}
	if err := nativeObservedProcessesExited(rows, children); err == nil {
		t.Fatal("live descendants reported exited")
	}
	// Reparenting does not change the recorded process's start identity.
	rows[11] = nativeProcessIdentity{PID: 11, Parent: 1, Started: "child"}
	delete(rows, 12)
	if err := nativeObservedProcessesExited(rows, children); err == nil {
		t.Fatal("reparented child reported exited")
	}
	rows[11] = nativeProcessIdentity{PID: 11, Parent: 1, Started: "different-process"}
	if err := nativeObservedProcessesExited(rows, children); err != nil {
		t.Fatal(err)
	}
	if rows[13].Started != "neighbor" {
		t.Fatal("neighbor modified")
	}
	rows[10] = nativeProcessIdentity{PID: 10, Parent: 1, Started: "reused"}
	if _, err := nativeProcessDescendants(rows, root); err == nil {
		t.Fatal("reused root adopted")
	}
}
