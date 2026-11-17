package archive

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/poteto/noodle/internal/rebuildable"
)

type Source struct {
	Path      string          `json:"path"`
	Head      string          `json:"head"`
	Common    string          `json:"common"`
	Admin     string          `json:"admin"`
	Branch    string          `json:"branch"`
	Tracked   map[string]bool `json:"tracked"`
	Excluded  []string        `json:"excluded"`
	AdminTree Tree            `json:"original_git_admin"`
}

type DirtySourceError struct{}

func (*DirtySourceError) Error() string {
	return "uncommitted or untracked code; commit the original branch before preservation"
}

func InspectSource(c Config, root string) (Source, error) {
	s := Source{Path: root, Tracked: map[string]bool{}}
	if !c.Managed(root) {
		return s, errors.New("path is outside scope or protected")
	}
	if err := canonical(root); err != nil {
		return s, err
	}
	top, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return s, err
	}
	if strings.TrimSpace(top) != root {
		return s, errors.New("path is not the exact Git worktree root")
	}
	head, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return s, err
	}
	s.Head = strings.TrimSpace(head)
	common, err := git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return s, err
	}
	s.Common = strings.TrimSpace(common)
	admin, err := git(root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return s, err
	}
	s.Admin = strings.TrimSpace(admin)
	if s.Common == s.Admin {
		return s, errors.New("primary checkout or standalone clone requires separate preservation review")
	}
	link, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil {
		return s, err
	}
	if !link.Mode().IsRegular() {
		return s, errors.New("checkout Git link is not a plain file")
	}
	backlink, err := os.ReadFile(filepath.Join(s.Admin, "gitdir"))
	if err != nil {
		return s, err
	}
	if strings.TrimSpace(string(backlink)) != filepath.Join(root, ".git") {
		return s, errors.New("worktree registration backlink differs from source")
	}
	for _, p := range []string{s.Common, s.Admin} {
		if err = canonical(p); err != nil {
			return s, err
		}
	}
	branch, branchErr := git(root, "symbolic-ref", "-q", "HEAD")
	if branchErr == nil {
		s.Branch = strings.TrimSpace(branch)
	}
	for _, dir := range []string{s.Common, s.Admin} {
		for _, name := range []string{"locked", "index.lock", "HEAD.lock", "config.lock", "packed-refs.lock", "shallow.lock", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "BISECT_LOG"} {
			_, err := os.Lstat(filepath.Join(dir, name))
			if err == nil {
				return s, fmt.Errorf("Git operation or lock requires retention: %s", name)
			}
			if !os.IsNotExist(err) {
				return s, err
			}
		}
	}
	for _, p := range []string{filepath.Join(s.Common, "shallow"), filepath.Join(s.Common, "objects", "info", "alternates"), filepath.Join(s.Admin, "config.worktree")} {
		if _, err := os.Lstat(p); err == nil {
			return s, fmt.Errorf("Git storage/configuration requires separate recovery: %s", p)
		} else if !os.IsNotExist(err) {
			return s, err
		}
	}
	stash, err := git(root, "stash", "list")
	if err != nil {
		return s, err
	}
	if stash != "" {
		return s, errors.New("shared repository stash requires retention")
	}
	index, err := git(root, "ls-files", "-v", "-z")
	if err != nil {
		return s, err
	}
	for _, row := range strings.Split(index, "\x00") {
		if row == "" {
			continue
		}
		if !strings.HasPrefix(row, "H ") {
			return s, errors.New("special Git index flags require retention")
		}
		rel := row[2:]
		if !safeRelative(rel) {
			return s, errors.New("tracked path is unsafe")
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "node_modules" {
				return s, errors.New("node_modules contains tracked content")
			}
		}
		s.Tracked[rel] = true
	}
	status, err := git(root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return s, err
	}
	if s.AdminTree, err = inspectAdmin(s); err != nil {
		return s, err
	}
	if status != "" {
		return s, &DirtySourceError{}
	}
	proofs, err := rebuildable.Dependencies(s.Tracked)
	if err != nil {
		return s, err
	}
	qualified := map[string]bool{}
	for _, proof := range proofs {
		qualified[proof.Directory] = true
	}
	// Only dependency directories with a tracked package declaration and lockfile
	// are excluded. Their contents are never walked or hashed.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == ".git" {
			return fmt.Errorf("nested repository requires separate processing: %s", rel)
		}
		if d.Name() != "node_modules" {
			return nil
		}
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("dependency path is not a plain directory: %s", rel)
		}
		if !qualified[filepath.ToSlash(rel)] {
			return fmt.Errorf("dependency tracked declaration or lockfile is unavailable: %s", rel)
		}
		s.Excluded = append(s.Excluded, rel)
		return fs.SkipDir
	})
	sort.Strings(s.Excluded)
	return s, err
}

