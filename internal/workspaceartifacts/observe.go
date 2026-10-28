package workspaceartifacts

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
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
