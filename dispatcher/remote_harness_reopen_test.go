package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/skill"
)

type savedHarnessFixture struct {
	root, runtimeDir string
	plan             *RemoteHarness
	saved            PreparedRemoteHarnessInput
	binding          PreparedRemoteHarnessReopenBinding
}

func prepareSavedHarnessFixture(t *testing.T, instructions bool) savedHarnessFixture {
	t.Helper()
	root, ref := remoteInstructionFixture(t)
	if !instructions {
		ref = nil
	}
	runtimeDir := filepath.Join(root, "runtime")
	req := DispatchRequest{
		Name: "original", Runtime: "openshell", Provider: "codex", Model: "gpt-6.1-sol",
		ReasoningLevel: "high", Skill: "missing-today-skill", SystemPrompt: "original wrapper",
		Prompt: "original task\n秘密 'quoted' $(literal)", WorktreePath: root, InstructionBundle: ref,
		EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order", "NOODLE_STAGE_INDEX": "2"},
	}
	plan, err := PrepareRemoteHarness(runtimeDir, skill.Resolver{}, req, filepath.Join(root, "codex"), "/sandbox/noodle-sessions", "/sandbox/payload/noodle-instructions")
	if err != nil {
		t.Fatal(err)
	}
	saved := PreparedRemoteHarnessInput{
		Spawn:        readHarnessFixtureFile(t, filepath.Join(plan.Directory, "spawn.json")),
		Startup:      readHarnessFixtureFile(t, plan.startup.path),
		Input:        readHarnessFixtureFile(t, inputPath(plan.Directory)),
		Prompt:       readHarnessFixtureFile(t, filepath.Join(plan.Directory, "prompt.txt")),
		Instructions: plan.Instructions,
	}
	binding := PreparedRemoteHarnessBinding{OrderID: "original-order", SessionID: plan.ID, WorktreePath: root, Binary: filepath.Join(root, "codex"), InstructionVerifier: "/sandbox/payload/noodle-instructions"}
	observed, err := InspectPreparedRemoteHarness(saved, binding)
	if err != nil {
		t.Fatal(err)
	}
	return savedHarnessFixture{root, runtimeDir, plan, saved, PreparedRemoteHarnessReopenBinding{binding, "2", observed}}
}

func readHarnessFixtureFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type savedHarnessFile struct {
	Mode os.FileMode
	Data string
}

