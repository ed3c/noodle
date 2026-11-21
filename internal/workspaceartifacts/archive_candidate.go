package workspaceartifacts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ArchiveCandidateTree describes an isolated tree comparison. It is never a
// publication receipt or authority to update a ref in the original repository.
type ArchiveCandidateTree struct {
	BaselineTree    string   `json:"baseline_tree"`
	IntegrationHead string   `json:"integration_head"`
	CandidateTree   string   `json:"candidate_tree"`
	ChangedPaths    []string `json:"changed_paths"`
}

// InspectArchiveCandidate runs the existing local-task scope oracle in a private
// Git repository. The original objects are read through a scratch alternate.
func InspectArchiveCandidate(ctx context.Context, target, scratch, restored string, baseline *Baseline, scope LocalTaskScope) (ArchiveCandidateTree, error) {
	var result ArchiveCandidateTree
	if err := VerifyBaseline(ctx, target, baseline); err != nil {
		return result, err
	}
	integrationRef := "refs/heads/" + scope.IntegrationBranch
	if _, err := git(ctx, target, nil, "check-ref-format", integrationRef); err != nil {
		return result, err
	}
	integration, err := git(ctx, target, nil, "rev-parse", "--verify", integrationRef+"^{commit}")
	if err != nil {
		return result, err
	}
	result.IntegrationHead = strings.TrimSpace(string(integration))
	format, err := git(ctx, target, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return result, err
	}
	common, err := git(ctx, target, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return result, err
	}
	objects, err := filepath.EvalSymlinks(filepath.Join(strings.TrimSpace(string(common)), "objects"))
	if err != nil || !filepath.IsAbs(objects) || strings.ContainsAny(objects, "\r\n") {
		return result, fmt.Errorf("archive inspection original Git object source is unavailable")
	}
	if err = os.Mkdir(scratch, 0700); err != nil {
		return result, err
	}
	if _, err = git(ctx, scratch, nil, "init", "--quiet", "--template=", "--object-format="+strings.TrimSpace(string(format))); err != nil {
		return result, err
	}
	if err = os.WriteFile(filepath.Join(scratch, ".git", "objects", "info", "alternates"), []byte(objects+"\n"), 0600); err != nil {
		return result, err
	}
	if _, err = git(ctx, scratch, nil, "update-ref", integrationRef, result.IntegrationHead); err != nil {
		return result, err
	}
	baseTree, err := git(ctx, scratch, nil, "rev-parse", "--verify", baseline.Head+"^{tree}")
	if err != nil {
		return result, err
	}
	result.BaselineTree = strings.TrimSpace(string(baseTree))
	entries, content, err := scanDirectory(restored)
	if err != nil {
		return result, err
	}
	result.CandidateTree, err = writeTree(ctx, scratch, entries, content)
	if err != nil {
		return result, err
	}
	var findings Result
	if err = scope.check(ctx, scratch, baseline.Head, result.CandidateTree, &findings); err != nil {
		return result, err
	}
	if findings.LocalTaskRefused() {
		return result, fmt.Errorf("archive candidate violates original scope: write_paths=%q role=%q unsupported=%q invalid=%q", findings.ScopeViolations, findings.RoleScopeViolations, findings.UnsupportedPaths, findings.InvalidPaths)
	}
	changed, err := localTaskChangedPaths(ctx, scratch, baseline.Head, result.CandidateTree)
	if err != nil {
		return result, err
	}
	result.ChangedPaths = make([]string, 0, len(changed))
	for name := range changed {
		if !LocalTaskPathAllowed(scope.WritePaths, name) {
			return result, fmt.Errorf("archive candidate changes a path outside original write_paths: %q", name)
		}
		result.ChangedPaths = append(result.ChangedPaths, name)
	}
	slices.Sort(result.ChangedPaths)
	after, err := git(ctx, target, nil, "rev-parse", "--verify", integrationRef+"^{commit}")
	if err != nil || strings.TrimSpace(string(after)) != result.IntegrationHead {
		return result, fmt.Errorf("archive inspection original integration revision changed")
	}
	return result, VerifyBaseline(ctx, target, baseline)
}
