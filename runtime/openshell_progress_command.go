package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/poteto/noodle/internal/guestprogress"
)

type openShellProgressBinding struct{ Binary, Directory, SessionID, CommandSHA256, SessionDirectory, Token, ArchivePolicy string }

func progressDirectory(session string) string {
	sum := sha256.Sum256([]byte(session))
	return "/sandbox/noodle-progress-" + hex.EncodeToString(sum[:12])
}

func wrapOpenShellProgress(command OpenShellCommand, binary, session, token string) (OpenShellCommand, error) {
	return wrapOpenShellProgressIn(command, binary, session, token, progressDirectory(session))
}

func wrapOpenShellProgressIn(command OpenShellCommand, binary, session, token, directory string) (OpenShellCommand, error) {
	return wrapOpenShellProgressWithPolicy(command, binary, session, token, directory, "")
}

func wrapOpenShellProgressWithPolicy(command OpenShellCommand, binary, session, token, directory, policy string) (OpenShellCommand, error) {
	if _, err := guestprogress.ParseArchivePolicy(policy); err != nil {
		return OpenShellCommand{}, err
	}
	if err := command.validate(); err != nil {
		return OpenShellCommand{}, err
	}
	if !strings.HasPrefix(binary, "/sandbox/payload/") || path.Clean(binary) != binary || session == "" || path.Base(session) != session || session == "." || session == ".." || len(token) != 64 || command.Env["NOODLE_SESSION_DIR"] == "" || command.Env["NOODLE_PROGRESS_TOKEN"] != "" {
		return OpenShellCommand{}, fmt.Errorf("OpenShell progress launch identity invalid")
	}
	if !path.IsAbs(directory) || path.Clean(directory) != directory || path.Base(directory) != path.Base(progressDirectory(session)) {
		return OpenShellCommand{}, fmt.Errorf("OpenShell progress directory differs from its original session")
	}
	digest, err := OpenShellCommandSHA256(command)
	if err != nil {
		return OpenShellCommand{}, err
	}
	if policy != "" {
		bound := sha256.Sum256([]byte("noodle-progress-archive-policy-v1\x00" + digest + "\x00" + policy))
		digest = hex.EncodeToString(bound[:])
	}
	wrapped := command
	wrapped.Env = maps.Clone(command.Env)
	wrapped.Env["NOODLE_PROGRESS_TOKEN"] = token
	wrapped.Argv = []string{binary, "run", "--directory", directory, "--session-id", session, "--command-sha256", digest, "--repository", command.Workdir, "--session-directory", command.Env["NOODLE_SESSION_DIR"]}
	if policy != "" {
		wrapped.Argv = append(wrapped.Argv, "--archive-policy", policy)
	}
	wrapped.Argv = append(append(wrapped.Argv, "--"), command.Argv...)
	return wrapped, nil
}

func unwrapOpenShellProgress(command OpenShellCommand) (OpenShellCommand, openShellProgressBinding, error) {
	var binding openShellProgressBinding
	a := command.Argv
	if len(a) < 14 || a[1] != "run" || a[2] != "--directory" || a[4] != "--session-id" || a[6] != "--command-sha256" || a[8] != "--repository" || a[10] != "--session-directory" {
		return OpenShellCommand{}, binding, fmt.Errorf("original native exec has no bound progress supervisor")
	}
	start, policy := 13, ""
	if a[12] == "--archive-policy" && len(a) >= 16 && a[14] == "--" && a[13] != "" {
		start, policy = 15, a[13]
	} else if a[12] != "--" {
		return OpenShellCommand{}, binding, fmt.Errorf("original progress archive policy syntax differs")
	}
	base := command
	base.Argv = slices.Clone(a[start:])
	base.Env = maps.Clone(command.Env)
	token := base.Env["NOODLE_PROGRESS_TOKEN"]
	delete(base.Env, "NOODLE_PROGRESS_TOKEN")
	expected, err := wrapOpenShellProgressWithPolicy(base, a[0], a[5], token, a[3], policy)
	if err != nil {
		return OpenShellCommand{}, binding, err
	}
	if !slices.Equal(expected.Argv, command.Argv) || !maps.Equal(expected.Env, command.Env) {
		return OpenShellCommand{}, binding, fmt.Errorf("original progress supervisor command binding differs")
	}
	binding = openShellProgressBinding{Binary: a[0], Directory: a[3], SessionID: a[5], CommandSHA256: a[7], SessionDirectory: a[11], Token: token, ArchivePolicy: policy}
	return base, binding, nil
}

// The caller supplies only a native UUID. The original model command determines
// the executable, token, workdir and session; no new arbitrary argv is accepted.
func progressStopCommand(original OpenShellCommand, requestID string) (OpenShellCommand, error) {
	_, binding, err := unwrapOpenShellProgress(original)
	if err != nil {
		return OpenShellCommand{}, err
	}
	command := OpenShellCommand{RequestID: requestID, Workdir: original.Workdir, Env: map[string]string{"NOODLE_PROGRESS_TOKEN": binding.Token}, Argv: []string{binding.Binary, "stop", "--directory", binding.Directory, "--session-id", binding.SessionID, "--command-sha256", binding.CommandSHA256, "--request-id", requestID}}
	return command, command.validate()
}
