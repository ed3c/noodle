package guestprogress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/poteto/noodle/internal/rebuildable"
)

// ArchivePolicy is fixed by the original Host index before model launch. Only
// future commands carry it; historical commands keep their full-tree archive.
type ArchivePolicy struct {
	Schema       int                      `json:"schema"`
	Dependencies []rebuildable.Dependency `json:"dependencies"`
}

func ParseArchivePolicy(encoded string) (*ArchivePolicy, error) {
	if encoded == "" {
		return nil, nil
	}
	if len(encoded) > 64<<10 {
		return nil, fmt.Errorf("archive rebuild policy exceeds its bound")
	}
	var policy ArchivePolicy
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return nil, err
	}
	if policy.Schema != 1 || len(policy.Dependencies) == 0 || len(policy.Dependencies) > 4096 {
		return nil, fmt.Errorf("archive rebuild policy schema or dependency count differs")
	}
	previous := ""
	for _, proof := range policy.Dependencies {
		if err := rebuildable.Validate(proof); err != nil {
			return nil, err
		}
		if proof.Directory <= previous {
			return nil, fmt.Errorf("archive dependency policy is duplicate or unsorted")
		}
		previous = proof.Directory
	}
	canonical, err := json.Marshal(policy)
	if err != nil || string(canonical) != encoded {
		return nil, fmt.Errorf("archive rebuild policy bytes are not canonical")
	}
	return &policy, nil
}

func ArchivePolicySHA256(encoded string) (string, error) {
	policy, err := ParseArchivePolicy(encoded)
	if err != nil || policy == nil {
		return "", err
	}
	return digest([]byte(encoded)), nil
}

// Manifest and lockfile contents may change during the admitted task. They are
// returned in full. If an anchor was deleted, retain dependencies rather than
// claiming the original proof still reconstructs them. No dependency is walked.
func repositoryExclusions(root string, policy *ArchivePolicy) (map[string]bool, []string, error) {
	excluded := map[string]bool{}
	paths := []string{}
	if policy == nil {
		return excluded, paths, nil
	}
	plain := func(name string, directory bool) (bool, error) {
		full := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		canonical, err := filepath.EvalSymlinks(full)
		if err != nil || canonical != full || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
			return false, fmt.Errorf("dependency rebuild source has an alias or type change: %s", name)
		}
		return true, nil
	}
	for _, proof := range policy.Dependencies {
		present, err := plain(proof.Directory, true)
		if err != nil {
			return nil, nil, err
		}
		if !present {
			continue
		}
		qualified := true
		for _, name := range []string{proof.Manifest, proof.Lockfile} {
			present, err := plain(name, false)
			if err != nil {
				return nil, nil, err
			}
			qualified = qualified && present
		}
		if qualified {
			excluded[proof.Directory] = true
			paths = append(paths, proof.Directory)
		}
	}
	return excluded, paths, nil
}

func ValidateReceiptForPolicy(result Receipt, identity Identity, requestID, encoded string) error {
	if err := ValidateReceipt(result, identity, requestID); err != nil {
		return err
	}
	sha, err := ArchivePolicySHA256(encoded)
	if err != nil {
		return err
	}
	if sha == "" {
		if result.Schema != 1 || result.ArchivePolicySHA256 != "" || len(result.ExcludedDependencies) != 0 {
			return fmt.Errorf("historical progress has no rebuild policy authority")
		}
		return nil
	}
	if result.Schema != 2 || result.ArchivePolicySHA256 != sha {
		return fmt.Errorf("progress supervisor did not consume the original archive rebuild policy")
	}
	policy, _ := ParseArchivePolicy(encoded)
	allowed := map[string]bool{}
	for _, proof := range policy.Dependencies {
		allowed[proof.Directory] = true
	}
	if !slices.IsSorted(result.ExcludedDependencies) {
		return fmt.Errorf("progress dependency exclusions are unsorted")
	}
	previous := ""
	for _, excluded := range result.ExcludedDependencies {
		if !allowed[excluded] || excluded == previous {
			return fmt.Errorf("progress excluded an unbound dependency")
		}
		previous = excluded
	}
	return nil
}
