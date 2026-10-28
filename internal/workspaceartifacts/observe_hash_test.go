package workspaceartifacts

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRawBlobIdentityMatchesGitObjectFormats(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			testGit(t, root, "init", "--quiet", "--object-format="+format)
			for _, data := range [][]byte{nil, []byte("line\r\n"), {0, 255, 1, 0}, bytes.Repeat([]byte("content"), 10000)} {
				expected, err := git(t.Context(), root, data, "hash-object", "--stdin", "--no-filters")
				if err != nil {
					t.Fatal(err)
				}
				id := strings.TrimSpace(string(expected))
				if !matchesRawBlob(data, id) || matchesRawBlob(append(append([]byte{}, data...), 1), id) {
					t.Fatalf("raw byte identity differs from Git %s", format)
				}
			}
		})
	}
	if matchesRawBlob(nil, "invalid") {
		t.Fatal("invalid object identity accepted")
	}
}

func TestReadCleanHeadDoesNotRequirePerFileGitProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable Git fixture")
	}
	root, _ := fixture(t)
	expected := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = hash-object ]; then exit 97; fi\ndone\nexec '" + strings.ReplaceAll(realGit, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	head, err := ReadCleanHead(context.Background(), root)
	if err != nil || head != expected {
		t.Fatalf("observation still depends on per-file Git invocation: %q %v", head, err)
	}
	put(t, root, "file.txt", []byte("changed bytes"), 0644)
	if _, err := ReadCleanHead(t.Context(), root); err == nil {
		t.Fatal("raw drift accepted")
	}
}

func TestReadCleanHeadSHA256RepositoryAndCancellation(t *testing.T) {
	root := t.TempDir()
	testGit(t, root, "init", "--quiet", "--object-format=sha256")
	testGit(t, root, "config", "user.name", "Fixture")
	testGit(t, root, "config", "user.email", "fixture@example.invalid")
	put(t, root, "file.txt", []byte("original\x00bytes\n"), 0644)
	testGit(t, root, "add", "file.txt")
	testGit(t, root, "commit", "--quiet", "-m", "original fixture")
	expected := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
	if got, err := ReadCleanHead(t.Context(), root); err != nil || got != expected {
		t.Fatalf("SHA256 observation: %q %v", got, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadCleanHead(cancelled, root); err == nil {
		t.Fatal("cancelled observation reported success")
	}
	put(t, root, "file.txt", []byte("changed\x00bytes\n"), 0644)
	if _, err := ReadCleanHead(t.Context(), root); err == nil {
		t.Fatal("SHA256 raw drift accepted")
	}
}
