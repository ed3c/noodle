//go:build !darwin || !cgo

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/execution"
)

func TestHostCheckoutArchiveUnsupportedBuildDefersBeforePolicySetup(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := execution.New(root, []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := prepareHostCheckoutArchive(context.Background(), e)
	if err == nil || policy != nil || !strings.Contains(err.Error(), "original archive support is unavailable") {
		t.Fatal("unsupported metadata build claimed policy readiness", policy, err)
	}
	for _, dir := range []string{root, home} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatal("unsupported policy setup created effects", dir, entries, err)
		}
	}
}
