package instructionbundle

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func toolFixture(t *testing.T) (string, Manifest) {
	t.Helper()
	root, m := fixture(t)
	data := bytes.Repeat([]byte{17}, MaxFileBytes+1)
	name := filepath.Join(root, "tool.tar")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion = 2
	m.ToolFiles = []File{{Source: FileRef{Path: name, SHA256: digest(data)}, OriginalSource: FileRef{Path: "/original/tool.tar", SHA256: digest(data)}, Destination: "tools/tool-1/tool.tar", Mode: mode(0600)}}
	m.Bindings = append(m.Bindings, Binding{Kind: "tool", Name: "tool-1", Path: m.ToolFiles[0].Destination})
	return root, m
}

func TestToolPayloadUsesOriginalTransferAndGuestVerification(t *testing.T) {
	root, m := toolFixture(t)
	ref := writeManifest(t, root, m)
	p, err := Prepare(t.Context(), ref, m.OrderID, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.ToolFiles[0].Source.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), p.Directory, ref.SHA256, m.OrderID); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(p.PromptSection()), []byte("kind=tool")) {
		t.Fatal("guest tool mapping missing")
	}
	if err := os.WriteFile(filepath.Join(p.Directory, m.ToolFiles[0].Destination), []byte("drift"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), p.Directory, ref.SHA256, m.OrderID); err == nil {
		t.Fatal("tool payload drift accepted")
	}
}

func TestToolPayloadCannotRaiseInstructionBudgetOrBecomeSkill(t *testing.T) {
	for _, change := range []string{"instruction", "namespace", "binding", "schema", "oversize"} {
		t.Run(change, func(t *testing.T) {
			root, m := toolFixture(t)
			switch change {
			case "instruction":
				m.SchemaVersion = 1
				m.ToolFiles[0].Destination = "sources/large.tar"
				m.Files = append(m.Files, m.ToolFiles[0])
				m.ToolFiles = nil
				m.Bindings = m.Bindings[:len(m.Bindings)-1]
			case "namespace":
				m.ToolFiles[0].Destination = "skills/build/tool.tar"
			case "binding":
				m.Bindings[len(m.Bindings)-1].Kind = "source"
			case "schema":
				m.SchemaVersion = 1
			case "oversize":
				f, err := os.OpenFile(m.ToolFiles[0].Source.Path, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(MaxToolFileBytes + 1); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ref := writeManifest(t, root, m)
			if _, err := Prepare(t.Context(), ref, m.OrderID, root); err == nil {
				t.Fatal("invalid tool boundary accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "instructions-"+ref.SHA256)); !os.IsNotExist(err) {
				t.Fatal("refused preparation published files")
			}
		})
	}
}
