package workspaceartifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/poteto/noodle/internal/filex"
)

const DiagnosticBaselineCompactionName = "diagnostic-baseline-compaction.json"

type diagnosticBaselineCompaction struct {
	Version    int                        `json:"version"`
	Repository string                     `json:"repository"`
	Target     string                     `json:"target"`
	Baseline   Baseline                   `json:"baseline"`
	Objects    map[string]compactedObject `json:"objects"`
	Sources    map[string]string          `json:"sources"`
	Copy       map[string]retentionEntry  `json:"copy"`
}

// VerifyDiagnosticBaseline proves only the original clean input, never a
// publication. After compaction it uses pinned objects and surviving bytes;
// the original worktree may then be removed by its separate original owner.
func VerifyDiagnosticBaseline(ctx context.Context, repository string, b *Baseline, target string, sources map[string]string) error {
	root, path, err := diagnosticBaselinePaths(ctx, repository, b, target)
	if err != nil {
		return err
	}
	raw, err := retentionReadFile(path)
	if os.IsNotExist(err) {
		_, err = prepareDiagnosticBaseline(ctx, root, b, target, sources)
		return err
	}
	if err != nil {
		return err
	}
	m, err := readDiagnosticBaseline(raw, root, b, target, sources)
	if err != nil {
		return err
	}
	refs, err := diagnosticBaselineRefs(ctx, m)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		fresh, err := prepareDiagnosticBaseline(ctx, root, b, target, sources)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(fresh, m) {
			return fmt.Errorf("unpinned diagnostic baseline intent differs from full original inputs")
		}
		return nil
	}
	if err := verifyDiagnosticBaseline(ctx, m, raw, true); err != nil {
		return err
	}
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return verifyDiagnosticCleanTarget(ctx, target, b)
}

// CompactDiagnosticBaseline removes only session/repo. Original dispatch,
// baseline receipt, instructions, logs and closure authority stay in place.
// The caller exclusively owns the original inactive session and worktree.
func CompactDiagnosticBaseline(ctx context.Context, repository string, b *Baseline, target string, sources map[string]string) error {
	return compactDiagnosticBaseline(ctx, repository, b, target, sources, os.Remove)
}

func compactDiagnosticBaseline(ctx context.Context, repository string, b *Baseline, target string, sources map[string]string, remove func(string) error) error {
	root, path, err := diagnosticBaselinePaths(ctx, repository, b, target)
	if err != nil {
		return err
	}
	raw, err := retentionReadFile(path)
	if os.IsNotExist(err) {
		m, e := prepareDiagnosticBaseline(ctx, root, b, target, sources)
		if e != nil {
			return e
		}
		raw, err = json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		if err = filex.WriteFileAtomicDurable(path, raw); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	m, err := readDiagnosticBaseline(raw, root, b, target, sources)
	if err != nil {
		return err
	}
	if err = verifyDiagnosticBaseline(ctx, m, raw, false); err != nil {
		return err
	}
	refs, err := diagnosticBaselineRefs(ctx, m)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		// No pin means deletion was never authorized: require the full clean inputs.
		fresh, e := prepareDiagnosticBaseline(ctx, root, b, target, sources)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(fresh, m) {
			return fmt.Errorf("diagnostic baseline intent changed before pinning")
		}
		if err = syncCompactionIntent(path); err != nil {
			return err
		}
		oid, e := compactionGit(ctx, root, raw, "hash-object", "-w", "--stdin", "--no-filters")
		if e != nil {
			return e
		}
		if err = syncDiagnosticBaseline(ctx, m, strings.TrimSpace(string(oid)), false); err != nil {
			return err
		}
		prefix := diagnosticBaselinePrefix(m)
		tx := fmt.Sprintf("start\noption no-deref\ncreate %s/base %s\ncreate %s/manifest %s\nprepare\ncommit\n", prefix, b.Head, prefix, strings.TrimSpace(string(oid)))
		if _, err = compactionGit(ctx, root, []byte(tx), "update-ref", "--stdin"); err != nil {
			return err
		}
	}
	if err = verifyDiagnosticBaseline(ctx, m, raw, true); err != nil {
		return err
	}
	refs, err = diagnosticBaselineRefs(ctx, m)
	if err != nil {
		return err
	}
	if err = syncDiagnosticBaseline(ctx, m, refs[diagnosticBaselinePrefix(m)+"/manifest"], true); err != nil {
		return err
	}
	return removeRetainedCopy(ctx, b.Directory, m.Copy, remove)
}

