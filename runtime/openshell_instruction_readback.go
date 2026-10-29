package runtime

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/instructionbundle"
)

// The manifest bounds the transported files without expanding thousands of
// file references into the Host response. Verify repeats after native readback.
type CompletedInstructionEvidence struct {
	Directory      string                 `json:"directory"`
	GuestDirectory string                 `json:"guest_directory"`
	Manifest       SessionReadbackFileRef `json:"manifest"`
}

func (r *OpenShellRuntime) instructionReadback(ctx context.Context, dir string, b CompletedSessionBinding, ref *dispatcher.InstructionBundleRef, command OpenShellCommand) (*CompletedInstructionEvidence, map[string]SessionReadbackFileRef, error) {
	if r.config.Profile.ProgressSupervisor != "" {
		base, progress, err := unwrapOpenShellProgress(command)
		if err != nil || progress.Binary != r.config.Profile.ProgressSupervisor || progress.SessionID != b.SessionID {
			return nil, nil, fmt.Errorf("original progress supervisor binding differs")
		}
		command = base
	}
	mappingPath := filepath.Join(dir, "instruction-mapping.json")
	if ref == nil {
		route, ok := r.config.Profile.Routes[b.Provider]
		if !ok || len(command.Argv) == 0 || command.Argv[0] != route.Binary {
			return nil, nil, fmt.Errorf("original native command requires missing instruction binding")
		}
		if _, err := os.Lstat(mappingPath); !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("instruction mapping has no original spawn binding")
		}
		return nil, nil, nil
	}
	directory := filepath.Join(dir, "instructions-"+ref.SHA256)
	guest := path.Join("/sandbox", filepath.Base(directory))
	manifest, err := instructionbundle.Verify(ctx, directory, ref.SHA256, b.OrderID)
	if err != nil {
		return nil, nil, err
	}
	var mapping struct {
		OriginalManifest dispatcher.InstructionBundleRef `json:"original_manifest"`
		Prepared         *instructionbundle.Prepared     `json:"prepared"`
	}
	mappingRef, err := ReadSessionReadbackFile(mappingPath, &mapping)
	if err != nil {
		return nil, nil, err
	}
	p := mapping.Prepared
	if mapping.OriginalManifest != *ref || p == nil || p.Directory != directory || p.GuestDirectory != guest || p.ManifestSHA256 != ref.SHA256 || !reflect.DeepEqual(p.Manifest, manifest) {
		return nil, nil, fmt.Errorf("original instruction mapping differs from transported inputs")
	}
	prefix, prefixErr := openShellInstructionCommandPrefix(r.config.Profile, b.Provider, b.OrderID, ref.SHA256)
	if prefixErr != nil || len(command.Argv) < len(prefix) || !slices.Equal(command.Argv[:len(prefix)], prefix) {
		return nil, nil, fmt.Errorf("original native command did not bind the instruction verifier")
	}
	manifestRef, err := ReadSessionReadbackFile(filepath.Join(directory, instructionbundle.ManifestName), nil)
	if err != nil {
		return nil, nil, err
	}
	if manifestRef.SHA256 != ref.SHA256 {
		return nil, nil, fmt.Errorf("transported instruction manifest changed during readback")
	}
	inputPath := filepath.Join(dir, "input.txt")
	inputRef, err := ReadSessionReadbackFile(inputPath, nil)
	if err != nil {
		return nil, nil, err
	}
	input, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, nil, err
	}
	if !strings.Contains(string(input), p.PromptSection()) {
		return nil, nil, fmt.Errorf("original harness input lacks the bound guest instruction mapping")
	}
	return &CompletedInstructionEvidence{Directory: directory, GuestDirectory: guest, Manifest: manifestRef}, map[string]SessionReadbackFileRef{"instruction_mapping": mappingRef, "instruction_manifest": manifestRef, "instruction_input": inputRef}, nil
}
