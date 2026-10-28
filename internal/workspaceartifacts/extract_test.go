package workspaceartifacts

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type tarFixtureEntry struct {
	header tar.Header
	body   string
}

func archiveTestDirectory(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeArchive(t *testing.T, root string, entries []tarFixtureEntry) string {
	t.Helper()
	file, err := os.Create(filepath.Join(root, "repo.tar"))
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for _, entry := range entries {
		header := entry.header
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			header.Size = int64(len(entry.body))
		}
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func TestExtractRoundTripIntoApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires executable bits and symlink creation")
	}
	target, base := fixture(t)
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Capture(context.Background(), target, filepath.Join(base, "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	archive := writeArchive(t, base, []tarFixtureEntry{
		{header: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0755}},
		{header: tar.Header{Name: "./bin/", Typeflag: tar.TypeDir, Mode: 0755}},
		{header: tar.Header{Name: "./bin/program", Typeflag: tar.TypeReg, Mode: 0755}, body: "\x00\xff\x01binary\x00"},
		{header: tar.Header{Name: "./.gitignore", Typeflag: tar.TypeReg, Mode: 0644}, body: "ignored\n.env\n"},
		{header: tar.Header{Name: "./link", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "bin/program"}},
		{header: tar.Header{Name: "./bin/relative", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "../.gitignore"}},
	})
	returned := filepath.Join(base, "returned")
	if err := Extract(context.Background(), archive, returned); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(returned)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("destination mode: %v %v", info, err)
	}
	expected, _, err := scanDirectory(returned)
	if err != nil {
		t.Fatal(err)
	}
	if expected["bin/program"].Mode != "100755" || expected["link"].Mode != "120000" {
		t.Fatalf("modes lost: %#v", expected)
	}
	result, err := Apply(context.Background(), target, b, returned, filepath.Join(base, "delivery"))
	if err != nil || !result.Applied {
		t.Fatalf("apply result=%+v error=%v", result, err)
	}
	actual, _, err := scanRepository(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("contents differ: %#v vs %#v", actual, expected)
	}
	if _, err := os.Lstat(filepath.Join(target, "file.txt")); !os.IsNotExist(err) {
		t.Fatal("missing returned file was not deleted")
	}
}

func TestExtractRejectsAllUnsafeHeadersBeforeCreatingDestination(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarFixtureEntry
	}{
		{"parent path", []tarFixtureEntry{{header: tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644}, body: "evil"}}},
		{"cleanable parent", []tarFixtureEntry{{header: tar.Header{Name: "./safe/../escape", Typeflag: tar.TypeReg, Mode: 0644}, body: "evil"}}},
		{"absolute path", []tarFixtureEntry{{header: tar.Header{Name: "/escape", Typeflag: tar.TypeReg, Mode: 0644}, body: "evil"}}},
		{"metadata", []tarFixtureEntry{{header: tar.Header{Name: "./.git/config", Typeflag: tar.TypeReg, Mode: 0644}, body: "evil"}}},
		{"private env", []tarFixtureEntry{{header: tar.Header{Name: "./.env", Typeflag: tar.TypeReg, Mode: 0600}, body: "secret"}}},
		{"duplicate alias", []tarFixtureEntry{{header: tar.Header{Name: "./same", Typeflag: tar.TypeReg, Mode: 0644}}, {header: tar.Header{Name: "same", Typeflag: tar.TypeReg, Mode: 0644}}}},
		{"duplicate root", []tarFixtureEntry{{header: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0755}}, {header: tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0755}}}},
		{"hardlink", []tarFixtureEntry{{header: tar.Header{Name: "link", Typeflag: tar.TypeLink, Mode: 0644, Linkname: "safe"}}}},
		{"fifo", []tarFixtureEntry{{header: tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0644}}}},
		{"device", []tarFixtureEntry{{header: tar.Header{Name: "device", Typeflag: tar.TypeChar, Mode: 0644}}}},
		{"setuid", []tarFixtureEntry{{header: tar.Header{Name: "binary", Typeflag: tar.TypeReg, Mode: 04755}}}},
		{"external symlink", []tarFixtureEntry{{header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "../escape"}}}},
		{"absolute symlink", []tarFixtureEntry{{header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "/escape"}}}},
		{"traversal symlink", []tarFixtureEntry{{header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "ignored/../safe"}}}},
		{"symlink chain", []tarFixtureEntry{{header: tar.Header{Name: "link1", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "link2"}}, {header: tar.Header{Name: "link2", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "safe"}}}},
		{"symlink ancestor", []tarFixtureEntry{{header: tar.Header{Name: "link/child", Typeflag: tar.TypeReg, Mode: 0644}, body: "evil"}, {header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "safe"}}}},
		{"symlink target ancestor", []tarFixtureEntry{{header: tar.Header{Name: "link1", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "link2/child"}}, {header: tar.Header{Name: "link2", Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "safe"}}}},
		{"file ancestor", []tarFixtureEntry{{header: tar.Header{Name: "safe/child", Typeflag: tar.TypeReg, Mode: 0644}}, {header: tar.Header{Name: "safe", Typeflag: tar.TypeReg, Mode: 0644}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := archiveTestDirectory(t)
			// A valid entry first proves validation doesn't extract a prefix.
			entries := append([]tarFixtureEntry{{header: tar.Header{Name: "valid", Typeflag: tar.TypeReg, Mode: 0644}, body: "ok"}}, test.entries...)
			archive := writeArchive(t, root, entries)
			destination := filepath.Join(root, "returned")
			if err := Extract(context.Background(), archive, destination); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("destination created before complete validation: %v", err)
			}
		})
	}
}

