package loopview

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var gitObjectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type IndexResult struct {
	File    IndexFile `json:"file"`
	Status  string    `json:"status"`
	Content *string   `json:"content"`
	Error   *string   `json:"error"`
}

func IndexContent(ctx context.Context, repo Repository, file IndexFile) IndexResult {
	result := IndexResult{File: file, Status: "unknown"}
	refuse := func(message string) IndexResult { result.Error = &message; return result }
	if repo.Revision == nil || !gitObjectID.MatchString(*repo.Revision) || file.GitBlob == nil || !gitObjectID.MatchString(*file.GitBlob) || !filepath.IsAbs(repo.Root) {
		return refuse("captured Git object binding unavailable")
	}
	if file.Bytes == nil || *file.Bytes < 0 || *file.Bytes > MaxBytes {
		return refuse("captured file exceeds display size bound")
	}
	if file.GitMode == nil || (*file.GitMode != "100644" && *file.GitMode != "100755") {
		return refuse("captured index entry is not a regular file")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	read := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-C", repo.Root}, args...)...)
		var output limitedOutput
		cmd.Stdout = &output
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("Git snapshot unavailable: %w: %.512s", err, stderr.String())
		}
		return output.Bytes(), nil
	}
	entry, err := read("ls-tree", "-z", *repo.Revision, "--", file.Path)
	if err != nil {
		return refuse(err.Error())
	}
	expected := *file.GitMode + " blob " + *file.GitBlob + "\t" + file.Path + "\x00"
	if string(entry) != expected {
		return refuse("index path does not match captured revision and blob")
	}
	size, err := read("cat-file", "-s", *file.GitBlob)
	if err != nil {
		return refuse(err.Error())
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
	if err != nil || n != *file.Bytes {
		return refuse("Git blob size differs from captured index")
	}
	data, err := read("cat-file", "blob", *file.GitBlob)
	if err != nil {
		return refuse(err.Error())
	}
	header := []byte(fmt.Sprintf("blob %d\x00", len(data)))
	object := append(header, data...)
	var actual string
	if len(*file.GitBlob) == 40 {
		sum := sha1.Sum(object)
		actual = hex.EncodeToString(sum[:])
	} else {
		sum := sha256.Sum256(object)
		actual = hex.EncodeToString(sum[:])
	}
	if actual != *file.GitBlob || int64(len(data)) != *file.Bytes {
		return refuse("Git blob content differs from captured identity")
	}
	if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		return refuse("captured file is not displayable UTF-8 text")
	}
	content := string(data)
	result.Status = "available"
	result.Content = &content
	return result
}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxBytes {
		return 0, fmt.Errorf("Git display output exceeds size bound")
	}
	return b.Buffer.Write(p)
}
