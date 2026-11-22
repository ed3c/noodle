package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveInspectionRejectsScratchThroughOriginalAlias(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original")
	if err := os.MkdirAll(filepath.Join(original, "scratch"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(original, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceInspectionTempRoot(filepath.Join(alias, "scratch"), original); err == nil || !strings.Contains(err.Error(), "overlaps original") {
		t.Fatalf("scratch path can write into original source: %v", err)
	}
	caseAlias := filepath.Join(root, "ORIGINAL", "scratch")
	if _, err := os.Stat(caseAlias); err == nil {
		if _, err := workspaceInspectionTempRoot(caseAlias, original); err == nil || !strings.Contains(err.Error(), "overlaps original") {
			t.Fatalf("case-folded ancestry can write into original source: %v", err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := workspaceInspectionTempRoot(root, original); err != nil {
		t.Fatal("external scratch root was refused", err)
	}
}

func TestWorkspaceCaptureInspectionPinsOriginalReceipt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "workspace-capture.json")
	if err = os.WriteFile(name, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := ReadSessionReadbackFile(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, []byte("{\"schema\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadOpenShellWorkspaceCapture(ref); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("changed capture was accepted under the original FileRef: %v", err)
	}
}