func inspectAdmin(source Source) (Tree, error) {
	excluded := []string{}
	allowed := map[string]bool{"noodle-incarnation.json": true, "HEAD": true, "logs": true, "logs/HEAD": true, "ORIG_HEAD": true, "FETCH_HEAD": true, "index": true, "gitdir": true, "commondir": true}
	refsObserved := false
	err := filepath.WalkDir(source.Admin, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source.Admin {
			return nil
		}
		rel, err := filepath.Rel(source.Admin, path)
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("registration symlink requires retention: %s", rel)
		}
		if rel == "COMMIT_EDITMSG" {
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("Git commit record is not a plain file: COMMIT_EDITMSG")
			}
			return nil
		}
		if rel == "refs" {
			if !d.IsDir() {
				return errors.New("Git registration refs is not a plain directory: refs")
			}
			namespace, err := git(source.Path, "rev-parse", "--path-format=absolute", "--git-path", "refs/worktree")
			if err != nil {
				return err
			}
			if filepath.Dir(strings.TrimSpace(namespace)) != path {
				return errors.New("Git registration refs owner binding differs: refs")
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			if len(entries) != 0 {
				return fmt.Errorf("nonempty Git registration refs require separate preservation: refs/%s", entries[0].Name())
			}
			refsObserved = true
			return nil
		}
		if rel == "soodles-test-requests.noindex" || strings.HasPrefix(rel, "soodles-test-requests.noindex/") {
			return nil
		}
		if !allowed[rel] {
			return fmt.Errorf("Git recovery record requires separate preservation: %s", rel)
		}
		return nil
	})
	if err != nil {
		return Tree{}, err
	}
	log, err := os.ReadFile(filepath.Join(source.Admin, "logs", "HEAD"))
	if err != nil && !os.IsNotExist(err) {
		return Tree{}, err
	}
	seen := map[string]bool{}
	var additional []string
	for _, name := range []string{"ORIG_HEAD", "FETCH_HEAD"} {
		data, err := os.ReadFile(filepath.Join(source.Admin, name))
		if err != nil && !os.IsNotExist(err) {
			return Tree{}, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				additional = append(additional, fields[0])
			}
		}
	}
	for _, oid := range additional {
		log = append(log, []byte("\n"+oid+" "+oid)...)
	}
	for _, line := range strings.Split(string(log), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return Tree{}, errors.New("Git HEAD reflog record is incomplete")
		}
		for _, oid := range fields[:2] {
			if strings.Trim(oid, "0") == "" || seen[oid] {
				continue
			}
			seen[oid] = true
			if len(oid) != 40 && len(oid) != 64 {
				return Tree{}, errors.New("Git HEAD reflog object ID is unsupported")
			}
			if _, err := hex.DecodeString(oid); err != nil {
				return Tree{}, errors.New("Git recovery object ID is malformed")
			}
			if _, err := git(source.Path, "merge-base", "--is-ancestor", oid, source.Head); err != nil {
				return Tree{}, fmt.Errorf("Git HEAD reflog contains a commit outside the preserved HEAD history; source retained: %s", oid)
			}
		}
	}
	for _, name := range []string{"HEAD", "index", "gitdir", "commondir", "noodle-incarnation.json"} {
		st, err := os.Lstat(filepath.Join(source.Admin, name))
		if err != nil || !st.Mode().IsRegular() {
			return Tree{}, fmt.Errorf("required Git registration file unavailable: %s", name)
		}
	}
	tree, err := Capture(source.Admin, excluded)
	if err != nil {
		return Tree{}, err
	}
	_, refsCaptured := tree.Entries["refs"]
	if refsObserved != refsCaptured {
		return Tree{}, errors.New("Git registration refs presence changed during capture: refs")
	}
	for rel, entry := range tree.Entries {
		if rel == "COMMIT_EDITMSG" && entry.Kind != "file" {
			return Tree{}, errors.New("Git commit record changed type during capture: COMMIT_EDITMSG")
		}
		if rel == "refs" && entry.Kind != "dir" {
			return Tree{}, errors.New("Git registration refs changed type during capture: refs")
		}
		if strings.HasPrefix(rel, "refs/") {
			return Tree{}, fmt.Errorf("Git registration refs changed during capture; separate preservation required: %s", rel)
		}
	}
	return tree, nil
}