func diagnosticBaselinePaths(ctx context.Context, repository string, b *Baseline, target string) (string, string, error) {
	if b == nil || !validObjectID(b.Head) || len(b.IndexSHA256) != 64 || b.Entries == nil || filepath.Base(b.Directory) != "repo" {
		return "", "", fmt.Errorf("diagnostic baseline identity incomplete")
	}
	for _, p := range []string{repository, b.Directory, target} {
		if err := noLinkedPath(p); err != nil {
			return "", "", err
		}
	}
	root, err := repositoryRoot(ctx, repository)
	if err != nil {
		return "", "", err
	}
	session := filepath.Dir(b.Directory)
	inside := func(parent, child string) bool {
		rel, e := filepath.Rel(parent, child)
		return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if inside(session, root) || inside(session, target) || (inside(root, session) && filepath.Dir(session) != filepath.Join(root, ".noodle", "sessions")) {
		return "", "", fmt.Errorf("diagnostic baseline paths overlap repository or differ from bounded session layout")
	}
	path := filepath.Join(session, DiagnosticBaselineCompactionName)
	if err := noLinkedPath(path); err != nil {
		return "", "", err
	}
	return root, path, nil
}

func prepareDiagnosticBaseline(ctx context.Context, root string, b *Baseline, target string, sources map[string]string) (*diagnosticBaselineCompaction, error) {
	if err := verifyDiagnosticCleanTarget(ctx, target, b); err != nil {
		return nil, err
	}
	entries, data, err := scanDirectory(b.Directory)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(entries, b.Entries) {
		return nil, fmt.Errorf("diagnostic retained baseline differs")
	}
	tree, err := retainedTree(ctx, root, b.Head)
	if err != nil {
		return nil, err
	}
	if err = retainedTreeBytes(ctx, tree, entries, data); err != nil {
		return nil, err
	}
	objects, err := retainedObjects(ctx, root, b.Head)
	if err != nil {
		return nil, err
	}
	inventory, err := retentionInventory(ctx, b.Directory)
	if err != nil {
		return nil, err
	}
	m := &diagnosticBaselineCompaction{Version: 1, Repository: root, Target: target, Baseline: *b, Objects: objects, Sources: sources, Copy: inventory}
	if err = verifyDiagnosticBaseline(ctx, m, nil, false); err != nil {
		return nil, err
	}
	return m, nil
}

func readDiagnosticBaseline(raw []byte, root string, b *Baseline, target string, sources map[string]string) (*diagnosticBaselineCompaction, error) {
	var m diagnosticBaselineCompaction
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Version != 1 || m.Repository != root || m.Target != target || !reflect.DeepEqual(m.Baseline, *b) || !reflect.DeepEqual(m.Sources, sources) {
		return nil, fmt.Errorf("diagnostic baseline compaction binding differs")
	}
	return &m, nil
}

func verifyDiagnosticBaseline(ctx context.Context, m *diagnosticBaselineCompaction, raw []byte, pinned bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	objects, err := retainedObjects(ctx, m.Repository, m.Baseline.Head)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(objects, m.Objects) {
		return fmt.Errorf("diagnostic baseline Git objects differ")
	}
	if len(m.Sources) == 0 {
		return fmt.Errorf("diagnostic baseline source evidence missing")
	}
	for path, want := range m.Sources {
		if path == m.Baseline.Directory || strings.HasPrefix(path, m.Baseline.Directory+string(filepath.Separator)) {
			return fmt.Errorf("diagnostic source overlaps deleted copy")
		}
		actual, err := retentionReadFile(path)
		if err != nil {
			return err
		}
		if digest(actual) != want {
			return fmt.Errorf("diagnostic baseline source changed: %s", path)
		}
	}
	current, err := retentionInventory(ctx, m.Baseline.Directory)
	if err != nil {
		return err
	}
	for name, entry := range current {
		if original, ok := m.Copy[name]; !ok || original != entry {
			return fmt.Errorf("diagnostic baseline copy changed: %s", name)
		}
	}
	if !pinned {
		return nil
	}
	refs, err := diagnosticBaselineRefs(ctx, m)
	if err != nil {
		return err
	}
	prefix := diagnosticBaselinePrefix(m)
	if len(refs) != 2 || refs[prefix+"/base"] != m.Baseline.Head || !matchesRawBlob(raw, refs[prefix+"/manifest"]) {
		return fmt.Errorf("diagnostic baseline pins differ")
	}
	saved, err := compactionGit(ctx, m.Repository, nil, "cat-file", "blob", refs[prefix+"/manifest"])
	if err != nil {
		return err
	}
	if !bytes.Equal(saved, raw) {
		return fmt.Errorf("diagnostic baseline pinned manifest differs")
	}
	return nil
}

func diagnosticBaselinePrefix(m *diagnosticBaselineCompaction) string {
	return "refs/noodle/diagnostic-baseline/" + digest([]byte(m.Baseline.Directory))
}
func diagnosticBaselineRefs(ctx context.Context, m *diagnosticBaselineCompaction) (map[string]string, error) {
	raw, err := compactionGit(ctx, m.Repository, nil, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", diagnosticBaselinePrefix(m)+"/")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("diagnostic baseline ref is symbolic or malformed")
		}
		refs[fields[0]] = fields[1]
	}
	return refs, nil
}

func syncDiagnosticBaseline(ctx context.Context, m *diagnosticBaselineCompaction, manifest string, refs bool) error {
	for path := range m.Sources {
		if err := syncRetentionRegular(path); err != nil {
			return err
		}
		if err := filex.SyncDir(filepath.Dir(path)); err != nil {
			return err
		}
	}
	if err := syncCompactionIntent(filepath.Join(filepath.Dir(m.Baseline.Directory), DiagnosticBaselineCompactionName)); err != nil {
		return err
	}
	raw, err := compactionGit(ctx, m.Repository, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common := strings.TrimSpace(string(raw))
	if err := syncRetainedGitObjects(ctx, m.Repository, m.Baseline.Head, manifest); err != nil {
		return err
	}
	if refs {
		for _, name := range []string{"base", "manifest"} {
			path := filepath.Join(common, filepath.FromSlash(diagnosticBaselinePrefix(m)), name)
			if err := syncRetentionRegular(path); os.IsNotExist(err) {
				if err := syncRetentionRegular(filepath.Join(common, "packed-refs")); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else {
				for dir := filepath.Dir(path); dir != common; dir = filepath.Dir(dir) {
					if err := filex.SyncDir(dir); err != nil {
						return err
					}
				}
			}
		}
	}
	return filex.SyncDir(common)
}

func verifyDiagnosticCleanTarget(ctx context.Context, target string, b *Baseline) error {
	head, err := ReadCleanHead(ctx, target)
	if err != nil {
		return err
	}
	_, index, err := identity(ctx, target)
	if err != nil {
		return err
	}
	if head != b.Head || index != b.IndexSHA256 {
		return fmt.Errorf("diagnostic original clean baseline HEAD or index differs")
	}
	return nil
}
