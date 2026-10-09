package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// TreeSHA256 is the read-only digest of a selected Skill's file contents.
//
// Rows are sorted [relative-posix-path, raw-file-sha256] pairs serialized with
// compact UTF-8 JSON. This matches FactoryWeaver's normalized Skill tree
// manifest for supported paths. It does not authenticate Git or the Worker.
func TreeSHA256(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("skill root must be an actual directory: %s", root)
	}
	rows := make([][2]string, 0)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill symlink is not admitted: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("skill has unsupported file type: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !utf8.ValidString(relative) || strings.ContainsAny(relative, "\u2028\u2029") {
			return fmt.Errorf("skill path cannot be represented consistently: %s", relative)
		}
		size, err := entry.Info()
		if err != nil {
			return err
		}
		if size.Size() > 2_000_000 {
			return fmt.Errorf("skill file exceeds byte limit: %s", relative)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		rows = append(rows, [2]string{relative, hex.EncodeToString(digest[:])})
		if len(rows) > 200 {
			return fmt.Errorf("skill tree exceeds file limit")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("empty skill root")
	}
	entry, err := os.Lstat(filepath.Join(root, "SKILL.md"))
	if err != nil || !entry.Mode().IsRegular() {
		return "", fmt.Errorf("missing regular SKILL.md")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(rows); err != nil {
		return "", err
	}
	raw := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
