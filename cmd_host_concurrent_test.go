package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/loop"
)

type environmentHostRuntime struct {
	fakeHostRuntime
	execution *execution.Context
	input     io.WriteCloser
	observed  string
}

func (f *environmentHostRuntime) Run(ctx context.Context) error {
	cmd := f.execution.Command(ctx, f.execution.Root(), "probe")
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	f.input = input
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	f.observed, err = bufio.NewReader(output).ReadString('\n')
	if err != nil {
		input.Close()
		cmd.Wait()
		return err
	}
	close(f.ready)
	return cmd.Wait()
}
func (f *environmentHostRuntime) Release(context.Context) error { return f.input.Close() }

func TestHostConcurrentNativeChildrenKeepPrivateEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	t.Setenv("CONTEXT_SECRET", "host")
	before, _ := os.Getwd()
	h := &noodleHost{dir: t.TempDir(), instanceID: "concurrent", binarySHA: "test-binary"}
	var runtimes []*environmentHostRuntime
	h.newRuntime = func(_, _ string, _ config.Config, deps loop.Dependencies) hostRuntime {
		f := &environmentHostRuntime{fakeHostRuntime: fakeHostRuntime{ready: make(chan struct{})}, execution: deps.Execution}
		runtimes = append(runtimes, f)
		return f
	}
	var responses []hostResponse
	for _, secret := range []string{"alpha", "beta"} {
		root := t.TempDir()
		bin := filepath.Join(root, "bin")
		if err := os.Mkdir(bin, 0700); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\nprintf '%s|%s|%s\\n' \"$PWD\" \"$HOME\" \"$CONTEXT_SECRET\"\nread release || :\n"
		if err := os.WriteFile(filepath.Join(bin, "probe"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		req := hostTestRequest(t, root)
		req.Environment = []string{"PATH=" + bin, "HOME=" + root, "CONTEXT_SECRET=" + secret}
		r := h.handle(context.Background(), req)
		if r.Status != "ok" {
			t.Fatalf("attach: %+v", r)
		}
		responses = append(responses, r)
		canonical, _ := filepath.EvalSymlinks(root)
		if got := runtimes[len(runtimes)-1].observed; got != canonical+"|"+root+"|"+secret+"\n" {
			t.Fatalf("child observed %q", got)
		}
	}
	if len(h.contexts) != 2 || responses[0].HostPID != responses[1].HostPID || responses[0].ContextID == responses[1].ContextID {
		t.Fatal("contexts did not coexist in one Host")
	}
	if cwd, _ := os.Getwd(); cwd != before || os.Getenv("CONTEXT_SECRET") != "host" {
		t.Fatal("Host process state changed")
	}
	for _, r := range responses {
		got := h.handle(context.Background(), hostRequest{Operation: "detach", ProjectDir: r.ProjectDir, ContextID: r.ContextID, InstanceID: r.HostInstanceID, BinarySHA256: h.binarySHA})
		if got.Status != "ok" || got.ContextID != r.ContextID || got.ContextState != "detached" {
			t.Fatalf("wrong completion owner: %+v", got)
		}
	}
	for _, r := range h.receipts {
		if r.ContextState != "detached" {
			t.Fatal("receipt overwritten")
		}
	}
}

func TestHostRejectsRemoteBeforeRuntimeAndReceipts(t *testing.T) {
	for _, name := range []string{"sprites", "cursor"} {
		t.Run(name, func(t *testing.T) {
			h := &noodleHost{dir: t.TempDir(), instanceID: "local", binarySHA: "test-binary", newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime {
				t.Fatal("refused remote request constructed runtime")
				return nil
			}}
			req := hostTestRequest(t, t.TempDir())
			data, err := readHostConfig(req.ProjectDir)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, []byte("\n[runtime]\ndefault='"+name+"'\n")...)
			if err := os.WriteFile(filepath.Join(req.ProjectDir, config.DefaultConfigPath), data, 0600); err != nil {
				t.Fatal(err)
			}
			req.ConfigSHA256 = hostDigest(data)
			got := h.handle(context.Background(), req)
			if got.Status != "refused" || !strings.Contains(got.Error, "confirmed remote completion") {
				t.Fatalf("remote: %+v", got)
			}
			if len(h.receipts) != 0 || len(h.contexts) != 0 {
				t.Fatal("refused remote request accepted custody")
			}
			if _, err := os.Stat(filepath.Join(h.dir, "receipts.json")); !os.IsNotExist(err) {
				t.Fatalf("remote receipt exists: %v", err)
			}
		})
	}
}

func TestHostRecoveryPreflightRefusesLiveAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		pid          int
		wantErr      bool
	}{
		{"live-terminal", "completed", os.Getpid(), true}, {"unknown", "running", 0, true}, {"finished", "completed", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			session := filepath.Join(dir, "sessions", "fixture")
			os.MkdirAll(session, 0700)
			data, _ := json.Marshal(map[string]string{"status": tc.status})
			os.WriteFile(filepath.Join(session, "meta.json"), data, 0600)
			if tc.pid != 0 {
				data, _ = json.Marshal(map[string]int{"pid": tc.pid})
				os.WriteFile(filepath.Join(session, "process.json"), data, 0600)
			}
			if err := hostRecoveryPreflight(dir); (err != nil) != tc.wantErr {
				t.Fatalf("preflight: %v", err)
			}
		})
	}
}

func TestHostFailureDoesNotBlockSiblingOrReplay(t *testing.T) {
	h := &noodleHost{dir: t.TempDir(), instanceID: "siblings", binarySHA: "test-binary"}
	h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime {
		return &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
	}
	a := hostTestRequest(t, t.TempDir())
	b := hostTestRequest(t, t.TempDir())
	ra := h.handle(context.Background(), a)
	rb := h.handle(context.Background(), b)
	if ra.Status != "ok" || rb.Status != "ok" {
		t.Fatalf("attach: %+v %+v", ra, rb)
	}
	c := h.contexts[ra.ContextID]
	c.cancel()
	err := <-c.done
	h.finish(c, err)
	defer c.lock.Close()
	if got := h.handle(context.Background(), a); got.Status != "unknown_effect" {
		t.Fatalf("failed request replay: %+v", got)
	}
	status := hostRequest{Operation: "status", ProjectDir: b.ProjectDir, ContextID: rb.ContextID, InstanceID: rb.HostInstanceID, BinarySHA256: h.binarySHA}
	if got := h.handle(context.Background(), status); got.Status != "ok" || got.ContextState != "active" {
		t.Fatalf("sibling blocked: %+v", got)
	}
	status.Operation = "detach"
	if got := h.handle(context.Background(), status); got.Status != "ok" {
		t.Fatalf("sibling release: %+v", got)
	}
}

func TestHostContextLimitAndSameRootRefuseBeforeRuntime(t *testing.T) {
	h := &noodleHost{dir: t.TempDir(), instanceID: "limit", binarySHA: "test-binary", contexts: make(map[string]*hostContext)}
	for i := 0; i < 16; i++ {
		id := fmt.Sprint(i)
		h.contexts[id] = &hostContext{receipt: hostReceipt{ContextID: id, ProjectDir: "/fixture/" + id}}
	}
	req := hostTestRequest(t, t.TempDir())
	if got := h.handle(context.Background(), req); got.Status != "busy" || !strings.Contains(got.Error, "16") {
		t.Fatalf("context limit: %+v", got)
	}
	h.contexts = map[string]*hostContext{"same": {receipt: hostReceipt{ContextID: "same", ProjectDir: req.ProjectDir}}}
	if got := h.handle(context.Background(), req); got.Status != "busy" || !strings.Contains(got.Error, "project") {
		t.Fatalf("same root: %+v", got)
	}
	if len(h.receipts) != 0 {
		t.Fatal("refusal persisted an accepted receipt")
	}
}
