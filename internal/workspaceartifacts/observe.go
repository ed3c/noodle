package workspaceartifacts

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// ReadCleanHead observes raw committed bytes without git status/diff, filters,
// hooks or index refresh. Git environment isolation and parsing stay shared
// with artifact plumbing; publication continues to honor normal commit hooks.
func ReadCleanHead(ctx context.Context, target string) (string, error) {
	root, err := repositoryRoot(ctx, target)
	if err != nil {
		return "", err
	}
	head, index, err := identity(ctx, root)
	if err != nil {
		return "", err
	}
	out, err := git(ctx, root, nil, "--no-lazy-fetch", "--no-replace-objects", "ls-tree", "-r", "-z", "--full-tree", head)
	if err != nil {
		return "", err
	}
	tree, err := parsePublicationTree(out, false)
	if err != nil {
		return "", err
	}
	if err = verifyPublicationIndex(ctx, root, tree); err != nil {
		return "", err
	}
	for name, expected := range tree {
		entry, data, err := readEntry(root, name)
		if err != nil {
			return "", err
		}
		if entry.Mode != expected.Mode {
			return "", fmt.Errorf("candidate worktree mode differs from HEAD at %q", name)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !matchesRawBlob(data, expected.Object) {
			return "", fmt.Errorf("candidate worktree bytes differ from HEAD at %q", name)
		}
	}
	untracked, err := git(ctx, root, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	if len(untracked) != 0 {
		return "", fmt.Errorf("candidate worktree contains untracked files")
	}
	afterHead, afterIndex, err := identity(ctx, root)
	if err != nil {
		return "", err
	}
	if afterHead != head || afterIndex != index {
		return "", fmt.Errorf("candidate HEAD or index changed during readback")
	}
	return head, nil
}

// Git hashes the raw blob header and bytes. Hash locally so observing a large
// repository does not start a subprocess per file or execute attribute filters.
func matchesRawBlob(data []byte, expected string) bool {
	var digest hash.Hash
	switch len(expected) {
	case sha1.Size * 2:
		digest = sha1.New()
	case sha256.Size * 2:
		digest = sha256.New()
	default:
		return false
	}
	fmt.Fprintf(digest, "blob %d\x00", len(data))
	digest.Write(data)
	return hex.EncodeToString(digest.Sum(nil)) == expected
}

// CleanIdentity binds the raw clean checkout to its attached integration branch.
// It is an observation, and never refreshes the index or executes Git hooks.
type CleanIdentity struct{ Head, Tree, Branch string }

func ReadCleanIdentity(ctx context.Context, target string) (CleanIdentity, error) {
	var result CleanIdentity
	root, err := repositoryRoot(ctx, target)
	if err != nil {
		return result, err
	}
	result.Head, err = ReadCleanHead(ctx, root)
	if err != nil {
		return result, err
	}
	tree, err := git(ctx, root, nil, "--no-lazy-fetch", "--no-replace-objects", "rev-parse", result.Head+"^{tree}")
	if err != nil {
		return result, err
	}
	branch, err := git(ctx, root, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return result, err
	}
	head, err := git(ctx, root, nil, "--no-lazy-fetch", "--no-replace-objects", "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(string(head)) != result.Head {
		return result, fmt.Errorf("checkout HEAD changed during identity observation")
	}
	result.Tree, result.Branch = strings.TrimSpace(string(tree)), strings.TrimSpace(string(branch))
	return result, nil
}
