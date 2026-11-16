package workspaceartifacts

import (
	"context"
	"fmt"
	"strings"

	"github.com/poteto/noodle/internal/rebuildable"
)

// RebuildableDependencies uses the original index, never a guest's generated
// .gitignore or a directory-name guess. Capture must still describe this root.
func RebuildableDependencies(ctx context.Context, repository string, baseline *Baseline) ([]rebuildable.Dependency, error) {
	if err := VerifyBaseline(ctx, repository, baseline); err != nil {
		return nil, err
	}
	raw, err := git(ctx, repository, nil, "ls-files", "--cached", "-z")
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, name := range strings.Split(string(raw), "\x00") {
		if name != "" {
			tracked[name] = true
		}
	}
	proofs, err := rebuildable.Dependencies(tracked)
	if err != nil {
		return nil, err
	}
	for _, proof := range proofs {
		for _, name := range []string{proof.Manifest, proof.Lockfile} {
			entry, ok := baseline.Entries[name]
			if !ok || (entry.Mode != "100644" && entry.Mode != "100755") {
				return nil, fmt.Errorf("dependency rebuild source is not an original regular file: %s", name)
			}
		}
	}
	return proofs, VerifyBaseline(ctx, repository, baseline)
}