func snapshotSavedHarness(t *testing.T, root string) map[string]savedHarnessFile {
	t.Helper()
	files := map[string]savedHarnessFile{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data := ""
		if !entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 {
				data, err = os.Readlink(name)
				if err != nil {
					return err
				}
			} else {
				data = string(readHarnessFixtureFile(t, name))
			}
		}
		files[name] = savedHarnessFile{info.Mode(), data}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestReopenPreparedRemoteHarnessPreservesSavedSession(t *testing.T) {
	for _, instructions := range []bool{false, true} {
		name := "plain"
		if instructions {
			name = "instructions"
		}
		t.Run(name, func(t *testing.T) {
			f := prepareSavedHarnessFixture(t, instructions)
			// Original source files can disappear. Reopen reads transported files.
			for _, name := range []string{"SKILL.md", "bundle.json"} {
				if err := os.Remove(filepath.Join(f.root, name)); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"raw.ndjson", "canonical.ndjson", "stderr.log", "events.ndjson"} {
				writeSavedHarnessFixture(t, &f, name, nil)
			}
			before := snapshotSavedHarness(t, f.root)
			plan, err := ReopenPreparedRemoteHarness(f.runtimeDir, f.saved, f.binding)
			if err != nil {
				t.Fatal(err)
			}
			if plan.ID != f.plan.ID || plan.Directory != f.plan.Directory || plan.GuestSessionDir != f.plan.GuestSessionDir || plan.runtimeDir != f.runtimeDir || plan.stamped != f.plan.stamped || plan.canonical != f.plan.canonical || plan.stderr != f.plan.stderr {
				t.Fatal("original session paths changed")
			}
			if !reflect.DeepEqual(plan.Command(), f.plan.Command()) || plan.prompt != string(f.saved.Input) || plan.req.Prompt != string(f.saved.Prompt) || plan.req.Model != f.plan.req.Model || plan.req.ReasoningLevel != "high" || plan.req.Skill != f.plan.req.Skill || plan.req.WorktreePath != f.root || !reflect.DeepEqual(plan.req.EnvVars, f.plan.req.EnvVars) || !reflect.DeepEqual(plan.Instructions, f.plan.Instructions) {
				t.Fatal("original argv/stdin/request/instruction bundle changed")
			}
			if plan.started || plan.pi != nil || plan.writer == nil || plan.startup.path != f.plan.startup.path || !reflect.DeepEqual(plan.startup.receipt, f.plan.startup.receipt) || len(plan.warnings) != 0 {
				t.Fatal("loaded harness is not the original unstarted Codex input")
			}
			observed, err := InspectPreparedRemoteHarness(f.saved, f.binding.PreparedRemoteHarnessBinding)
			if err != nil || observed != f.binding.Observation {
				t.Fatalf("pure inspection changed after reopen: %+v %v", observed, err)
			}
			if !reflect.DeepEqual(before, snapshotSavedHarness(t, f.root)) {
				t.Fatal("constructor changed saved bytes/modes or created evidence")
			}
			if instructions && plan.Instructions == f.saved.Instructions {
				t.Fatal("reopened materialization aliases caller-owned state")
			}
		})
	}
}

func replaceSavedHarnessJSON(t *testing.T, f *savedHarnessFixture, name string, edit func(map[string]any)) {
	t.Helper()
	var data *[]byte
	if name == "spawn.json" {
		data = &f.saved.Spawn
	} else {
		data = &f.saved.Startup
	}
	var value map[string]any
	if err := json.Unmarshal(*data, &value); err != nil {
		t.Fatal(err)
	}
	edit(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	*data = encoded
	if err := os.WriteFile(filepath.Join(f.plan.Directory, name), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReopenPreparedRemoteHarnessRefusesBeforeWriting(t *testing.T) {
	type refusalCase struct {
		name string
		edit func(*testing.T, *savedHarnessFixture)
	}
	cases := []refusalCase{
		{"order", func(t *testing.T, f *savedHarnessFixture) { f.binding.OrderID = "foreign" }},
		{"session", func(t *testing.T, f *savedHarnessFixture) { f.binding.SessionID = "foreign" }},
		{"session-traversal", func(t *testing.T, f *savedHarnessFixture) { f.binding.SessionID = "../foreign" }},
		{"session-whitespace", func(t *testing.T, f *savedHarnessFixture) { f.binding.SessionID += " " }},
		{"worktree", func(t *testing.T, f *savedHarnessFixture) { f.binding.WorktreePath += "-foreign" }},
		{"stage", func(t *testing.T, f *savedHarnessFixture) { f.binding.StageIndex = "3" }},
		{"binary", func(t *testing.T, f *savedHarnessFixture) { f.binding.Binary += "-drift" }},
		{"verifier", func(t *testing.T, f *savedHarnessFixture) { f.binding.InstructionVerifier += "-drift" }},
		{"observation", func(t *testing.T, f *savedHarnessFixture) { f.binding.Observation.ArgvSHA256 = "wrong" }},
		{"guest-directory", func(t *testing.T, f *savedHarnessFixture) { f.binding.Observation.GuestSessionDirectory += "-drift" }},
		{"missing-prepared", func(t *testing.T, f *savedHarnessFixture) { f.saved.Instructions = nil }},
		{"prepared-manifest", func(t *testing.T, f *savedHarnessFixture) { f.saved.Instructions.Manifest = nil }},
		{"prepared-guest", func(t *testing.T, f *savedHarnessFixture) { f.saved.Instructions.GuestDirectory += "-drift" }},
		{"prepared-mode", func(t *testing.T, f *savedHarnessFixture) { *f.saved.Instructions.Manifest.Files[0].Mode ^= 0100 }},
		{"materialized-mode", func(t *testing.T, f *savedHarnessFixture) {
			if err := os.Chmod(filepath.Join(f.plan.Instructions.Directory, "skills", "selected", "SKILL.md"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"materialized-bytes", func(t *testing.T, f *savedHarnessFixture) {
			writeSavedHarnessFixture(t, f, filepath.Join(filepath.Base(f.plan.Instructions.Directory), "skills", "selected", "SKILL.md"), []byte("changed skill"))
		}},
		{"manifest-bytes", func(t *testing.T, f *savedHarnessFixture) {
			writeSavedHarnessFixture(t, f, filepath.Join(filepath.Base(f.plan.Instructions.Directory), "manifest.json"), []byte("{}"))
		}},
		{"runtime-dir", func(t *testing.T, f *savedHarnessFixture) { f.runtimeDir += "-missing" }},
		{"input", func(t *testing.T, f *savedHarnessFixture) {
			f.saved.Input[0] ^= 1
			writeSavedHarnessFixture(t, f, "input.txt", f.saved.Input)
		}},
		{"prompt", func(t *testing.T, f *savedHarnessFixture) {
			f.saved.Prompt[0] ^= 1
			writeSavedHarnessFixture(t, f, "prompt.txt", f.saved.Prompt)
		}},
	}
	for _, field := range []string{"session_id", "order_id", "worktree_path", "model", "reasoning_level", "stage_index", "resume_session_id", "resume_thread_id", "resume_source_sha256", "runtime", "provider"} {
		cases = append(cases, refusalCase{"spawn-" + field, func(t *testing.T, f *savedHarnessFixture) {
			replaceSavedHarnessJSON(t, f, "spawn.json", func(v map[string]any) {
				v[field] = "drift"
				if field == "reasoning_level" {
					v[field] = "medium"
				}
			})
		}})
	}
	for _, field := range []string{"session_id", "first_stdout", "first_stderr", "first_init_at", "native_thread_id", "native_thread_source_sha256", "native_thread_error", "persistence_error"} {
		cases = append(cases, refusalCase{"startup-" + field, func(t *testing.T, f *savedHarnessFixture) {
			replaceSavedHarnessJSON(t, f, "codex-startup.json", func(v map[string]any) {
				value := any("drift")
				if field == "first_stdout" || field == "first_stderr" {
					value = map[string]any{"at": nowUTC(), "bytes": 1}
				} else if field == "first_init_at" {
					value = nowUTC()
				}
				v[field] = value
			})
		}})
	}
	for _, field := range []string{"expected_bytes", "write_started_at", "write_completed_at", "written_bytes", "write_error", "close_completed_at", "close_error"} {
		cases = append(cases, refusalCase{"stdin-" + field, func(t *testing.T, f *savedHarnessFixture) {
			replaceSavedHarnessJSON(t, f, "codex-startup.json", func(v map[string]any) {
				value := any("drift")
				if strings.HasSuffix(field, "_at") {
					value = nowUTC()
				} else if field == "expected_bytes" || field == "written_bytes" {
					value = 0
				}
				v["stdin"].(map[string]any)[field] = value
			})
		}})
	}
	for _, name := range []string{"spawn.json", "codex-startup.json", "input.txt", "prompt.txt", "instruction-mapping.json", "instructions/manifest", "instructions/skill"} {
		cases = append(cases, refusalCase{"missing-" + name, func(t *testing.T, f *savedHarnessFixture) {
			file := filepath.Join(f.plan.Directory, name)
			if name == "instructions/manifest" {
				file = filepath.Join(f.plan.Instructions.Directory, "manifest.json")
			} else if name == "instructions/skill" {
				file = filepath.Join(f.plan.Instructions.Directory, "skills", "selected", "SKILL.md")
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}})
	}
	for _, name := range []string{"spawn.json", "codex-startup.json", "input.txt", "prompt.txt", "instruction-mapping.json", "raw.ndjson", "canonical.ndjson", "stderr.log", "events.ndjson", "heartbeat.json", "process.json", "native.json"} {
		cases = append(cases, refusalCase{"disk-drift-" + name, func(t *testing.T, f *savedHarnessFixture) {
			writeSavedHarnessFixture(t, f, name, []byte("original start evidence"))
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := prepareSavedHarnessFixture(t, true)
			tc.edit(t, &f)
			before := snapshotSavedHarness(t, f.root)
			plan, err := ReopenPreparedRemoteHarness(f.runtimeDir, f.saved, f.binding)
			if err == nil || plan != nil || err.Error() == "" {
				t.Fatalf("invalid saved harness accepted: %v %v", plan, err)
			}
			if !reflect.DeepEqual(before, snapshotSavedHarness(t, f.root)) {
				t.Fatal("refused constructor changed evidence")
			}
		})
	}
}

func writeSavedHarnessFixture(t *testing.T, f *savedHarnessFixture, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.plan.Directory, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReopenPreparedRemoteHarnessStartSendsOriginalInput(t *testing.T) {
	f := prepareSavedHarnessFixture(t, false)
	writeHarnessCLI(t, f.root, "codex", `printf '%s\0' "$@" > argv
cat > received
printf '%s\n' '{"type":"thread.started","thread_id":"`+resumeTestThread+`"}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	before := snapshotSavedHarness(t, f.root)
	plan, err := ReopenPreparedRemoteHarness(f.runtimeDir, f.saved, f.binding)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshotSavedHarness(t, f.root)) {
		t.Fatal("constructor started a process or changed evidence")
	}
	argv := plan.Command()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = f.root
	process, err := StartProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.ForceKill(); <-process.Done() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	session, err := plan.Start(ctx, process, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitHarnessSession(t, session)
	process.IOWaitGroup().Wait()
	if !bytes.Equal(readHarnessFixtureFile(t, filepath.Join(f.root, "received")), f.saved.Input) || !reflect.DeepEqual(readHarnessArgs(t, f.root), f.plan.Command()[1:]) || session.ID() != f.plan.ID {
		t.Fatal("original stdin/argv/session did not reach the consumer")
	}
	receipt := readCodexStartup(t, plan.startup.path)
	if receipt.Stdin.WrittenBytes == nil || *receipt.Stdin.WrittenBytes != len(f.saved.Input) || receipt.Stdin.CloseCompletedAt == nil || receipt.Stdin.WriteError != "" || receipt.Stdin.CloseError != "" || receipt.FirstInitAt == nil {
		t.Fatalf("loaded startup did not observe first input: %+v", receipt)
	}
	if _, err := plan.Start(ctx, process, nil); err == nil {
		t.Fatal("second Start accepted")
	}
	events := readHarnessFixtureFile(t, filepath.Join(plan.Directory, "events.ndjson"))
	if bytes.Count(events, []byte(`"prompt_injected"`)) != 1 {
		t.Fatal("original prompt event missing or duplicated")
	}
	if reopened, err := ReopenPreparedRemoteHarness(f.runtimeDir, f.saved, f.binding); err == nil || reopened != nil {
		t.Fatal("started harness reopened from stale saved input")
	}
}
