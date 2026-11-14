package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/instructionbundle"
)

type PreparedRemoteHarnessReopenBinding struct {
	PreparedRemoteHarnessBinding
	StageIndex  string
	Observation PreparedRemoteHarnessObservation
}

// ReopenPreparedRemoteHarness reconstructs only the original pre-Create Codex
// harness. The binding pins the original inspection and canonical stage. The
// caller retains responsibility for exclusive writer custody, v5 qualification,
// auth revision, quota, native readback and Runtime/Loop watcher transfer. Disk
// checks grant none of that authority and do not fence concurrent writers.
func ReopenPreparedRemoteHarness(runtimeDir string, saved PreparedRemoteHarnessInput, b PreparedRemoteHarnessReopenBinding) (*RemoteHarness, error) {
	if !filepath.IsAbs(runtimeDir) || filepath.Clean(runtimeDir) != runtimeDir || strings.TrimSpace(runtimeDir) != runtimeDir || b.OrderID == "" || !filepath.IsAbs(b.WorktreePath) || strings.ContainsAny(b.SessionID, `/\`) {
		return nil, fmt.Errorf("saved remote harness reopen binding incomplete")
	}
	if b.SessionID == "" || strings.TrimSpace(b.SessionID) != b.SessionID || b.SessionID == "." || b.SessionID == ".." || filepath.Base(b.SessionID) != b.SessionID {
		return nil, fmt.Errorf("saved remote harness session identity invalid")
	}
	dir, _, stamped, canonical, stderr := sessionPaths(runtimeDir, b.SessionID)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("read saved remote harness directory: %w", err)
	}
	if resolved != dir {
		return nil, fmt.Errorf("saved remote harness directory contains symlink")
	}
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"spawn.json", saved.Spawn}, {"codex-startup.json", saved.Startup},
		{"input.txt", saved.Input}, {"prompt.txt", saved.Prompt},
	} {
		data, err := readSavedHarnessFile(filepath.Join(dir, file.name))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(data, file.data) {
			return nil, fmt.Errorf("saved remote harness file differs: %s", file.name)
		}
	}
	var spawn dispatchMetadata
	if err := json.Unmarshal(saved.Spawn, &spawn); err != nil {
		return nil, fmt.Errorf("decode saved remote harness spawn: %w", err)
	}
	if b.StageIndex == "" || spawn.StageIndex != b.StageIndex {
		return nil, fmt.Errorf("saved remote harness stage differs from canonical stage")
	}
	prepared, err := reopenHarnessInstructions(dir, b.OrderID, spawn.InstructionBundle, saved.Instructions)
	if err != nil {
		return nil, err
	}
	// Use the verified disk mapping rather than caller-owned mutable pointers.
	saved.Instructions = prepared
	observed, err := InspectPreparedRemoteHarness(saved, b.PreparedRemoteHarnessBinding)
	if err != nil {
		return nil, err
	}
	if observed != b.Observation {
		return nil, fmt.Errorf("saved remote harness observation differs from original binding")
	}
	if err := checkSavedHarnessUnstarted(dir); err != nil {
		return nil, err
	}
	req := DispatchRequest{
		Runtime: spawn.Runtime, Provider: spawn.Provider, Model: spawn.Model,
		Skill: spawn.Skill, ReasoningLevel: spawn.ReasoningLevel,
		Prompt: string(saved.Prompt), WorktreePath: spawn.WorktreePath,
		InstructionBundle: spawn.InstructionBundle,
		EnvVars: map[string]string{"NOODLE_ORDER_ID": spawn.OrderID, "NOODLE_STAGE_INDEX": spawn.StageIndex},
		DispatchWarning: spawn.DispatchWarning, DisplayName: spawn.DisplayName,
		Title: spawn.Title, RetryCount: spawn.RetryCount, TaskKey: spawn.TaskKey,
		TaskRole: spawn.TaskRole, DegradedFromSessionID: spawn.DegradedFromSessionID,
		DegradationReason: spawn.DegradationReason,
	}
	argv, err := preparedCodexCommand(req, b.Binary)
	if err != nil {
		return nil, err
	}
	if prepared != nil {
		argv = append([]string{b.InstructionVerifier, "--directory", prepared.GuestDirectory, "--sha256", prepared.ManifestSHA256, "--order-id", b.OrderID, "--"}, argv...)
	}
	var receipt codexStartupReceipt
	if err := json.Unmarshal(saved.Startup, &receipt); err != nil {
		return nil, fmt.Errorf("decode saved Codex startup: %w", err)
	}
	writer, err := event.NewEventWriter(runtimeDir, b.SessionID)
	if err != nil {
		return nil, fmt.Errorf("open saved remote harness writer: %w", err)
	}
	// Resolved skill warnings had no saved before-image. Do not resolve today's
	// skill or invent warnings, and do not turn this diagnostic gap into a gate.
	return &RemoteHarness{
		ID: b.SessionID, Directory: dir, GuestSessionDir: observed.GuestSessionDirectory,
		Instructions: prepared, req: req, runtimeDir: runtimeDir, prompt: string(saved.Input),
		argv: argv, stamped: stamped, canonical: canonical, stderr: stderr, writer: writer,
		startup: &codexStartup{path: filepath.Join(dir, "codex-startup.json"), receipt: receipt},
	}, nil
}

func readSavedHarnessFile(name string) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect saved remote harness file %s: %w", filepath.Base(name), err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("saved remote harness file is not regular: %s", filepath.Base(name))
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read saved remote harness file %s: %w", filepath.Base(name), err)
	}
	return data, nil
}

func reopenHarnessInstructions(dir, order string, ref *InstructionBundleRef, saved *instructionbundle.Prepared) (*instructionbundle.Prepared, error) {
	name := filepath.Join(dir, "instruction-mapping.json")
	if ref == nil {
		if _, err := os.Lstat(name); !os.IsNotExist(err) || saved != nil {
			return nil, fmt.Errorf("saved remote harness instructions have no spawn binding")
		}
		return nil, nil
	}
	data, err := readSavedHarnessFile(name)
	if err != nil {
		return nil, err
	}
	var mapping struct {
		OriginalManifest InstructionBundleRef        `json:"original_manifest"`
		Prepared         *instructionbundle.Prepared `json:"prepared"`
	}
	if err := json.Unmarshal(data, &mapping); err != nil {
		return nil, fmt.Errorf("decode saved remote harness instruction mapping: %w", err)
	}
	p := mapping.Prepared
	directory := filepath.Join(dir, "instructions-"+ref.SHA256)
	manifest, err := instructionbundle.Verify(context.Background(), directory, ref.SHA256, order)
	if err != nil {
		return nil, fmt.Errorf("verify saved remote harness instructions: %w", err)
	}
	if mapping.OriginalManifest != *ref || p == nil || p.Directory != directory || p.GuestDirectory != "/sandbox/"+filepath.Base(directory) || p.ManifestSHA256 != ref.SHA256 || !reflect.DeepEqual(p.Manifest, manifest) || !reflect.DeepEqual(p, saved) {
		return nil, fmt.Errorf("saved remote harness instruction mapping differs")
	}
	return p, nil
}

func checkSavedHarnessUnstarted(dir string) error {
	for _, name := range []string{"raw.ndjson", "canonical.ndjson", "stderr.log", "events.ndjson", "heartbeat.json", "process.json", "native.json"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect saved remote harness start evidence %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() != 0 || name == "heartbeat.json" || name == "process.json" || name == "native.json" {
			return fmt.Errorf("saved remote harness has start evidence: %s", name)
		}
	}
	return nil
}
