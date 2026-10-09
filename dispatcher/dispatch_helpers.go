package dispatcher

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/poteto/noodle/skill"
)

// resolveSkillBundle resolves the skill bundle for a dispatch request: uses
// SystemPrompt verbatim if set, or falls back to loading the named skill bundle.
func resolveSkillBundle(resolver skill.Resolver, req DispatchRequest) (loadedSkill, error) {
	pin := req.RequiredSkillSHA256
	if pin != "" || req.RequiredSkillTreeSHA256 != "" {
		digest, err := hex.DecodeString(pin)
		if err != nil || len(digest) != 32 || strings.ToLower(pin) != pin {
			return loadedSkill{}, fmt.Errorf("required Skill SHA-256 is not exact lowercase hex")
		}
		treePin, treeErr := hex.DecodeString(req.RequiredSkillTreeSHA256)
		if treeErr != nil || len(treePin) != 32 || strings.ToLower(req.RequiredSkillTreeSHA256) != req.RequiredSkillTreeSHA256 {
			return loadedSkill{}, fmt.Errorf("required Skill tree SHA-256 is not exact lowercase hex")
		}
		if strings.TrimSpace(req.Skill) == "" {
			return loadedSkill{}, fmt.Errorf("required Skill cannot have an empty name")
		}
		if strings.TrimSpace(req.SystemPrompt) != "" {
			return loadedSkill{}, fmt.Errorf("required Skill cannot be replaced by SystemPrompt")
		}
		// A first-match-wins resolver alone cannot prove the required method
		// was unambiguous across configured project/user/global providers.
		seen := make(map[string]struct{})
		for _, source := range resolver.SearchPaths {
			found, err := (skill.Resolver{SearchPaths: []string{source}}).Resolve(strings.TrimSpace(req.Skill))
			if errors.Is(err, skill.ErrNotFound) {
				continue
			}
			if err != nil {
				return loadedSkill{}, err
			}
			seen[found.Path] = struct{}{}
			if len(seen) > 1 {
				return loadedSkill{}, fmt.Errorf("required Skill has shadowed providers")
			}
		}
		for path := range seen {
			tree, err := skill.TreeSHA256(path)
			if err != nil {
				return loadedSkill{}, fmt.Errorf("required Skill tree cannot be verified: %w", err)
			}
			if tree != req.RequiredSkillTreeSHA256 {
				return loadedSkill{}, fmt.Errorf("required Skill tree SHA-256 mismatch")
			}
		}
		loaded, err := loadSkillBundle(resolver, req.Provider, req.Skill)
		if err != nil {
			return loadedSkill{}, err
		}
		if loaded.ResolvedPath == "" || loaded.EntrySHA256 == "" {
			return loadedSkill{}, fmt.Errorf("required Skill missing from resolver")
		}
		if loaded.EntrySHA256 != pin {
			return loadedSkill{}, fmt.Errorf("required Skill source SHA-256 mismatch")
		}
		tree, err := skill.TreeSHA256(loaded.ResolvedPath)
		if err != nil {
			return loadedSkill{}, fmt.Errorf("required Skill tree cannot be verified: %w", err)
		}
		if tree != req.RequiredSkillTreeSHA256 {
			return loadedSkill{}, fmt.Errorf("required Skill tree SHA-256 mismatch")
		}
		if len(loaded.Warnings) != 0 {
			return loadedSkill{}, fmt.Errorf("required Skill was incomplete: %s", strings.Join(loaded.Warnings, ", "))
		}
		return loaded, nil
	}
	if sp := strings.TrimSpace(req.SystemPrompt); sp != "" {
		return loadedSkill{SystemPrompt: sp}, nil
	}
	return loadSkillBundle(resolver, req.Provider, req.Skill)
}

// writePromptFiles writes prompt.txt (the user-facing prompt) and, when the
// composed prompt differs, input.txt (the full prompt sent to the agent).
// It returns the path to the file containing the composed prompt.
func writePromptFiles(sessionDir, promptPath, prompt, composedPrompt string) (inputFile string, err error) {
	if err := os.WriteFile(promptPath, []byte(prompt), 0o644); err != nil {
		return "", fmt.Errorf("write prompt file: %w", err)
	}
	inputFile = promptPath
	if composedPrompt != prompt {
		inputFile = inputPath(sessionDir)
		if err := os.WriteFile(inputFile, []byte(composedPrompt), 0o644); err != nil {
			return "", fmt.Errorf("write input file: %w", err)
		}
	}
	return inputFile, nil
}