func TestExtractRejectsArchiveSymlinkAndParentSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink creation")
	}
	root := archiveTestDirectory(t)
	archive := writeArchive(t, root, nil)
	if err := os.Symlink(archive, filepath.Join(root, "archive-link")); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), filepath.Join(root, "archive-link"), filepath.Join(root, "returned")); err == nil {
		t.Fatal("archive symlink accepted")
	}
	if err := os.Symlink(root, filepath.Join(root, "parent-link")); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), archive, filepath.Join(root, "parent-link", "returned")); err == nil {
		t.Fatal("parent symlink accepted")
	}
	if _, err := os.Lstat(filepath.Join(root, "returned")); !os.IsNotExist(err) {
		t.Fatal("destination created through symlink")
	}
}

func TestExtractRejectsHiddenTrailingArchive(t *testing.T) {
	root := archiveTestDirectory(t)
	archive := writeArchive(t, root, nil)
	file, err := os.OpenFile(archive, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	if err := writer.WriteHeader(&tar.Header{Name: "../hidden", Typeflag: tar.TypeReg, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "returned")
	if err := Extract(context.Background(), archive, destination); err == nil || !strings.Contains(err.Error(), "after end marker") {
		t.Fatalf("hidden archive accepted: %v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("invalid archive extracted")
	}
}

func TestExtractLimitsBeforeWriting(t *testing.T) {
	t.Run("file bytes", func(t *testing.T) {
		root := archiveTestDirectory(t)
		file, err := os.Create(filepath.Join(root, "repo.tar"))
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(file)
		if err := writer.WriteHeader(&tar.Header{Name: "huge", Typeflag: tar.TypeReg, Mode: 0644, Size: maxArchivePayload + 1}); err != nil {
			t.Fatal(err)
		}
		// Deliberately no body: the limit must fail before attempting to read it.
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, "returned")
		if err := Extract(context.Background(), file.Name(), destination); err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("size limit missed: %v", err)
		}
		if _, err := os.Lstat(destination); !os.IsNotExist(err) {
			t.Fatal("oversized archive extracted")
		}
	})
	t.Run("entry count", func(t *testing.T) {
		root := archiveTestDirectory(t)
		file, err := os.Create(filepath.Join(root, "repo.tar"))
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(file)
		for i := 0; i <= maxArchiveEntries; i++ {
			if err := writer.WriteHeader(&tar.Header{Name: fmt.Sprintf("entry-%d", i), Typeflag: tar.TypeReg, Mode: 0644}); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, "returned")
		if err := Extract(context.Background(), file.Name(), destination); err == nil || !strings.Contains(err.Error(), "entries") {
			t.Fatalf("entry limit missed: %v", err)
		}
		if _, err := os.Lstat(destination); !os.IsNotExist(err) {
			t.Fatal("oversized archive extracted")
		}
	})
}

func TestExtractCancellationAndExistingDestination(t *testing.T) {
	root := archiveTestDirectory(t)
	archive := writeArchive(t, root, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	destination := filepath.Join(root, "returned")
	if err := Extract(ctx, archive, destination); err == nil {
		t.Fatal("cancelled extraction accepted")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("cancelled extraction created destination")
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, destination, "keep", []byte("local"), 0600)
	if err := Extract(context.Background(), archive, destination); err == nil {
		t.Fatal("existing destination accepted")
	}
	if data, err := os.ReadFile(filepath.Join(destination, "keep")); err != nil || string(data) != "local" {
		t.Fatal("existing destination modified")
	}
}

func TestExtractRejectsTruncatedEndMarker(t *testing.T) {
	root := archiveTestDirectory(t)
	archive := writeArchive(t, root, []tarFixtureEntry{{header: tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644}, body: "body"}})
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(archive, info.Size()-1024); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "returned")
	if err := Extract(context.Background(), archive, destination); err == nil || !strings.Contains(err.Error(), "end marker") {
		t.Fatalf("truncated archive accepted: %v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("truncated archive extracted")
	}
}

func TestArchiveSecondPassRejectsDrift(t *testing.T) {
	for _, change := range []string{"body", "header", "entry count", "padding"} {
		t.Run(change, func(t *testing.T) {
			root := archiveTestDirectory(t)
			archive := writeArchive(t, root, []tarFixtureEntry{{header: tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644}, body: "original"}})
			before, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			entries, firstHash, err := readArchive(context.Background(), bytes.NewReader(before), nil, "")
			if err != nil {
				t.Fatal(err)
			}
			name, body := "file", "original"
			switch change {
			case "body":
				body = "modified"
			case "header":
				name = "different"
			}
			updated := []tarFixtureEntry{{header: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644}, body: body}}
			if change == "entry count" {
				updated = nil
			}
			writeArchive(t, root, updated)
			after, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			if change == "padding" {
				after = append(after, make([]byte, 512)...)
			}
			destination := filepath.Join(root, "returned")
			if err := os.Mkdir(destination, 0700); err != nil {
				t.Fatal(err)
			}
			_, secondHash, err := readArchive(context.Background(), bytes.NewReader(after), entries, destination)
			if change == "padding" {
				if err != nil || firstHash == secondHash {
					t.Fatalf("padding drift missed: %v", err)
				}
			} else if err == nil {
				t.Fatal("second-pass drift accepted")
			}
		})
	}
}
