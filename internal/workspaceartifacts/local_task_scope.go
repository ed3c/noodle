package workspaceartifacts

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// LocalTaskScope is the original Host's local-task publication boundary.
type LocalTaskScope struct {
	IntegrationBranch  string
	WritePaths         []string
	PriorCandidateHead string
	RoleWritePaths     []string
}

func LocalTaskSafePath(name string) bool {
	if !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool { return r < 32 || r == 127 || r == '\\' }) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// LocalTaskPathAllowed treats write paths as literal files or subtrees.
func LocalTaskPathAllowed(scopes []string, name string) bool {
	return LocalTaskSafePath(name) && inLocalTaskScope(scopes, name)
}

func inLocalTaskScope(scopes []string, name string) bool {
	for _, scope := range scopes {
		if LocalTaskSafePath(scope) && (name == scope || strings.HasPrefix(name, scope+"/")) {
			return true
		}
	}
	return false
}

func (scope *LocalTaskScope) check(ctx context.Context, target, parent, tree string, result *Result) error {
	if scope == nil {
		return nil
	}
	base, err := git(ctx, target, nil, "merge-base", "refs/heads/"+scope.IntegrationBranch, parent)
	if err != nil {
		return fmt.Errorf("resolve local task integration base: %w", err)
	}
	paths, err := localTaskChangedPaths(ctx, target, strings.TrimSpace(string(base)), tree)
	if err != nil {
		return err
	}
	out, err := git(ctx, target, nil, "ls-tree", "-r", "-z", "--full-tree", tree)
	if err != nil {
		return err
	}
	entries, err := parsePublicationTree(out, false)
	if err != nil {
		return err
	}
	invalid := map[string]bool{}
	for name := range paths {
		if !LocalTaskSafePath(name) {
			invalid[name] = true
			continue
		}
		if !inLocalTaskScope(scope.WritePaths, name) {
			result.ScopeViolations = append(result.ScopeViolations, name)
		}
	}
	for name, entry := range entries {
		regular := entry.Mode == "100644" || entry.Mode == "100755"
		ancestor := !regular && slices.ContainsFunc(scope.WritePaths, func(path string) bool {
			return strings.HasPrefix(path, name+"/")
		})
		if !paths[name] && !inLocalTaskScope(scope.WritePaths, name) && !ancestor {
			continue
		}
		if !LocalTaskSafePath(name) {
			invalid[name] = true
			continue
		}
		if !regular {
			result.UnsupportedPaths = append(result.UnsupportedPaths, name)
		}
	}
	if scope.PriorCandidateHead != "" {
		affected, err := localTaskChangedPaths(ctx, target, scope.PriorCandidateHead, tree)
		if err != nil {
			return err
		}
		for name := range affected {
			if !LocalTaskSafePath(name) {
				invalid[name] = true
				continue
			}
			if !inLocalTaskScope(scope.RoleWritePaths, name) {
				result.RoleScopeViolations = append(result.RoleScopeViolations, name)
			}
		}
	}
	for name := range invalid {
		if !utf8.ValidString(name) {
			name = strconv.Quote(name)
		}
		result.InvalidPaths = append(result.InvalidPaths, name)
	}
	slices.Sort(result.ScopeViolations)
	slices.Sort(result.RoleScopeViolations)
	slices.Sort(result.UnsupportedPaths)
	slices.Sort(result.InvalidPaths)
	return nil
}

func localTaskChangedPaths(ctx context.Context, target, base, tree string) (map[string]bool, error) {
	out, err := git(ctx, target, nil, "diff", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, tree, "--")
	if err != nil {
		return nil, fmt.Errorf("check local task candidate scope: %w", err)
	}
	paths := map[string]bool{}
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			paths[name] = true
		}
	}
	return paths, nil
}
