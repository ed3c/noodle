package admission

import (
	"reflect"
	"testing"
)

func TestResidentInventoryKeepsChatGPTAncestryAndMixedGroupDiagnostic(t *testing.T) {
	data := "1 0 1 Thu Oct 15 12:00:00 2026 100 /sbin/launchd\n" +
		"10 1 10 Thu Oct 15 12:00:01 2026 100 /Applications/ChatGPT.app/Contents/MacOS/ChatGPT\n" +
		"20 10 10 Thu Oct 15 12:00:02 2026 100 /Applications/ChatGPT.app/Contents/Resources/codex\n" +
		"30 20 30 Thu Oct 15 12:00:03 2026 100 /runtime/node\n" +
		"31 30 30 Thu Oct 15 12:00:04 2026 100 /runtime/python\n" +
		"32 20 30 Thu Oct 15 12:00:05 2026 100 /unrelated/service\n"
	rows, err := parseResidents(data)
	if err != nil {
		t.Fatal(err)
	}
	wantAncestors := []int{30, 20, 10, 1}
	if rows[4].ProcessGroup != 30 || !reflect.DeepEqual(rows[4].AncestorPIDs, wantAncestors) || !rows[4].AncestryComplete {
		t.Fatalf("child lost group or parent lineage: %+v", rows[4])
	}
	if rows[5].ProcessGroup != 30 || rows[5].Executable != "/unrelated/service" {
		t.Fatalf("mixed group member disappeared: %+v", rows[5])
	}
	for _, row := range rows {
		if row.Ownership != "needs_owner_readback" {
			t.Fatalf("ancestry or group became signal authority: %+v", row)
		}
	}
}

func TestResidentInventoryMissingAncestorRemainsIncomplete(t *testing.T) {
	rows, err := parseResidents("31 30 30 Thu Oct 15 12:00:04 2026 100 /runtime/python\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows[0].AncestorPIDs, []int{30}) || rows[0].AncestryComplete || rows[0].Ownership != "needs_owner_readback" {
		t.Fatalf("missing parent presented as known: %+v", rows[0])
	}
}

func TestResidentInventoryZeroGroupRemainsDiagnostic(t *testing.T) {
	rows, err := parseResidents("2 0 0 Thu Oct 15 12:00:04 2026 0 /kernel/thread\n")
	if err != nil || len(rows) != 1 || rows[0].ProcessGroup != 0 || rows[0].Ownership != "needs_owner_readback" {
		t.Fatalf("zero group made inventory unavailable or became ownership: %+v %v", rows, err)
	}
}

func TestResidentInventoryRefusesInvalidGroupAndAmbiguousAncestry(t *testing.T) {
	for name, data := range map[string]string{
		"group":     "31 30 bad Thu Oct 15 12:00:04 2026 100 /runtime/python\n",
		"duplicate": "31 30 30 Thu Oct 15 12:00:04 2026 100 /runtime/python\n31 30 30 Thu Oct 15 12:00:04 2026 100 /runtime/python\n",
		"cycle":     "31 30 30 Thu Oct 15 12:00:04 2026 100 /runtime/python\n30 31 30 Thu Oct 15 12:00:03 2026 100 /runtime/node\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResidents(data); err == nil {
				t.Fatal("ambiguous inventory accepted")
			}
		})
	}
}
