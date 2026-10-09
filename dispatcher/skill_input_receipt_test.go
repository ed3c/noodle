package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/skill"
)

func decodeSkillInputReceipt(t *testing.T, path string) skillInputReceipt {
	t.Helper()
	var value skillInputReceipt
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSkillInputReceiptKeepsPreparedAndLaunchedEvidenceSeparate(t *testing.T) {
	dir := t.TempDir()
	req := DispatchRequest{Skill: "pstack", WorktreePath: "/tmp/fictional/worktree"}
	loaded := loadedSkill{
		SystemPrompt: "# Selected method\nactual bytes",
		ResolvedPath: "/tmp/fictional/skills/pstack",
		SourcePath: "/tmp/fictional/skills",
		EntrySHA256: strings.Repeat("a", 64),
	}
	if err := writePreparedSkillInputReceipt(dir, "session-test", req, loaded, "full composed input"); err != nil {
		t.Fatal(err)
	}
	path := skillReceiptPath(dir)
	r := decodeSkillInputReceipt(t, path)
	if r.Phase != "PREPARED_BEFORE_OS_LAUNCH" || r.OSProcessLaunched || r.ProcessPID != 0 ||
		r.SelectionMode != "RESOLVED_SKILL_EMBEDDED" || r.SelectedSkill != "pstack" {
		t.Fatalf("prepared metadata granted nonexistent process: %+v", r)
	}
	hash := sha256.Sum256([]byte(loaded.SystemPrompt))
	if r.MethodologyPromptSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("incorrect loaded method digest: %+v", r)
	}
	if r.EffectiveAgentCatalogVerified || r.GlobalSkillInheritanceExcluded ||
		r.OriginalOwnerVerified || r.EffectAuthority {
		t.Fatalf("candidate data claimed external authority: %+v", r)
	}
	if err := markSkillInputProcessLaunched(dir, "foreign-session", 77); err == nil {
		t.Fatal("foreign session got a launch receipt")
	}
	if err := markSkillInputProcessLaunched(dir, "session-test", 0); err == nil {
		t.Fatal("invalid PID got a launch receipt")
	}
	if err := markSkillInputProcessLaunched(dir, "session-test", 77); err != nil {
		t.Fatal(err)
	}
	after := decodeSkillInputReceipt(t, path)
	if after.Phase != "OS_PROCESS_LAUNCHED_NOT_AGENT_ATTESTED" ||
		!after.OSProcessLaunched || after.ProcessPID != 77 ||
		after.EffectiveAgentCatalogVerified || after.EffectAuthority {
		t.Fatalf("OS launch became model or authorization proof: %+v", after)
	}
	if err := markSkillInputProcessLaunched(dir, "session-test", 77); err == nil {
		t.Fatal("replayed launch must not overwrite exact receipt")
	}
}

func TestSkillInputReceiptDisclosesMissingMethodAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name string
		req DispatchRequest
		loaded loadedSkill
		want string
	}{
		{"missing", DispatchRequest{Skill: "not-installed"}, loadedSkill{Warnings: []string{"method missing"}}, "SELECTED_SKILL_MISSING_WARNING"},
		{"override", DispatchRequest{Skill: "pstack", SystemPrompt: "replacement"}, loadedSkill{SystemPrompt: "replacement"}, "SYSTEM_PROMPT_OVERRIDE"},
		{"none", DispatchRequest{}, loadedSkill{}, "NO_SKILL_SELECTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := writePreparedSkillInputReceipt(dir, "session", tc.req, tc.loaded, "input"); err != nil {
				t.Fatal(err)
			}
			r := decodeSkillInputReceipt(t, skillReceiptPath(dir))
			if r.SelectionMode != tc.want || r.OSProcessLaunched ||
				r.EffectiveAgentCatalogVerified || r.OriginalOwnerVerified {
				t.Fatalf("missing/overridden method concealed: %+v", r)
			}
		})
	}
}

func TestProcessDispatcherWritesActualSelectedMethodAtOSLaunch(t *testing.T) {
	// A real OS child process with a fake text consumer. This does not launch
	// Codex or prove that an Agent obeyed the selected methodology.
	search := t.TempDir()
	dir := filepath.Join(search, "poteto-mode")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := "# actual method bytes\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "guide.md"), []byte("guide text"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	runtimeDir := filepath.Join(t.TempDir(), ".noodle")
	d := NewProcessDispatcher(ProcessDispatcherConfig{
		ProjectDir: worktree, RuntimeDir: runtimeDir,
		RuntimeKind: "process", RuntimeDefault: "cat >/dev/null",
		SkillResolver: skill.Resolver{SearchPaths: []string{search}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := d.Dispatch(ctx, DispatchRequest{
		Name: "fixture", Prompt: "a bounded synthetic issue",
		Skill: "poteto-mode", Provider: "codex", Model: "test-model",
		WorktreePath: worktree, AllowPrimaryCheckout: true,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	defer func() { _ = session.ForceKill() }()
	path := filepath.Join(runtimeDir, "sessions", session.ID())
	receipt := decodeSkillInputReceipt(t, filepath.Join(path, "skill-input.json"))
	if !receipt.OSProcessLaunched || receipt.ProcessPID <= 0 ||
		receipt.SelectionMode != "RESOLVED_SKILL_EMBEDDED" ||
		receipt.SelectedSkillPath != dir || receipt.SelectedSourcePath != search {
		t.Fatalf("worker dispatch receipt is not actual selected method: %+v", receipt)
	}
	sum := sha256.Sum256([]byte(entry))
	if receipt.SelectedSkillMDSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("wrong source bytes: %+v", receipt)
	}
	input, err := os.ReadFile(filepath.Join(path, "input.txt"))
	if err != nil {
		t.Fatalf("missing composed input: %v", err)
	}
	digest := sha256.Sum256(input)
	if receipt.ComposedInputSHA256 != hex.EncodeToString(digest[:]) ||
		receipt.MethodologyPromptSHA256 == "" {
		t.Fatalf("actual composed input bytes not bound: %+v", receipt)
	}
	if receipt.EffectiveAgentCatalogVerified || receipt.GlobalSkillInheritanceExcluded ||
		receipt.OriginalOwnerVerified || receipt.EffectAuthority {
		t.Fatalf("OS process boundary falsely attested Agent internals: %+v", receipt)
	}
	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("test child process did not terminate")
	}
}
