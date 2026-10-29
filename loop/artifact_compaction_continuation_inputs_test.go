package loop

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/guestprogress"
	"github.com/poteto/noodle/internal/instructionbundle"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestArtifactCompactionOwnerRetainsOriginalInputs(t *testing.T) {
	l, request, dir := artifactOwnerFixture(t)
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ref := func(path string) loopruntime.SessionReadbackFileRef {
		t.Helper()
		r, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	update := func(name, key string, value any) {
		t.Helper()
		path := filepath.Join(dir, name)
		var v map[string]any
		if _, err := loopruntime.ReadSessionReadbackFile(path, &v); err != nil {
			t.Fatal(err)
		}
		v[key] = value
		recoveryWriteJSON(t, path, v)
	}

	source := filepath.Join(dir, "frozen", "SKILL.md")
	write(source, []byte("Read the original admitted task instructions.\n"))
	sourceRef := ref(source)
	mode := uint32(0600)
	manifestPath := filepath.Join(dir, "original-instructions.json")
	recoveryWriteJSON(t, manifestPath, instructionbundle.Manifest{
		SchemaVersion: 1, OrderID: request.OrderID,
		Files: []instructionbundle.File{{
			Source:         instructionbundle.FileRef{Path: sourceRef.Path, SHA256: sourceRef.SHA256},
			OriginalSource: instructionbundle.FileRef{Path: sourceRef.Path, SHA256: sourceRef.SHA256},
			Destination:    "skills/build/SKILL.md", Mode: &mode,
		}},
		Bindings: []instructionbundle.Binding{{Kind: "selected_skill", Name: "build", Path: "skills/build/SKILL.md"}},
	})
	manifestRef := ref(manifestPath)
	bundle := instructionbundle.FileRef{Path: manifestRef.Path, SHA256: manifestRef.SHA256}
	prepared, err := instructionbundle.Prepare(t.Context(), bundle, request.OrderID, dir)
	if err != nil {
		t.Fatal(err)
	}
	recoveryWriteJSON(t, filepath.Join(dir, "instruction-mapping.json"), map[string]any{"original_manifest": bundle, "prepared": prepared})
	write(filepath.Join(dir, "input.txt"), []byte("Original task\n"+prepared.PromptSection()))
	update("spawn.json", "instruction_bundle", bundle)

	progress := guestprogress.Receipt{
		Schema: 1, Identity: guestprogress.Identity{SessionID: request.SessionID,
			CommandSHA256: recoveryDigest([]byte("original command")), TokenSHA256: recoveryDigest([]byte("original token"))},
		RequestID: "11111111-1111-4111-8111-111111111111", Status: "archived", ChildrenReaped: true,
	}
	want := map[string]loopruntime.SessionReadbackFileRef{
		"original_instruction_manifest": manifestRef,
		"instruction_manifest":          ref(filepath.Join(prepared.Directory, instructionbundle.ManifestName)),
		"instruction_file_0":            ref(filepath.Join(prepared.Directory, "skills", "build", "SKILL.md")),
		"instruction-mapping.json":      ref(filepath.Join(dir, "instruction-mapping.json")),
		"input.txt":                     ref(filepath.Join(dir, "input.txt")),
	}
	for _, name := range []string{"noodle-result.tar", "noodle-session.tar"} {
		var raw bytes.Buffer
		archive := tar.NewWriter(&raw)
		body := []byte("original retained " + name + "\n")
		if err := archive.WriteHeader(&tar.Header{Name: "original.txt", Mode: 0600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "progress-archive-original", name)
		write(path, raw.Bytes())
		r := ref(path)
		want["archive_"+name] = r
		progress.Archives = append(progress.Archives, guestprogress.Archive{Name: name, SHA256: r.SHA256, Bytes: int64(raw.Len())})
	}
	progressPath := filepath.Join(dir, "progress-archive-original", "result.json")
	recoveryWriteJSON(t, progressPath, progress)
	want["progress_receipt"] = ref(progressPath)
	update("openshell-dispatch.json", "Progress", progress)
	update("openshell-dispatch.json", "ProgressSource", want["progress_receipt"])
	read := func() (ArtifactCompactionOwner, error) {
		return ReadArtifactCompactionOwner(l.projectDir, l.config, request.OrderID, request.SessionID, "original-context", "pause-original")
	}
	owner, err := read()
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range want {
		if got := owner.Sources[name]; got != expected {
			t.Fatalf("original evidence %s was not retained: got %+v, want %+v", name, got, expected)
		}
	}

	// Mapping and prompt bytes are frozen as refs here; their native semantics
	// belong to the original CLI. Exercise hash-bound inputs at this boundary.
	for _, name := range []string{"original_instruction_manifest", "instruction_manifest", "instruction_file_0", "progress_receipt", "archive_noodle-result.tar", "archive_noodle-session.tar"} {
		path := want[name].Path
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"altered", "missing"} {
			t.Run(name+"/"+change, func(t *testing.T) {
				t.Cleanup(func() { write(path, original) })
				if change == "altered" {
					write(path, append(append([]byte(nil), original...), '\n'))
				} else if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if _, err := read(); err == nil {
					t.Fatalf("%s original evidence accepted: %s", change, name)
				}
			})
		}
	}
	if _, err := read(); err != nil {
		t.Fatalf("restored original evidence rejected: %v", err)
	}
}
