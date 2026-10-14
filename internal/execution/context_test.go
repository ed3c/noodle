package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommandsUsePrivatePathCWDAndEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	t.Setenv("PRIVATE_TOKEN", "host")
	before, _ := os.Getwd()
	for _, token := range []string{"first", "second"} {
		root := t.TempDir()
		bin := filepath.Join(root, "bin")
		if err := os.Mkdir(bin, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "probe"), []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$PWD\" \"$PRIVATE_TOKEN\" \"$HOME\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		env := []string{"PATH=" + bin, "HOME=" + root, "PRIVATE_TOKEN=" + token}
		e, err := New(root, env)
		if err != nil {
			t.Fatal(err)
		}
		env[2] = "PRIVATE_TOKEN=mutated"
		copy := e.Environ()
		copy[0] = "HOME=mutated"
		cmd := e.Command(context.Background(), root, "probe")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != root+"|"+token+"|"+root {
			t.Fatalf("wrong environment: %s", out)
		}
		if cmd.Path != filepath.Join(bin, "probe") {
			t.Fatalf("host PATH selected %s", cmd.Path)
		}
		got, err := e.ResolvePath("~/skills")
		if err != nil || got != filepath.Join(root, "skills") {
			t.Fatalf("home path: %s %v", got, err)
		}
	}
	if after, _ := os.Getwd(); after != before || os.Getenv("PRIVATE_TOKEN") != "host" {
		t.Fatal("mutated process environment")
	}
}

func TestCommandPreservesErrDotForImplicitRelativeExecutables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	root := t.TempDir()
	tool := filepath.Join(root, "probe")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s' ran"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".", ":" + root, ".:" + root} {
		e, err := New(root, []string{"PATH=" + path})
		if err != nil {
			t.Fatal(err)
		}
		out, err := e.Command(context.Background(), root, "probe").Output()
		if !errors.Is(err, exec.ErrDot) || len(out) != 0 {
			t.Fatalf("PATH %q implicitly executed local tool: %q %v", path, out, err)
		}
		out, err = e.Command(context.Background(), root, "./probe").Output()
		if err != nil || string(out) != "ran" {
			t.Fatalf("explicit relative tool rejected: %q %v", out, err)
		}
	}
	e, err := New(root, []string{"PATH=" + root})
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Command(context.Background(), root, "probe").Output()
	if err != nil || string(out) != "ran" {
		t.Fatalf("absolute PATH tool rejected: %q %v", out, err)
	}
}

func TestMissingContextPathCannotInheritHostExecutable(t *testing.T) {
	e, err := New(t.TempDir(), []string{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := e.Command(context.Background(), e.Root(), "go", "version")
	if cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "not found") {
		t.Fatalf("ambient executable accepted: %+v", cmd)
	}
	if cmd.Env == nil {
		t.Fatal("nil environment inherits Host")
	}
	if _, ok := e.Lookup("HOME"); ok {
		t.Fatal("inherited HOME")
	}
}

func TestCommandBindsPWDtoItsDirectoryWithoutMutatingContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "logical-root")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	e, err := New(root, []string{"PATH=" + os.Getenv("PATH"), "PWD=/wrong-context"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Command(context.Background(), alias, "sh", "-c", `printf '%s' "$PWD"`).Output()
	if err != nil || string(out) != alias {
		t.Fatalf("child PWD: %q %v", out, err)
	}
	other := t.TempDir()
	out, err = e.Command(context.Background(), other, "sh", "-c", `printf '%s' "$PWD"`).Output()
	if err != nil || string(out) != other {
		t.Fatalf("second child PWD: %q %v", out, err)
	}
	if value, _ := e.Lookup("PWD"); value != "/wrong-context" {
		t.Fatal("command changed context snapshot")
	}
}
