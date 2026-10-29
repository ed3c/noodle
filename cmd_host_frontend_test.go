//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeFrontendHelper(t *testing.T) {
	args := os.Getenv("NOODLE_TEST_FRONTEND_ARGS")
	if args == "" {
		return
	}
	var command []string
	if err := json.Unmarshal([]byte(args), &command); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{os.Args[0]}, command...)
	if err := selectNativeFrontend(); err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.WriteString("ORIGINAL_BINARY\n")
	os.Exit(0)
}

func TestNativeFrontendPinsCapabilitiesAndRunningWorker(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "selected-noodle")
	script := []byte("#!/bin/sh\nprintf 'SELECTED_BINARY\\n'\n")
	if err = os.WriteFile(target, script, 0700); err != nil {
		t.Fatal(err)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err = updateHostPool(root, func(p *hostPool) error {
		p.Active = id
		p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), Binary: target, SHA: hostDigest(script), State: "active"}
		old := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		p.Generations[old] = hostGeneration{ID: old, Directory: filepath.Join(root, "g", old), Binary: self, SHA: hostDigest(data), State: "retiring"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args   []string
		worker bool
		want   string
	}{
		{[]string{"host", "capabilities"}, false, "ORIGINAL_BINARY"},
		{[]string{"host", "task", "capabilities"}, false, "ORIGINAL_BINARY"},
		{[]string{"host", "task", "review-capabilities"}, false, "ORIGINAL_BINARY"},
		{[]string{"host", "task", "session-readback"}, false, "ORIGINAL_BINARY"},
		{[]string{"version"}, false, "SELECTED_BINARY"},
		{[]string{"version"}, true, "ORIGINAL_BINARY"},
	} {
		cmd := exec.Command(self, "-test.run=^TestNativeFrontendHelper$")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "NOODLE_HOST_DIR=") && !strings.HasPrefix(entry, "NOODLE_SESSION_ID=") && !strings.HasPrefix(entry, "NOODLE_TEST_FRONTEND_ARGS=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		args, _ := json.Marshal(tc.args)
		cmd.Env = append(cmd.Env, "NOODLE_HOST_DIR="+root, "NOODLE_TEST_FRONTEND_ARGS="+string(args))
		if tc.worker {
			cmd.Env = append(cmd.Env, "NOODLE_SESSION_ID=fixture-original-round")
		}
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Fatalf("args=%v worker=%v: %s %v", tc.args, tc.worker, out, err)
		}
	}
}
