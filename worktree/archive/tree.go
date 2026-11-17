package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type Tree struct {
	Entries  map[string]Entry `json:"entries"`
	Root     Metadata         `json:"root_metadata"`
	Mode     uint32           `json:"root_mode"`
	Excluded []string         `json:"excluded"`
}

func Capture(root string, excluded []string) (Tree, error) {
	return capture(root, excluded, false)
}

func capture(root string, excluded []string, includeGit bool) (Tree, error) {
	t := Tree{Entries: map[string]Entry{}, Excluded: excluded}
	if err := canonical(root); err != nil {
		return t, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return t, err
	}
	if st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return t, errors.New("root special permissions require review")
	}
	t.Mode = uint32(st.Mode().Perm())
	if t.Root, err = ReadMetadata(root); err != nil {
		return t, err
	}
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
		if !safeRelative(rel) {
			return errors.New("source path cannot be represented losslessly in the manifest")
		}
		if rel == ".git" && !includeGit {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		for _, skip := range excluded {
			if rel == skip || strings.HasPrefix(rel, skip+string(filepath.Separator)) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if d.Name() == "node_modules" {
			return fmt.Errorf("dependency has no rebuild proof: %s", rel)
		}
		if d.Name() == ".git" && !includeGit {
			return fmt.Errorf("nested repository requires separate owner review: %s", rel)
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !includeGit {
			if err := CheckIndependentFile(path); err != nil {
				return err
			}
		}
		if st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("special permissions require review: %s", rel)
		}
		meta, err := ReadMetadata(path)
		if err != nil {
			return err
		}
		e := Entry{Mode: uint32(st.Mode().Perm()), Metadata: meta, HasMetadata: true}
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			e.Kind = "link"
			e.Target, err = os.Readlink(path)
			if !utf8.ValidString(e.Target) {
				return errors.New("symlink target cannot be represented losslessly")
			}
		case st.IsDir():
			e.Kind = "dir"
		case st.Mode().IsRegular():
			e.Kind = "file"
			e.Size = st.Size()
			e.MtimeNS = st.ModTime().UnixNano()
			e.Hash, err = FileHash(path)
		default:
			return fmt.Errorf("special file requires review: %s", rel)
		}
		if err != nil {
			return err
		}
		t.Entries[rel] = e
		return nil
	})
	return t, err
}

func ordered(entries map[string]Entry, reverse bool) []string {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.Count(keys[i], string(filepath.Separator)), strings.Count(keys[j], string(filepath.Separator))
		if a == b {
			if reverse {
				return keys[i] > keys[j]
			}
			return keys[i] < keys[j]
		}
		if reverse {
			return a > b
		}
		return a < b
	})
	return keys
}

func checkParents(root, rel string) error {
	if !safeRelative(rel) {
		return fmt.Errorf("unsafe restore path: %s", rel)
	}
	p := filepath.Dir(filepath.Join(root, rel))
	for p != root {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("restore parent is not a plain directory: %s", p)
		}
		next := filepath.Dir(p)
		if next == p {
			return errors.New("restore escaped destination")
		}
		p = next
	}
	return nil
}

// Materialize overlays only ignored data. Tracked contents come from the Git pool.
func (s Store) Materialize(dest string, t Tree, tracked map[string]bool) error {
	return s.materialize(dest, t, tracked, false)
}

func (s Store) materialize(dest string, t Tree, tracked map[string]bool, includeGit bool) error {
	if err := canonical(dest); err != nil {
		return err
	}
	for _, rel := range ordered(t.Entries, false) {
		e := t.Entries[rel]
		if err := checkParents(dest, rel); err != nil {
			return err
		}
		p := filepath.Join(dest, rel)
		if e.Kind == "dir" {
			st, err := os.Lstat(p)
			if os.IsNotExist(err) {
				err = os.Mkdir(p, 0700)
			} else if err == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
				return fmt.Errorf("restore directory differs: %s", rel)
			}
			if err != nil {
				return err
			}
			continue
		}
		if tracked[rel] {
			continue
		}
		switch e.Kind {
		case "file":
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			blob, err := s.blob(e.Hash)
			if err == nil {
				err = s.decode(blob, f)
			}
			if err == nil {
				err = f.Sync()
			}
			err = errors.Join(err, f.Close())
			if err != nil {
				return err
			}
		case "link":
			if err := os.Symlink(e.Target, p); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported restored kind %q", e.Kind)
		}
	}
	for _, rel := range ordered(t.Entries, true) {
		e := t.Entries[rel]
		p := filepath.Join(dest, rel)
		if err := checkParents(dest, rel); err != nil {
			return err
		}
		var mode *os.FileMode
		if e.Kind != "link" {
			m := os.FileMode(e.Mode)
			mode = &m
		}
		if e.HasMetadata {
			if err := ApplyMetadata(p, e.Metadata, mode); err != nil {
				return err
			}
		} else if mode != nil {
			if err := os.Chmod(p, *mode); err != nil {
				return err
			}
		}
		if e.Kind == "file" {
			stamp := time.Unix(0, e.MtimeNS)
			if err := os.Chtimes(p, stamp, stamp); err != nil {
				return err
			}
		}
	}
	mode := os.FileMode(t.Mode)
	if err := ApplyMetadata(dest, t.Root, &mode); err != nil {
		return err
	}
	return verifyTree(dest, t, includeGit)
}

func VerifyTree(dest string, want Tree) error {
	return verifyTree(dest, want, false)
}

func verifyTree(dest string, want Tree, includeGit bool) error {
	actual, err := capture(dest, want.Excluded, includeGit)
	if err != nil {
		return err
	}
	for rel, e := range want.Entries {
		if !e.HasMetadata {
			a := actual.Entries[rel]
			a.Metadata = e.Metadata
			a.HasMetadata = false
			actual.Entries[rel] = a
		}
	}
	if !reflect.DeepEqual(actual, want) {
		if len(actual.Entries) != len(want.Entries) {
			return errors.New("restored path inventory differs")
		}
		for rel, e := range want.Entries {
			if !reflect.DeepEqual(actual.Entries[rel], e) {
				return fmt.Errorf("restored bytes, permissions, timestamp or extended metadata differs: %s", rel)
			}
		}
		return errors.New("restored root permissions or extended metadata differs")
	}
	return nil
}
