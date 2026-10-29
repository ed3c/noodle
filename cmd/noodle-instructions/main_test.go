package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/poteto/noodle/internal/instructionbundle"
)

func TestVerifierChild(t *testing.T) {
	if os.Getenv("NOODLE_INSTRUCTION_VERIFIER_CHILD") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("NOODLE_INSTRUCTION_ARGS")), &args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(1)
}

func TestExecMarkerChild(t *testing.T) {
	if os.Getenv("NOODLE_INSTRUCTION_MARKER_CHILD") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("NOODLE_INSTRUCTION_MARKER"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func testSHA(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func TestGuestVerifierExecutesOnlyVerifiedBundleInSamePID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix exec contract")
	}
	for _, change := range []string{"valid", "missing", "bytes", "mode", "extra", "manifest"} {
		t.Run(change, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			frozen := filepath.Join(root, "SKILL.md")
			body := []byte("frozen instructions\n")
			if err := os.WriteFile(frozen, body, 0600); err != nil {
				t.Fatal(err)
			}
			mode := uint32(0600)
			manifest := instructionbundle.Manifest{SchemaVersion: 1, OrderID: "original-order", Files: []instructionbundle.File{{Source: instructionbundle.FileRef{Path: frozen, SHA256: testSHA(body)}, OriginalSource: instructionbundle.FileRef{Path: "/unavailable/original/SKILL.md", SHA256: testSHA(body)}, Destination: "skills/build/SKILL.md", Mode: &mode}}, Bindings: []instructionbundle.Binding{{Kind: "selected_skill", Name: "build", Path: "skills/build/SKILL.md"}}}
			b, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			ref := instructionbundle.FileRef{Path: filepath.Join(root, "bundle.json"), SHA256: testSHA(b)}
			if err := os.WriteFile(ref.Path, b, 0600); err != nil {
				t.Fatal(err)
			}
			prepared, err := instructionbundle.Prepare(t.Context(), ref, manifest.OrderID, root)
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(prepared.Directory, "skills/build/SKILL.md")
			switch change {
			case "missing":
				err = os.Remove(name)
			case "bytes":
				err = os.WriteFile(name, []byte("changed"), 0600)
			case "mode":
				err = os.Chmod(name, 0700)
			case "extra":
				err = os.WriteFile(filepath.Join(prepared.Directory, "extra"), []byte("extra"), 0600)
			case "manifest":
				err = os.WriteFile(filepath.Join(prepared.Directory, "manifest.json"), []byte("{}"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"--directory", prepared.Directory, "--sha256", ref.SHA256, "--order-id", manifest.OrderID, "--", executable, "-test.run=^TestExecMarkerChild$"}
			encoded, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "executed")
			cmd := exec.Command(executable, "-test.run=^TestVerifierChild$")
			cmd.Env = append(os.Environ(), "NOODLE_INSTRUCTION_VERIFIER_CHILD=1", "NOODLE_INSTRUCTION_MARKER_CHILD=1", "NOODLE_INSTRUCTION_MARKER="+marker, "NOODLE_INSTRUCTION_ARGS="+string(encoded))
			out, err := cmd.CombinedOutput()
			if change != "valid" {
				if err == nil {
					t.Fatalf("invalid bundle launched: %s", out)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("verification failure executed harness")
				}
				return
			}
			if err != nil {
				t.Fatalf("valid bundle refused: %v %s", err, out)
			}
			pid, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if string(pid) != strconv.Itoa(cmd.Process.Pid) {
				t.Fatal("verifier launched a second process rather than replacing itself")
			}
		})
	}
}
