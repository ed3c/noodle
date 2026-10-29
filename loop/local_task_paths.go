package loop

import (
	"fmt"
	"github.com/poteto/noodle/internal/execution"
	"os"
	"path/filepath"
	"strings"
)

// Local task paths are literal file or subtree boundaries, never glob patterns.
func localTaskPathAllowed(scopes []string, name string) bool {
	if !localTaskSafePath(name) {
		return false
	}
	for _, scope := range scopes {
		if localTaskSafePath(scope) && (name == scope || strings.HasPrefix(name, scope+"/")) {
			return true
		}
	}
	return false
}

// Expand Git-visible files, including tracked deletions. Ignored build output
// is not candidate content and must not become part of a directory grant.
func (l *Loop) localTaskScopeFiles(root string, scopes []string) (map[string]bool, error) {
	return LocalTaskScopeFiles(l.deps.Execution, root, scopes)
}

// LocalTaskScopeFiles is shared by native candidate readers and merge checks.
func LocalTaskScopeFiles(e *execution.Context, root string, scopes []string) (map[string]bool, error) {
	names := map[string]bool{}
	for _, args := range [][]string{{"ls-tree", "-r", "--name-only", "-z", "HEAD"}, {"ls-files", "--cached", "--others", "--exclude-standard", "-z"}} {
		data, err := boundGitCommand(e, root, append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			return nil, err
		}
		for _, name := range strings.Split(string(data), "\x00") {
			if name != "" && localTaskPathAllowed(scopes, name) {
				names[name] = true
			}
		}
	}
	for _, scope := range scopes {
		if !localTaskSafePath(scope) {
			return nil, fmt.Errorf("local task scope path invalid: %s", scope)
		}
		path := filepath.Join(root, scope)
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && info.IsDir() {
			resolved, resolveErr := filepath.EvalSymlinks(path)
			canonical, rootErr := filepath.EvalSymlinks(root)
			if resolveErr != nil || rootErr != nil || resolved != filepath.Join(canonical, scope) {
				return nil, fmt.Errorf("local task scope directory identity changed: %s", scope)
			}
			continue
		}
		// A missing subtree still has its tracked descendants in the inventory.
		hasChildren := false
		for name := range names {
			if strings.HasPrefix(name, scope+"/") {
				hasChildren = true
				break
			}
		}
		if !hasChildren || err == nil {
			names[scope] = true
		}
	}
	return names, nil
}
