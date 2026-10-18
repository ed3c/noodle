package loopview

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func testRun(id string) Run {
	return Run{ID: id, Repository: "org/repo", Mode: "outer-only", Status: "unknown", Stages: []Stage{}, Cards: []Card{}, Notes: []Note{}, Breakpoints: []Breakpoint{}}
}

func TestImportMergeRefreshAndIdentityRefusal(t *testing.T) {
	dir := t.TempDir()
	a := testRun("run-a")
	changed, count, err := Import(dir, View{SchemaVersion: 1, Runs: []Run{a}})
	if err != nil || !changed || count != 1 {
		t.Fatalf("first import: %v %d %v", changed, count, err)
	}
	changed, count, err = Import(dir, View{SchemaVersion: 1, Runs: []Run{a}})
	if err != nil || changed || count != 1 {
		t.Fatalf("repeat import: %v %d %v", changed, count, err)
	}
	b := testRun("run-b")
	if _, _, err = Import(dir, View{SchemaVersion: 1, Runs: []Run{b}}); err != nil {
		t.Fatal(err)
	}
	a.Status = "prepared"
	if _, _, err = Import(dir, View{SchemaVersion: 1, Runs: []Run{a}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	a.Repository = "other/repo"
	if _, _, err = Import(dir, View{SchemaVersion: 1, Runs: []Run{a}}); err == nil {
		t.Fatal("reassigned run accepted")
	}
	after, _ := os.ReadFile(Path(dir))
	if string(before) != string(after) {
		t.Fatal("refusal modified registry")
	}
	view, err := Load(dir)
	if err != nil || len(view.Runs) != 2 || view.Runs[0].Status != "prepared" {
		t.Fatalf("readback: %+v %v", view, err)
	}
}

func TestDecodeRejectsMalformedProjection(t *testing.T) {
	for _, input := range []string{
		`{"schema_version":2,"runs":[]}`,
		`{"schema_version":1,"runs":null}`,
		`{"schema_version":1,"runs":[],"execute":"anything"}`,
		`{"schema_version":1,"runs":[]} {}`,
		`{"schema_version":1,"runs":[{"id":"a","repository":"repo","mode":"outer-only","status":"unknown","next":{}}]}`,
		strings.Repeat(" ", MaxBytes+1),
	} {
		if _, err := Decode([]byte(input)); err == nil {
			t.Fatalf("accepted malformed input: %.120s", input)
		}
	}
	run := testRun("a")
	run.Breakpoints = []Breakpoint{{StageID: "missing"}}
	data, _ := json.Marshal(View{SchemaVersion: 1, Runs: []Run{run}})
	if _, err := Decode(data); err == nil {
		t.Fatal("unknown breakpoint stage accepted")
	}
}

func TestImportDoesNotRepairCorruptRegistryOrInventConsumption(t *testing.T) {
	dir := t.TempDir()
	run := testRun("a")
	run.Stages = []Stage{{ID: "s", Kind: "dispatch", Status: "transmitted", Prompt: "<script>not executed</script>", ConsumerStatus: "unknown", InputRefs: []string{"../../private"}, OutputRefs: []string{}}}
	data, _ := json.Marshal(View{SchemaVersion: 1, Runs: []Run{run}})
	view, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Import(dir, view); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runs[0].Stages[0].ConsumerStatus != "unknown" || got.Runs[0].Stages[0].InputRefs[0] != "../../private" {
		t.Fatal("display evidence was reinterpreted")
	}
	if err := os.WriteFile(Path(dir), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Import(dir, view); err == nil {
		t.Fatal("corrupt registry overwritten")
	}
}

func TestRecoveryDiagnosticsFitAlongsideExistingRuns(t *testing.T) {
	dir := t.TempDir()
	original := testRun("history")
	original.Stages = []Stage{{ID: "saved", Kind: "source", Status: "unknown", Prompt: strings.Repeat("x", 15<<20), InputRefs: []string{}, OutputRefs: []string{}, ConsumerStatus: "unknown"}}
	if _, _, err := Import(dir, View{SchemaVersion: 1, Runs: []Run{original}}); err != nil {
		t.Fatal(err)
	}
	diagnostic := testRun("recovery")
	diagnostic.Stages = []Stage{{ID: "readback", Kind: "source", Status: "unknown", Prompt: strings.Repeat("y", 2<<20), InputRefs: []string{}, OutputRefs: []string{}, ConsumerStatus: "unknown"}}
	if _, _, err := Import(dir, View{SchemaVersion: 1, Runs: []Run{diagnostic}}); err != nil {
		t.Fatal(err)
	}
	saved, err := Load(dir)
	if err != nil || len(saved.Runs) != 2 || saved.Runs[0].Stages[0].Prompt != original.Stages[0].Prompt {
		t.Fatalf("original evidence changed: %v", err)
	}
}
