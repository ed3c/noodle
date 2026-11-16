// Package archive preserves bounded checkout evidence in the original archive store.
package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type Metadata struct {
	Xattrs map[string]string `json:"xattrs"`
	ACL    string            `json:"acl"`
}

// Entry uses the existing archive tuple format, including integer nanoseconds.
type Entry struct {
	Kind        string
	Mode        uint32
	Size        int64
	MtimeNS     int64
	Hash        string
	Target      string
	Metadata    Metadata
	HasMetadata bool
}

func (e Entry) MarshalJSON() ([]byte, error) {
	switch e.Kind {
	case "file":
		return json.Marshal([]any{e.Kind, e.Mode, e.Size, e.MtimeNS, e.Hash, e.Metadata})
	case "dir":
		return json.Marshal([]any{e.Kind, e.Mode, e.Metadata})
	case "link":
		return json.Marshal([]any{e.Kind, e.Target, e.Metadata})
	default:
		return nil, fmt.Errorf("unsupported manifest kind %q", e.Kind)
	}
}

func (e *Entry) UnmarshalJSON(raw []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return err
	}
	if len(parts) < 2 {
		return errors.New("manifest entry is incomplete")
	}
	if err := json.Unmarshal(parts[0], &e.Kind); err != nil {
		return err
	}
	var values []any
	switch e.Kind {
	case "file":
		values = []any{&e.Kind, &e.Mode, &e.Size, &e.MtimeNS, &e.Hash, &e.Metadata}
	case "dir":
		values = []any{&e.Kind, &e.Mode, &e.Metadata}
	case "link":
		values = []any{&e.Kind, &e.Target, &e.Metadata}
	default:
		return fmt.Errorf("unsupported manifest kind %q", e.Kind)
	}
	e.HasMetadata = len(parts) == len(values)

	if len(parts) != len(values) {
		return errors.New("manifest entry fields differ")
	}
	for i, v := range values {
		if err := json.Unmarshal(parts[i], v); err != nil {
			return err
		}
	}
	if e.Mode > 07777 || e.Size < 0 || (e.Kind == "file" && !validHash(e.Hash)) {
		return errors.New("manifest entry value is invalid")
	}
	return nil
}

type Store struct {
	Root  string
	Codec string
}

func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}

func canonical(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("path is not absolute and clean: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if resolved != path {
		return fmt.Errorf("symlink path is unsupported: %s", path)
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func durableMkdirAll(path string, mode os.FileMode) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("durable directory path is not absolute and clean")
	}
	if st, err := os.Lstat(path); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("durable directory is not a plain directory")
		}
		return errors.Join(syncDir(path), syncDir(filepath.Dir(path)))
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := durableMkdirAll(filepath.Dir(path), mode); err != nil {
		return err
	}
	if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
		return err
	}
	if err := canonical(path); err != nil {
		return err
	}
	return errors.Join(syncDir(path), syncDir(filepath.Dir(path)))
}

func WriteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".publish-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (s Store) blob(key string) (string, error) {
	if !validHash(key) {
		return "", errors.New("blob hash is invalid")
	}
	if err := canonical(s.Root); err != nil {
		return "", err
	}
	p := filepath.Join(s.Root, key[:2], key+".zst")
	for _, q := range []string{filepath.Dir(p), p} {
		st, err := os.Lstat(q)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("blob path is a symlink: %s", q)
		}
	}
	return p, nil
}

func (s Store) decode(path string, dst io.Writer) error {
	if !filepath.IsAbs(s.Codec) {
		return errors.New("absolute zstd executable is missing")
	}
	cmd, cancel := timedCommand(2*time.Minute, s.Codec, "-q", "-d", "-c", "--", path)
	defer cancel()
	cmd.Stdout = dst
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("decode blob: %w: %s", err, stderr.String())
	}
	return nil
}

func (s Store) verifyPath(path, key string, size int64) error {
	h := sha256.New()
	counter := &countWriter{Writer: h}
	if err := s.decode(path, counter); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != key || counter.n != size {
		return fmt.Errorf("blob content differs: %s", path)
	}
	return nil
}

type countWriter struct {
	io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.n += int64(n)
	return n, err
}

func (s Store) Verify(key string, size int64) error {
	p, err := s.blob(key)
	if err != nil {
		return err
	}
	return s.verifyPath(p, key, size)
}

func (s Store) Put(source string, e Entry) error {
	p, err := s.blob(e.Hash)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(p); err == nil {
		return s.Verify(e.Hash, e.Size)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = durableMkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	available, err := AvailableBytes(s.Root)
	if err != nil {
		return err
	}
	if available < uint64(e.Size)+(2<<30) {
		return errors.New("insufficient verified free space for archival temporary data; source retained")
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".blob-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	src, err := os.Open(source)
	if err != nil {
		f.Close()
		return err
	}
	defer src.Close()
	h := sha256.New()
	cmd, cancel := timedCommand(2*time.Minute, s.Codec, "-q", "-3", "-c")
	defer cancel()
	cmd.Stdin = io.TeeReader(src, h)
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil && hex.EncodeToString(h.Sum(nil)) != e.Hash {
		err = errors.New("source changed while archiving")
	}
	if err == nil {
		err = f.Chmod(0444)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return fmt.Errorf("encode blob: %w: %s", err, stderr.String())
	}
	if err = s.verifyPath(tmp, e.Hash, e.Size); err != nil {
		return err
	}
	if err = os.Link(tmp, p); err != nil && !os.IsExist(err) {
		return err
	}
	if err = syncDir(filepath.Dir(p)); err != nil {
		return err
	}
	if err = syncDir(s.Root); err != nil {
		return err
	}
	return s.Verify(e.Hash, e.Size)
}

func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func ReadManifest(path, codec string) (map[string]Entry, error) {
	var raw []byte
	if strings.HasSuffix(path, ".zst") {
		var b strings.Builder
		if err := (Store{Codec: codec}).decode(path, &b); err != nil {
			return nil, err
		}
		raw = []byte(b.String())
	} else {
		var err error
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	var entries map[string]Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	for rel := range entries {
		if !safeRelative(rel) {
			return nil, fmt.Errorf("unsafe manifest path %q", rel)
		}
	}
	return entries, nil
}

func safeRelative(rel string) bool {
	return utf8.ValidString(rel) && rel != "" && rel != "." && !filepath.IsAbs(rel) && filepath.Clean(rel) == rel && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
