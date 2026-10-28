package admission

import "testing"

func TestVisibleInventoryPreservesUserAndAncestry(t *testing.T) {
	rows, err := parseVisibleProcesses("1 0 1 0 Mon Oct 26 10:00:00 2026 20 /sbin/launchd\n12 1 12 501 Mon Oct 26 10:01:00 2026 30 /usr/bin/python\n13 12 12 501 Mon Oct 26 10:02:00 2026 40 /usr/bin/python\n")
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	if rows[0].UID == nil || *rows[0].UID != 0 || rows[2].UID == nil || *rows[2].UID != 501 {
		t.Fatal(rows)
	}
	if !rows[2].AncestryComplete || len(rows[2].AncestorPIDs) != 2 || rows[2].AncestorPIDs[1] != 1 {
		t.Fatal(rows[2])
	}
}

func TestVisibleInventoryRejectsMissingOrDuplicateIdentity(t *testing.T) {
	for _, data := range []string{"12 1 12 missing Mon Oct 26 10:01:00 2026 30 /usr/bin/python", "12 1 12 501 Mon Oct 26 10:01:00 2026 30 /usr/bin/python\n12 1 12 0 Mon Oct 26 10:01:00 2026 30 /usr/bin/python", "12 1 12 501"} {
		if _, err := parseVisibleProcesses(data); err == nil {
			t.Fatal("invalid visible inventory accepted", data)
		}
	}
}

func TestVisibleInventoryPreservesSignedSystemUID(t *testing.T) {
	rows, err := parseVisibleProcesses("75151 1 75151 -2 Fri Oct 23 12:52:48 2026 1840 /usr/libexec/dhcp6d\n")
	if err != nil || len(rows) != 1 || rows[0].UID == nil || *rows[0].UID != -2 || rows[0].Ownership != "needs_owner_readback" {
		t.Fatalf("signed system UID is observation, never control authority: %+v %v", rows, err)
	}
}
