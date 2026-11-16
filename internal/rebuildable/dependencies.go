// Package rebuildable identifies dependencies from the original Git index.
// It does not inspect or hash dependency contents, or grant cleanup authority.
package rebuildable

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

type Dependency struct {
	Directory string `json:"directory"`
	Manifest  string `json:"manifest"`
	Lockfile  string `json:"lockfile"`
}

var lockfiles = []string{"pnpm-lock.yaml", "package-lock.json", "yarn.lock", "bun.lock", "bun.lockb"}

// Dependencies also declares directories not created yet. A worker may install
// dependencies after dispatch; those still need the same original index proof.
func Dependencies(tracked map[string]bool) ([]Dependency, error) {
	for name := range tracked {
		if !safePath(name) {
			return nil, fmt.Errorf("tracked rebuild source path is unsafe: %s", name)
		}
		for _, part := range strings.Split(name, "/") {
			if part == "node_modules" {
				return nil, fmt.Errorf("node_modules contains tracked content")
			}
		}
	}
	result := []Dependency{}
	for name := range tracked {
		if path.Base(name) != "package.json" {
			continue
		}
		parent := path.Dir(name)
		selected := ""
		for directory := parent; ; directory = path.Dir(directory) {
			for _, lock := range lockfiles {
				if tracked[path.Join(directory, lock)] {
					selected = path.Join(directory, lock)
					break
				}
			}
			if selected != "" || directory == "." {
				break
			}
		}
		if selected != "" {
			result = append(result, Dependency{path.Join(parent, "node_modules"), name, selected})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Directory < result[j].Directory })
	return result, nil
}

func safePath(name string) bool {
	return name != "" && name != "." && path.Clean(name) == name && !path.IsAbs(name) && !strings.ContainsAny(name, "\\\x00\r\n:") && name != ".." && !strings.HasPrefix(name, "../")
}

func Validate(dependency Dependency) error {
	for _, name := range []string{dependency.Directory, dependency.Manifest, dependency.Lockfile} {
		if !safePath(name) {
			return fmt.Errorf("dependency rebuild proof path is unsafe")
		}
	}
	proofs, err := Dependencies(map[string]bool{dependency.Manifest: true, dependency.Lockfile: true})
	if err != nil || len(proofs) != 1 || proofs[0] != dependency {
		return fmt.Errorf("dependency declaration or ancestor lockfile differs")
	}
	return nil
}
