package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/reducer"
)

type hostThreadProfileSource struct {
	unknown map[string]json.RawMessage
	Thread  string `json:"thread_id"`
	Path    string `json:"path"`
	SHA     string `json:"metadata_sha256"`
}
type hostExecutionProfile struct {
	unknown map[string]json.RawMessage
	Kind    string                    `json:"kind"`
	Threads []hostThreadProfileSource `json:"threads"`
}

// Explicit selection supplies a new execution profile; it never recreates the
// original transport environment or changes that admission's identity.
func selectHostExecutionProfile(ctx context.Context, req hostRequest, snapshot reducer.DurableSnapshot) (hostContextInputs, hostExecutionProfile, error) {
	inputs, err := captureHostContextInputs(req)
	profile := hostExecutionProfile{Kind: "selected_current_profile"}
	if err != nil {
		return inputs, profile, err
	}
	for _, entry := range req.Environment {
		if strings.HasPrefix(entry, "CODEX_HOME=") {
			inputs.NativeHome = strings.TrimPrefix(entry, "CODEX_HOME=")
		}
	}
	if inputs.NativeHome == "" {
		inputs.NativeHome = filepath.Join(inputs.Home, ".codex")
	}
	if !filepath.IsAbs(inputs.NativeHome) {
		return inputs, profile, fmt.Errorf("selected native home is not absolute")
	}
	inputs.NativeAuth = false
	if info, readErr := os.Stat(filepath.Join(inputs.NativeHome, "auth.json")); readErr == nil {
		if !info.Mode().IsRegular() {
			return inputs, profile, fmt.Errorf("selected native auth source is not a file")
		}
		inputs.NativeAuth = true
	} else if !os.IsNotExist(readErr) {
		return inputs, profile, readErr
	}
	if raw, readErr := os.ReadFile(filepath.Join(inputs.NativeHome, "config.toml")); readErr == nil {
		inputs.NativeConfigSHA = hostDigest(raw)
	} else if !os.IsNotExist(readErr) {
		return inputs, profile, readErr
	}
	for _, order := range snapshot.State.Orders {
		for _, stage := range order.Stages {
			for _, attempt := range stage.Attempts {
				if err = ctx.Err(); err != nil {
					return inputs, profile, err
				}
				var thread, logical string
				if stage.Runtime == dispatcher.CodexNativeRuntime {
					record, readErr := dispatcher.ReadReleasedNativeSession(ctx, filepath.Join(req.ProjectDir, ".noodle"), attempt.SessionID)
					if readErr != nil {
						return inputs, profile, readErr
					}
					if record.NativeHome != inputs.NativeHome {
						return inputs, profile, fmt.Errorf("native resume requires its original native home; selected profile differs")
					}
					thread, logical = record.ThreadID, record.Worktree
				} else {
					if err = verifyControllerProcessAttempt(filepath.Join(req.ProjectDir, ".noodle"), attempt.SessionID); err != nil {
						return inputs, profile, err
					}
					raw, readErr := os.ReadFile(filepath.Join(req.ProjectDir, ".noodle", "sessions", attempt.SessionID, "spawn.json"))
					if readErr != nil {
						return inputs, profile, readErr
					}
					var spawn struct {
						Worktree string `json:"worktree_path"`
						Provider string `json:"provider"`
					}
					if json.Unmarshal(raw, &spawn) != nil || spawn.Provider != "codex" {
						return inputs, profile, fmt.Errorf("selected profile cannot resume the original provider")
					}
					logical = spawn.Worktree
					thread, err = dispatcher.ReadCodexProcessThread(filepath.Join(req.ProjectDir, ".noodle"), attempt.SessionID)
					if err != nil {
						return inputs, profile, err
					}
				}
				source, readErr := findHostThreadProfileSource(ctx, inputs.NativeHome, thread, logical)
				if readErr != nil {
					return inputs, profile, readErr
				}
				profile.Threads = append(profile.Threads, source)
			}
		}
	}
	if len(profile.Threads) == 0 {
		return inputs, profile, fmt.Errorf("original task has no source-bound native thread history")
	}
	return inputs, profile, nil
}

func findHostThreadProfileSource(ctx context.Context, home, thread, logical string) (hostThreadProfileSource, error) {
	var selected hostThreadProfileSource
	if thread == "" || strings.ContainsAny(thread, "/\\") {
		return selected, fmt.Errorf("original native thread identity invalid")
	}
	for _, name := range []string{"sessions", "archived_sessions"} {
		base := filepath.Join(home, name)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) && path == base {
					return fs.SkipDir
				}
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), thread+".jsonl") {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("native thread history is not a regular file")
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 4096), 1<<20)
			if !scanner.Scan() {
				file.Close()
				return fmt.Errorf("native thread metadata missing")
			}
			line := append([]byte{}, scanner.Bytes()...)
			file.Close()
			var record struct {
				Type    string `json:"type"`
				Payload struct {
					ID  string `json:"id"`
					CWD string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &record) != nil || record.Type != "session_meta" || record.Payload.ID != thread || record.Payload.CWD != logical {
				return fmt.Errorf("native thread history differs from original task/worktree")
			}
			if selected.Path != "" {
				return fmt.Errorf("native thread profile source is ambiguous")
			}
			selected = hostThreadProfileSource{Thread: thread, Path: path, SHA: hostDigest(line)}
			return nil
		})
		if err != nil {
			return selected, err
		}
	}
	if selected.Path == "" {
		return selected, fmt.Errorf("selected execution profile lacks original native thread %s", thread)
	}
	return selected, nil
}
