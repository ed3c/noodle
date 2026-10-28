//go:build linux

package guestprogress

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The helper is a separate process so its subreaper and Wait4 never capture
// children belonging to the test runner or other concurrent tests.
func TestGuestProgressProcess(t *testing.T) {
	mode := os.Getenv("NOODLE_PROGRESS_TEST_CHILD")
	if mode == "" {
		return
	}
	root := os.Getenv("NOODLE_PROGRESS_TEST_ROOT")
	switch mode {
	case "supervisor":
		var cfg Config
		if err := readJSON(filepath.Join(root, "config.json"), &cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(91)
		}
		_ = os.Setenv("NOODLE_PROGRESS_TEST_CHILD", "harness")
		fault := os.Getenv("NOODLE_PROGRESS_TEST_FAULT")
		ops := supervisorOperations{writeJSON: writeJSON, signalChildren: signalOwnedChildren}
		if fault != "" {
			ops.writeJSON = func(directory, name string, value any) error {
				if name == fault {
					return fmt.Errorf("injected persistence failure: %s", name)
				}
				return writeJSON(directory, name, value)
			}
			ops.signalChildren = func(signal unix.Signal) error {
				if _, err := os.Stat(filepath.Join(root, "release-signal")); err != nil {
					return syscall.EPERM
				}
				return signalOwnedChildren(signal)
			}
		}
		code, err := runWithOperations(cfg, ops)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(92)
		}
		os.Exit(code)
	case "harness":
		if err := os.WriteFile(filepath.Join(root, "session", "transcript.json"), []byte(`{"progress":"saved"}`), 0600); err != nil {
			os.Exit(96)
		}
		command := exec.Command(os.Args[0], "-test.run=^TestGuestProgressProcess$")
		command.Env = replaceTestMode(os.Environ(), "writer")
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := command.Start(); err != nil {
			os.Exit(93)
		}
		_ = os.WriteFile(filepath.Join(root, "harness-ready"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for {
			time.Sleep(time.Hour)
		}
	case "writer":
		signal.Ignore(syscall.SIGTERM)
		_ = os.WriteFile(filepath.Join(root, "escaped-pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for i := 0; ; i++ {
			temporary := filepath.Join(root, "repo", "progress.pending")
			if err := os.WriteFile(temporary, []byte(fmt.Sprintf("preserved %d", i)), 0600); err != nil {
				os.Exit(94)
			}
			if err := os.Rename(temporary, filepath.Join(root, "repo", "progress.txt")); err != nil {
				os.Exit(94)
			}
			time.Sleep(time.Millisecond)
		}
	default:
		os.Exit(95)
	}
}

func replaceTestMode(environment []string, mode string) []string {
	result := []string{}
	for _, value := range environment {
		if !strings.HasPrefix(value, "NOODLE_PROGRESS_TEST_CHILD=") {
			result = append(result, value)
		}
	}
	return append(result, "NOODLE_PROGRESS_TEST_CHILD="+mode)
}

func waitFixtureFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fixture not ready: %s", path)
}

func TestStopReapsEscapedWriterBeforeArchiving(t *testing.T) {
	root, err := os.MkdirTemp("", "ngp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("preserved fixture: %s", root)
		} else {
			_ = os.RemoveAll(root)
		}
	})
	for _, name := range []string{"repo", "session"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	identity, token := fixtureIdentity()
	cfg := Config{Directory: filepath.Join(root, "control"), Repository: filepath.Join(root, "repo"), SessionDirectory: filepath.Join(root, "session"), Identity: identity, Token: token, Argv: []string{os.Args[0], "-test.run=^TestGuestProgressProcess$"}, Grace: 100 * time.Millisecond}
	if err := writeJSON(root, "config.json", cfg); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(root, "supervisor.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestGuestProgressProcess$")
	command.Env = append(replaceTestMode(os.Environ(), "supervisor"), "NOODLE_PROGRESS_TEST_ROOT="+root, "NOODLE_PROGRESS_TOKEN="+token)
	command.Stdout = output
	command.Stderr = output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	request := Request{Schema: 1, Identity: identity, Token: token, RequestID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	joined := false
	t.Cleanup(func() {
		if !joined {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = Stop(ctx, cfg.Directory, request)
			cancel()
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	waitFixtureFile(t, filepath.Join(root, "harness-ready"))
	waitFixtureFile(t, filepath.Join(root, "escaped-pid"))
	waitFixtureFile(t, filepath.Join(root, "repo", "progress.txt"))
	wrong := request
	wrong.Token = strings.Repeat("d", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err = Stop(ctx, cfg.Directory, wrong); err == nil {
		t.Fatal("wrong token stopped the harness")
	}
	if _, err = os.Stat(filepath.Join(cfg.Directory, "stop.json")); !os.IsNotExist(err) {
		t.Fatal("rejected request created stop intent")
	}
	result, err := Stop(ctx, cfg.Directory, request)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(root, "supervisor.log"))
		t.Fatalf("stop: %v; %s", err, log)
	}
	if err = command.Wait(); err == nil {
		t.Fatal("interrupted harness was reported successful")
	}
	joined = true
	if result.ExitCode != 143 || !result.ChildrenReaped {
		t.Fatalf("exit evidence: %+v", result)
	}
	archived := map[string]map[string][]byte{}
	for _, archive := range result.Archives {
		file, err := os.Open(filepath.Join(cfg.Directory, archive.Name))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil || digest(data) != archive.SHA256 || int64(len(data)) != archive.Bytes {
			t.Fatal("archive digest or size differs")
		}
		entries := map[string][]byte{}
		reader := tar.NewReader(bytes.NewReader(data))
		for {
			header, readErr := reader.Next()
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			contents, readErr := io.ReadAll(reader)
			if readErr != nil {
				t.Fatal(readErr)
			}
			entries[header.Name] = contents
		}
		archived[archive.Name] = entries
	}
	before, err := os.ReadFile(filepath.Join(root, "repo", "progress.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 || !bytes.Equal(archived["noodle-result.tar"]["progress.txt"], before) {
		t.Fatal("archive omitted or changed the final guest progress")
	}
	if string(archived["noodle-session.tar"]["transcript.json"]) != `{"progress":"saved"}` {
		t.Fatal("archive omitted or changed the session transcript")
	}
	pidBytes, err := os.ReadFile(filepath.Join(root, "escaped-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	// ECHILD is the completion proof; this additionally checks the fixture's
	// detached writer is absent rather than merely outside its original group.
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("escaped writer still present: %v", err)
	}
	again, err := Stop(ctx, cfg.Directory, request)
	if err != nil || again.RequestID != result.RequestID {
		t.Fatalf("idempotent stop: %+v %v", again, err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "repo", "progress.txt"))
	if string(before) != string(after) {
		t.Fatal("writer changed progress after archival")
	}
	data, _ := json.Marshal(result)
	t.Logf("native Linux progress proof: %s", data)
}

func TestPostLaunchFailureRetainsOwnerUntilReaped(t *testing.T) {
	for _, fault := range []string{"process.json", "stop.json", "signal"} {
		t.Run(fault, func(t *testing.T) {
			root, err := os.MkdirTemp("", "ngp-f-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("preserved fixture: %s", root)
				} else {
					_ = os.RemoveAll(root)
				}
			})
			for _, name := range []string{"repo", "session"} {
				if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			identity, token := fixtureIdentity()
			cfg := Config{Directory: filepath.Join(root, "control"), Repository: filepath.Join(root, "repo"), SessionDirectory: filepath.Join(root, "session"), Identity: identity, Token: token, Argv: []string{os.Args[0], "-test.run=^TestGuestProgressProcess$"}, Grace: 100 * time.Millisecond}
			if err := writeJSON(root, "config.json", cfg); err != nil {
				t.Fatal(err)
			}
			output, err := os.Create(filepath.Join(root, "supervisor.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			command := exec.Command(os.Args[0], "-test.run=^TestGuestProgressProcess$")
			command.Env = append(replaceTestMode(os.Environ(), "supervisor"), "NOODLE_PROGRESS_TEST_ROOT="+root, "NOODLE_PROGRESS_TEST_FAULT="+fault)
			command.Stdout, command.Stderr = output, output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			joined := false
			t.Cleanup(func() {
				if !joined {
					_ = os.WriteFile(filepath.Join(root, "release-signal"), nil, 0600)
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						_ = command.Process.Kill()
						<-done
					}
				}
			})
			waitFixtureFile(t, filepath.Join(root, "repo", "progress.txt"))
			if fault != "process.json" {
				waitFixtureFile(t, filepath.Join(cfg.Directory, "process.json"))
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_, err := Stop(ctx, cfg.Directory, Request{Schema: 1, Identity: identity, Token: token, RequestID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
				cancel()
				if err == nil {
					t.Fatal("failed drain reported complete")
				}
			}
			waitFixtureFile(t, filepath.Join(cfg.Directory, "failure.json"))
			select {
			case err := <-done:
				joined = true
				t.Fatalf("owner exited with live descendants: %v", err)
			default:
			}
			var failure Receipt
			if err := readJSON(filepath.Join(cfg.Directory, "failure.json"), &failure); err != nil {
				t.Fatal(err)
			}
			if failure.ChildrenReaped || failure.Status != "unknown" {
				t.Fatal("failure claimed a completed drain")
			}
			if _, err := os.Stat(filepath.Join(cfg.Directory, "result.json")); !os.IsNotExist(err) {
				t.Fatal("failure published success")
			}
			if err := os.WriteFile(filepath.Join(root, "release-signal"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				joined = true
				if err == nil {
					t.Fatal("post-launch failure reported success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owner failed to finish after descendant drain")
			}
			if err := readJSON(filepath.Join(cfg.Directory, "failure.json"), &failure); err != nil {
				t.Fatal(err)
			}
			if !failure.ChildrenReaped || failure.Error == "" {
				t.Fatal("final failure omitted drain evidence")
			}
			pidBytes, err := os.ReadFile(filepath.Join(root, "escaped-pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(pidBytes))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("descendant survived: %v", err)
			}
		})
	}
}
