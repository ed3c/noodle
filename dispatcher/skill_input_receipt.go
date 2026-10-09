package dispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/filex"
)

// skillInputReceipt describes Noodle's prompt assembly and OS process-launch
// boundary. It never claims Codex/Claude internal Skill discovery, consumption
// of stdin, or an accepted Soodles owner/Factory profile.
type skillInputReceipt struct {
	Protocol                       string   `json:"protocol"`
	Phase                          string   `json:"phase"`
	SessionID                      string   `json:"session_id"`
	WorktreePath                   string   `json:"worktree_path"`
	SelectedSkill                  string   `json:"selected_skill"`
	SelectionMode                  string   `json:"selection_mode"`
	SelectedSourcePath             string   `json:"selected_source_path,omitempty"`
	SelectedSkillPath              string   `json:"selected_skill_path,omitempty"`
	SelectedSkillMDSHA256          string   `json:"selected_skill_md_sha256,omitempty"`
	RequiredSkillSHA256            string   `json:"required_skill_sha256,omitempty"`
	RequiredSkillPinMatched        bool     `json:"required_skill_pin_matched"`
	MethodologyPromptSHA256        string   `json:"methodology_prompt_sha256"`
	ComposedInputSHA256            string   `json:"composed_input_sha256"`
	Warnings                       []string `json:"warnings"`
	ProcessPID                     int      `json:"process_pid"`
	OSProcessLaunched              bool     `json:"os_process_launched"`
	EffectiveAgentCatalogVerified  bool     `json:"effective_agent_catalog_verified"`
	GlobalSkillInheritanceExcluded bool    `json:"global_skill_inheritance_excluded"`
	OriginalOwnerVerified          bool     `json:"original_owner_verified"`
	EffectAuthority                bool     `json:"effect_authority"`
}

func receiptSHA256(payload string) string {
	raw := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(raw[:])
}

func skillReceiptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "skill-input.json")
}

func writePreparedSkillInputReceipt(sessionDir, sessionID string, req DispatchRequest, loaded loadedSkill, composed string) error {
	mode := "NO_SKILL_SELECTED"
	switch {
	case strings.TrimSpace(req.SystemPrompt) != "":
		mode = "SYSTEM_PROMPT_OVERRIDE"
	case strings.TrimSpace(req.Skill) != "" && loaded.ResolvedPath == "":
		mode = "SELECTED_SKILL_MISSING_WARNING"
	case loaded.ResolvedPath != "":
		mode = "RESOLVED_SKILL_EMBEDDED"
	}
	receipt := skillInputReceipt{
		Protocol: "noodle/skill-input-v1",
		Phase: "PREPARED_BEFORE_OS_LAUNCH",
		SessionID: sessionID, WorktreePath: req.WorktreePath,
		SelectedSkill: strings.TrimSpace(req.Skill),
		SelectionMode: mode,
		SelectedSourcePath: loaded.SourcePath,
		SelectedSkillPath: loaded.ResolvedPath,
		SelectedSkillMDSHA256: loaded.EntrySHA256,
		RequiredSkillSHA256: req.RequiredSkillSHA256,
		RequiredSkillPinMatched: req.RequiredSkillSHA256 != "" &&
			loaded.EntrySHA256 == req.RequiredSkillSHA256 &&
			loaded.ResolvedPath != "" && len(loaded.Warnings) == 0 &&
			strings.TrimSpace(req.SystemPrompt) == "",
		MethodologyPromptSHA256: receiptSHA256(loaded.SystemPrompt),
		ComposedInputSHA256: receiptSHA256(composed),
		Warnings: append([]string{}, loaded.Warnings...),
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("marshal skill input receipt: %w", err)
	}
	if err := filex.WriteFileAtomic(skillReceiptPath(sessionDir), payload); err != nil {
		return fmt.Errorf("write prepared skill input receipt: %w", err)
	}
	return nil
}

func markSkillInputProcessLaunched(sessionDir, sessionID string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid process PID")
	}
	path := skillReceiptPath(sessionDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var receipt skillInputReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return err
	}
	if receipt.Protocol != "noodle/skill-input-v1" ||
		receipt.SessionID != sessionID || receipt.Phase != "PREPARED_BEFORE_OS_LAUNCH" ||
		receipt.OSProcessLaunched || receipt.ProcessPID != 0 {
		return fmt.Errorf("skill input receipt identity or phase mismatch")
	}
	receipt.Phase = "OS_PROCESS_LAUNCHED_NOT_AGENT_ATTESTED"
	receipt.OSProcessLaunched = true
	receipt.ProcessPID = pid
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return filex.WriteFileAtomic(path, data)
}


