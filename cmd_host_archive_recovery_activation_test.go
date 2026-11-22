package main

import (
	"os"
	"path/filepath"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree/archive"
)

func TestArchiveRecoveryActivationAuthenticatesLargeBinary(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "openshell-gateway")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = file.Truncate((64 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	digest, err := archive.FileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	ref := loopruntime.SessionReadbackFileRef{Path: path, SHA256: digest}
	if err = verifyArchiveRecoveryActivationBinary(ref); err != nil {
		t.Fatalf("original large executable refused: %v", err)
	}
	if _, err = file.WriteAt([]byte{'N'}, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err = verifyArchiveRecoveryActivationBinary(ref); err == nil {
		t.Fatal("changed executable accepted")
	}
}
