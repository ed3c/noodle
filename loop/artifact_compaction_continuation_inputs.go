package loop

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/poteto/noodle/internal/guestprogress"
	"github.com/poteto/noodle/internal/instructionbundle"
	loopruntime "github.com/poteto/noodle/runtime"
)

// Freeze original materialized inputs and retained archives; no upload or native
// execution is inferred. The original CLI validates their runtime observation.
func artifactCompactionOriginalInputs(dir, orderID string, bundle *instructionbundle.FileRef, progress *guestprogress.Receipt, progressSource *loopruntime.SessionReadbackFileRef, refs map[string]loopruntime.SessionReadbackFileRef) error {
	read := func(name, path, expected string) error {
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			return err
		}
		if expected != "" && ref.SHA256 != expected {
			return fmt.Errorf("artifact continuation original input changed: %s", name)
		}
		refs[name] = ref
		return nil
	}
	if bundle != nil {
		directory := filepath.Join(dir, "instructions-"+bundle.SHA256)
		manifest, err := instructionbundle.Verify(context.Background(), directory, bundle.SHA256, orderID)
		if err != nil {
			return err
		}
		for name, path := range map[string]string{"original_instruction_manifest": bundle.Path, "instruction_manifest": filepath.Join(directory, instructionbundle.ManifestName)} {
			if err := read(name, path, bundle.SHA256); err != nil {
				return err
			}
		}
		for _, name := range []string{"instruction-mapping.json", "input.txt"} {
			if err := read(name, filepath.Join(dir, name), ""); err != nil {
				return err
			}
		}
		for i, file := range manifest.Files {
			if err := read(fmt.Sprintf("instruction_file_%d", i), filepath.Join(directory, filepath.FromSlash(file.Destination)), file.Source.SHA256); err != nil {
				return err
			}
		}
	}
	if progress == nil && progressSource == nil {
		return nil
	}
	if progress == nil || progressSource == nil || len(progress.Archives) != 2 {
		return fmt.Errorf("artifact continuation retained progress source is incomplete")
	}
	if err := read("progress_receipt", progressSource.Path, progressSource.SHA256); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, archive := range progress.Archives {
		if (archive.Name != "noodle-result.tar" && archive.Name != "noodle-session.tar") || seen[archive.Name] || archive.Bytes <= 0 {
			return fmt.Errorf("artifact continuation retained archive identity differs")
		}
		seen[archive.Name] = true
		paths, err := filepath.Glob(filepath.Join(dir, "progress-archive-*", archive.Name))
		if err != nil {
			return err
		}
		if len(paths) != 1 {
			return fmt.Errorf("artifact continuation retained archive is missing or ambiguous")
		}
		if err := read("archive_"+archive.Name, paths[0], archive.SHA256); err != nil {
			return err
		}
	}
	return nil
}
