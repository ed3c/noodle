package dispatcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strconv"

	"github.com/poteto/noodle/internal/instructionbundle"
)

type PreparedRemoteHarnessInput struct {
	Spawn, Startup, Input, Prompt []byte
	Instructions                  *instructionbundle.Prepared
}
type PreparedRemoteHarnessBinding struct {
	OrderID, SessionID, WorktreePath, Binary, InstructionVerifier string
}
type PreparedRemoteHarnessObservation struct {
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	ReasoningLevel        string `json:"reasoning_level"`
	GuestSessionDirectory string `json:"guest_session_directory"`
	InputBytes            int    `json:"input_bytes"`
	ArgvSHA256            string `json:"argv_sha256"`
}

// InspectPreparedRemoteHarness checks the original saved producer inputs. It
// resolves no current Skill, creates no session/startup/writer and grants no
// recovery eligibility. The caller binds and rechecks the files and Prepared
// materialization. A future native effect identity is deliberately not needed.
func InspectPreparedRemoteHarness(saved PreparedRemoteHarnessInput, b PreparedRemoteHarnessBinding) (result PreparedRemoteHarnessObservation, err error) {
	var spawn dispatchMetadata
	if err = json.Unmarshal(saved.Spawn, &spawn); err != nil {
		return result, err
	}
	if spawn.SessionID != b.SessionID || spawn.OrderID != b.OrderID || spawn.WorktreePath != b.WorktreePath || spawn.Provider != "codex" || spawn.Runtime != "openshell" || spawn.Model == "" || spawn.StageIndex == "" || spawn.ResumeSessionID != "" || spawn.ResumeThreadID != "" || spawn.ResumeSourceSHA != "" {
		return result, fmt.Errorf("saved remote harness identity/runtime differs")
	}
	if i, err := strconv.Atoi(spawn.StageIndex); err != nil || i < 0 {
		return result, fmt.Errorf("saved remote harness stage differs")
	}
	if b.SessionID == "" || path.Base(b.SessionID) != b.SessionID || b.SessionID == "." || b.SessionID == ".." || !path.IsAbs(b.Binary) || path.Clean(b.Binary) != b.Binary || len(saved.Prompt) == 0 {
		return result, fmt.Errorf("saved remote harness binding incomplete")
	}
	req := DispatchRequest{Runtime: spawn.Runtime, Provider: spawn.Provider, Model: spawn.Model, Skill: spawn.Skill, ReasoningLevel: spawn.ReasoningLevel, Prompt: string(saved.Prompt)}
	_, expected := composePrompts(req.Provider, req.Prompt, buildSessionPreamble(req), "")
	argv, err := preparedCodexCommand(req, b.Binary)
	if err != nil {
		return result, err
	}
	prepared := saved.Instructions
	if spawn.InstructionBundle != nil {
		if prepared == nil || prepared.ManifestSHA256 != spawn.InstructionBundle.SHA256 || !path.IsAbs(b.InstructionVerifier) || path.Clean(b.InstructionVerifier) != b.InstructionVerifier {
			return result, fmt.Errorf("saved remote harness instruction binding missing")
		}
		expected += "\n\n" + prepared.PromptSection()
		argv = append([]string{b.InstructionVerifier, "--directory", prepared.GuestDirectory, "--sha256", prepared.ManifestSHA256, "--order-id", b.OrderID, "--"}, argv...)
	} else if prepared != nil {
		return result, fmt.Errorf("saved instructions have no original spawn binding")
	}
	if !bytes.Equal(saved.Input, []byte(expected)) {
		return result, fmt.Errorf("saved remote harness input differs from original prompt/preamble/materialization")
	}
	var startup codexStartupReceipt
	if err = json.Unmarshal(saved.Startup, &startup); err != nil {
		return result, err
	}
	stdin := startup.Stdin
	if startup.SessionID != b.SessionID || stdin.ExpectedBytes != len(saved.Input) || stdin.WriteStartedAt != nil || stdin.WriteCompletedAt != nil || stdin.WrittenBytes != nil || stdin.CloseCompletedAt != nil || stdin.WriteError != "" || stdin.CloseError != "" || startup.FirstStdout != nil || startup.FirstStderr != nil || startup.FirstInitAt != nil || startup.NativeThreadID != "" || startup.NativeThreadSourceSHA256 != "" || startup.NativeThreadError != "" || startup.PersistenceError != "" {
		return result, fmt.Errorf("saved remote harness startup is not original unstarted input")
	}
	raw, err := json.Marshal(argv)
	if err != nil {
		return result, err
	}
	sum := sha256.Sum256(raw)
	return PreparedRemoteHarnessObservation{spawn.Provider, spawn.Model, spawn.ReasoningLevel, path.Join("/sandbox/noodle-sessions", b.SessionID), len(saved.Input), hex.EncodeToString(sum[:])}, nil
}
