package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/poteto/noodle/internal/filex"
)

// Expand the original owner's authority before deleting any execution copy.
// Hashing an acceptance file alone does not preserve its referenced evidence.
func prepareReclamationEvidence(authority SessionReadbackFileRef, sources map[string]SessionReadbackFileRef, deletionRoots []string, expectedOrderID string) (map[string]SessionReadbackFileRef, error) {
	var closure struct {
		Order struct {
			OrderID string `json:"order_id"`
			Status  string `json:"status"`
		} `json:"order"`
		Reason      string                            `json:"reason"`
		Diagnostics map[string]SessionReadbackFileRef `json:"diagnostics"`
	}
	actual, err := ReadSessionReadbackFile(authority.Path, &closure)
	if err != nil {
		return nil, err
	}
	if actual != authority || closure.Order.OrderID == "" || closure.Order.OrderID != expectedOrderID || strings.TrimSpace(closure.Reason) == "" || len(closure.Diagnostics) != 1 {
		return nil, fmt.Errorf("original task closure evidence is incomplete")
	}
	refs := map[string]SessionReadbackFileRef{"task_closure": authority}
	for name, ref := range sources {
		if name != "native_custody" {
			refs[name] = ref
		}
	}
	if acceptance, ok := closure.Diagnostics["owner_acceptance"]; ok {
		value, err := ReadTaskDeliveryAcceptance(acceptance)
		if err != nil {
			return nil, err
		}
		if closure.Order.Status != "completed" || value.OrderID != closure.Order.OrderID {
			return nil, fmt.Errorf("original accepted closure identity differs")
		}
		refs["owner_acceptance"] = acceptance
		for i, condition := range value.Conditions {
			for j, ref := range condition.Evidence {
				refs[fmt.Sprintf("acceptance_condition_%d_%d", i, j)] = ref
			}
		}
	} else if diagnostic, ok := closure.Diagnostics["original_flow_diagnostics"]; ok && (closure.Order.Status == "failed" || closure.Order.Status == "cancelled") {
		refs["original_flow_diagnostics"] = diagnostic
	} else {
		return nil, fmt.Errorf("original closure lacks accepted delivery or explicit failed-flow diagnostics")
	}
	if err := VerifySessionReadbackFiles(refs); err != nil {
		return nil, err
	}
	roots := []string{}
	for _, root := range deletionRoots {
		canonical, err := canonicalReclamationRoot(root)
		if err != nil {
			return nil, err
		}
		roots = append(roots, canonical)
	}
	for name, ref := range refs {
		for _, root := range roots {
			rel, err := filepath.Rel(root, ref.Path)
			if err != nil {
				return nil, err
			}
			if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("retained %s evidence overlaps execution data scheduled for deletion", name)
			}
		}
	}
	if err := syncReclamationEvidence(refs); err != nil {
		return nil, err
	}
	return refs, VerifySessionReadbackFiles(refs)
}

func canonicalReclamationRoot(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
		return "", fmt.Errorf("reclamation deletion root is not a bounded absolute path")
	}
	ancestor := path
	suffix := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		suffix = append(suffix, filepath.Base(ancestor))
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("reclamation deletion root has no existing ancestor")
		}
		ancestor = parent
	}
}

// Persist original bytes and directory entries, including downloaded archives,
// before their only remote copy is retired. A closure fsync cannot do this.
func syncReclamationEvidence(refs map[string]SessionReadbackFileRef) error {
	dirs := map[string]bool{}
	for _, ref := range refs {
		before, err := os.Lstat(ref.Path)
		if err != nil {
			return err
		}
		if !before.Mode().IsRegular() {
			return fmt.Errorf("reclamation evidence is not a regular file")
		}
		f, err := os.Open(ref.Path)
		if err != nil {
			return err
		}
		opened, statErr := f.Stat()
		if statErr != nil || !os.SameFile(before, opened) {
			return errors.Join(statErr, f.Close(), fmt.Errorf("reclamation evidence changed before persistence"))
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
		after, err := os.Lstat(ref.Path)
		if err != nil {
			return err
		}
		if !os.SameFile(before, after) {
			return fmt.Errorf("reclamation evidence changed during persistence")
		}
		for dir := filepath.Dir(ref.Path); ; dir = filepath.Dir(dir) {
			dirs[dir] = true
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ordered)))
	for _, dir := range ordered {
		if err := filex.SyncDir(dir); err != nil {
			return err
		}
	}
	return nil
}
