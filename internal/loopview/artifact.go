package loopview

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ArtifactResult is inert display data. Status never authorizes an owner effect.
type ArtifactResult struct {
	Artifact Artifact `json:"artifact"`
	Status   string   `json:"status"`
	Content  *string  `json:"content"`
	Error    *string  `json:"error"`
}

func ArtifactContent(a Artifact) ArtifactResult {
	metadata := a
	metadata.Content = nil
	metadata.PublicContent = nil
	result := ArtifactResult{Artifact: metadata, Status: a.Status}
	refuse := func(status, message string) ArtifactResult {
		result.Status = status
		result.Error = &message
		return result
	}
	if a.Status != "available" {
		return refuse(a.Status, "artifact content is not available in this snapshot")
	}
	ref := a.Source
	if ref == nil || ref.SHA256 == nil || ref.Bytes == nil {
		return refuse("unknown", "original artifact version binding missing")
	}
	if *ref.Bytes < 0 || *ref.Bytes > MaxBytes {
		return refuse("unknown", "artifact exceeds display size bound")
	}
	expected, err := hex.DecodeString(*ref.SHA256)
	if err != nil || len(expected) != sha256.Size {
		return refuse("unknown", "artifact digest invalid")
	}

	if !filepath.IsAbs(ref.Path) || filepath.Clean(ref.Path) != ref.Path {
		return refuse("unknown", "artifact path is not an absolute clean bound path")
	}
	// Inline snapshots do not waive current source checks. Pin the opened file,
	// reject symlink components and verify its bytes against the registered version.
	volume := filepath.VolumeName(ref.Path)
	current := volume + string(filepath.Separator)
	var last os.FileInfo
	for _, component := range strings.Split(strings.TrimPrefix(ref.Path, current), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		last, err = os.Lstat(current)
		if err != nil {
			return refuse("unknown", fmt.Sprintf("artifact source unavailable: %v", err))
		}
		if last.Mode()&os.ModeSymlink != 0 {
			return refuse("unknown", "artifact source contains a symlink")
		}
	}
	if last == nil || !last.Mode().IsRegular() {
		return refuse("unknown", "artifact source is not a regular file")
	}
	root, openErr := os.OpenRoot(filepath.Dir(ref.Path))
	if openErr != nil {
		return refuse("unknown", openErr.Error())
	}
	defer root.Close()
	f, openErr := root.Open(filepath.Base(ref.Path))
	if openErr != nil {
		return refuse("unknown", openErr.Error())
	}
	defer f.Close()
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() || !os.SameFile(last, info) {
		return refuse("stale", "artifact source changed during open")
	}
	if info.Size() != *ref.Bytes {
		return refuse("stale", "artifact byte size changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return refuse("unknown", err.Error())
	}
	if int64(len(data)) != *ref.Bytes {
		return refuse("stale", "artifact byte size differs from captured version")
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != strings.ToLower(*ref.SHA256) {
		return refuse("stale", "artifact digest differs from captured version")
	}
	if !utf8.Valid(data) {
		return refuse("unknown", "artifact is not UTF-8 text")
	}
	content := string(data)
	result.Content = &content
	return result
}