// skillInputReadback compares three Noodle-owned session files with the
// composed prompt file. It is read-only and never attests an Agent's effective
// Skill catalog or the independent Soodles Supervisor's authorization.
type skillInputReadback struct {
	Status                  string `json:"status"`
	SessionID               string `json:"session_id"`
	WorktreePath            string `json:"worktree_path"`
	SelectedSkill           string `json:"selected_skill"`
	SelectionMode           string `json:"selection_mode"`
	SkillMDSHA256           string `json:"skill_md_sha256"`
	ComposedInputSHA256     string `json:"composed_input_sha256"`
	PID                     int    `json:"pid"`
	OriginalOwnerVerified   bool   `json:"original_owner_verified"`
	AgentCatalogVerified    bool   `json:"agent_catalog_verified"`
	EffectAuthority         bool   `json:"effect_authority"`
}

func readSkillInputReadback(sessionDir, sessionID, expectedSkill, expectedWorktree string) (skillInputReadback, error) {
	// A caller selects the session path and claims, never the candidate JSON.
	var prepared skillInputReceipt
	var spawned dispatchMetadata
	var process processMetadata
	for _, spec := range []struct {
		filename string
		out      any
	}{
		{"skill-input.json", &prepared},
		{"spawn.json", &spawned},
		{"process.json", &process},
	} {
		raw, err := os.ReadFile(filepath.Join(sessionDir, spec.filename))
		if err != nil {
			return skillInputReadback{}, fmt.Errorf("session %s missing: %w", spec.filename, err)
		}
		if err := json.Unmarshal(raw, spec.out); err != nil {
			return skillInputReadback{}, fmt.Errorf("session %s invalid: %w", spec.filename, err)
		}
	}
	if prepared.Protocol != "noodle/skill-input-v1" ||
		prepared.Phase != "OS_PROCESS_LAUNCHED_NOT_AGENT_ATTESTED" ||
		!prepared.OSProcessLaunched || prepared.ProcessPID <= 0 ||
		prepared.SessionID != sessionID || spawned.SessionID != sessionID ||
		process.SessionID != sessionID || process.PID != prepared.ProcessPID {
		return skillInputReadback{}, fmt.Errorf("session skill input, spawn or PID identity mismatch")
	}
	if prepared.WorktreePath != expectedWorktree || spawned.WorktreePath != expectedWorktree ||
		prepared.SelectedSkill != expectedSkill || spawned.Skill != expectedSkill {
		return skillInputReadback{}, fmt.Errorf("session worktree or method identity mismatch")
	}
	inputFile := filepath.Join(sessionDir, "input.txt")
	raw, err := os.ReadFile(inputFile)
	if os.IsNotExist(err) {
		raw, err = os.ReadFile(filepath.Join(sessionDir, "prompt.txt"))
	}
	if err != nil {
		return skillInputReadback{}, fmt.Errorf("session composed input missing: %w", err)
	}
	actualInputHash := sha256.Sum256(raw)
	if prepared.ComposedInputSHA256 != hex.EncodeToString(actualInputHash[:]) {
		return skillInputReadback{}, fmt.Errorf("session composed prompt digest drift")
	}
	if prepared.SelectionMode != "RESOLVED_SKILL_EMBEDDED" ||
		prepared.SelectedSkillPath == "" || prepared.SelectedSkillMDSHA256 == "" {
		return skillInputReadback{}, fmt.Errorf("session mandatory selected Skill not embedded")
	}
	if prepared.RequiredSkillSHA256 != "" &&
		(!prepared.RequiredSkillPinMatched ||
			prepared.RequiredSkillSHA256 != prepared.SelectedSkillMDSHA256) {
		return skillInputReadback{}, fmt.Errorf("session required Skill source pin mismatch")
	}
	selectedBytes, err := os.ReadFile(filepath.Join(prepared.SelectedSkillPath, "SKILL.md"))
	if err != nil {
		return skillInputReadback{}, fmt.Errorf("session selected SKILL.md absent: %w", err)
	}
	digest := sha256.Sum256(selectedBytes)
	if prepared.SelectedSkillMDSHA256 != hex.EncodeToString(digest[:]) {
		return skillInputReadback{}, fmt.Errorf("session selected SKILL.md digest drift")
	}
	// This confirms the original file/prompt/process artifacts are consistent
	// at read time; self-consistency is not provider/host authentication.
	return skillInputReadback{
		Status: "NOODLE_LOCAL_OS_BOUND_INPUT_READBACK",
		SessionID: sessionID, WorktreePath: expectedWorktree,
		SelectedSkill: expectedSkill, SelectionMode: prepared.SelectionMode,
		SkillMDSHA256: prepared.SelectedSkillMDSHA256,
		ComposedInputSHA256: prepared.ComposedInputSHA256, PID: prepared.ProcessPID,
	}, nil
}
