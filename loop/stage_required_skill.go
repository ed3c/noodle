package loop

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Stage extra fields are an opt-in route from an externally selected Noodle
// order to the already existing dispatcher RequiredSkill* constraints. These
// fields do not create/verify Soodles Supervisor authorization.
const (
	stageRequiredSkillSHA256     = "required_skill_sha256"
	stageRequiredSkillTreeSHA256 = "required_skill_tree_sha256"
)

// stageRequiredSkillPins requires a complete immutable pair or none. The
// original order's stage, not the LLM prompt, supplies these bytes.
//
// This validation happens before interruption offering, worktree mutation,
// dispatch, or OS process launch. The dispatcher independently verifies the
// selected SKILL.md and whole tree again immediately before launch.
func stageRequiredSkillPins(stage Stage) (string, string, error) {
	raw, hasRaw := stage.Extra[stageRequiredSkillSHA256]
	tree, hasTree := stage.Extra[stageRequiredSkillTreeSHA256]
	if !hasRaw && !hasTree {
		return "", "", nil // existing tasks retain legacy behavior
	}
	if !hasRaw || !hasTree {
		return "", "", fmt.Errorf("required Skill pin pair incomplete")
	}
	decode := func(data json.RawMessage, field string) (string, error) {
		var value string
		if len(data) == 0 || json.Unmarshal(data, &value) != nil {
			return "", fmt.Errorf("%s must be a lowercase SHA-256 string", field)
		}
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 32 || value != strings.ToLower(value) {
			return "", fmt.Errorf("%s must be a 64-digit lowercase SHA-256", field)
		}
		return value, nil
	}
	entry, err := decode(raw, stageRequiredSkillSHA256)
	if err != nil {
		return "", "", err
	}
	whole, err := decode(tree, stageRequiredSkillTreeSHA256)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(stage.Skill) == "" {
		return "", "", fmt.Errorf("required Skill pin cannot refer to unnamed stage Skill")
	}
	return entry, whole, nil
}
