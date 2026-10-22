package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type taskRoleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// The original task owner supplies a frozen role; it never changes the order or worktree.
func (l *Loop) taskRole(cand dispatchCandidate) (string, string, error) {
	var prompt struct {
		Kind string        `json:"kind"`
		Role *taskRoleFile `json:"task_role"`
	}
	if json.Unmarshal([]byte(cand.Stage.Prompt), &prompt) != nil || prompt.Role == nil {
		return "", cand.Stage.Skill, nil
	}
	if prompt.Kind != "local_task" || cand.Stage.Skill != "local-task" || !l.retainTaskData(cand.OrderID) {
		return "", "", fmt.Errorf("task role requires the original retained local task")
	}
	read := func(ref taskRoleFile) ([]byte, error) {
		if !filepath.IsAbs(ref.Path) || len(ref.SHA256) != 64 {
			return nil, fmt.Errorf("task role source reference invalid")
		}
		info, err := os.Lstat(ref.Path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("task role source is not a regular file")
		}
		raw, err := os.ReadFile(ref.Path)
		if err != nil {
			return nil, err
		}
		if recoveryDigest(raw) != ref.SHA256 {
			return nil, fmt.Errorf("task role source changed: %s", ref.Path)
		}
		return raw, nil
	}
	raw, err := read(*prompt.Role)
	if err != nil {
		return "", "", err
	}
	var role struct {
		Schema  int            `json:"schema"`
		Kind    string         `json:"kind"`
		OrderID string         `json:"order_id"`
		Skill   string         `json:"skill"`
		Context string         `json:"context_sha256"`
		Files   []taskRoleFile `json:"files"`
	}
	if json.Unmarshal(raw, &role) != nil || role.Schema != 1 || role.Kind != "local_task_role" || role.OrderID != cand.OrderID || role.Skill == "" || strings.ContainsAny(role.Skill, `/\`) || len(role.Files) == 0 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(role.Context) {
		return "", "", fmt.Errorf("task role original binding invalid")
	}
	for _, ref := range role.Files {
		if _, err := read(ref); err != nil {
			return "", "", err
		}
	}
	return role.Context, role.Skill, nil
}
