// Package execution binds native operations to one immutable project environment.
package execution

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type Context struct {
	root string
	env  map[string]string
}

func New(root string, environ []string) (*Context, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if canonical, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = canonical
	}
	e := &Context{root: filepath.Clean(root), env: make(map[string]string)}
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, 0) {
			return nil, fmt.Errorf("invalid context environment entry")
		}
		e.env[envKey(key)] = value
	}
	return e, nil
}

func envKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

// Capture is the standalone CLI boundary. Resident contexts use New instead.
func Capture(root string) *Context {
	e, err := New(root, os.Environ())
	if err != nil {
		panic(err)
	}
	return e
}

func (e *Context) Root() string { return e.root }
func (e *Context) Lookup(key string) (string, bool) {
	value, ok := e.env[envKey(key)]
	return value, ok
}
func (e *Context) Environ() []string {
	out := make([]string, 0, len(e.env))
	for key, value := range e.env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

func (e *Context) ResolvePath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, _ := e.Lookup("HOME")
		if home == "" && runtime.GOOS == "windows" {
			home, _ = e.Lookup("USERPROFILE")
		}
		if home == "" {
			return "", fmt.Errorf("context home directory is missing")
		}
		path = filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.root, path)
	}
	return filepath.Clean(path), nil
}

func (e *Context) Command(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	dir, dirErr := e.ResolvePath(dir)
	binary, err := e.lookPath(dir, name)
	if dirErr != nil {
		err = dirErr
	}
	// Use an absolute path even for failures: exec must never consult Host PATH.
	if binary == "" {
		binary = filepath.Join(e.root, ".noodle-missing-executable")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = dir, e.Environ()
	// Explicit Env disables os/exec's automatic PWD binding. Restore it per child.
	for i := 0; i < len(cmd.Env); i++ {
		key, _, _ := strings.Cut(cmd.Env[i], "=")
		if envKey(key) == envKey("PWD") {
			cmd.Env = append(cmd.Env[:i], cmd.Env[i+1:]...)
			i--
		}
	}
	cmd.Env = append(cmd.Env, "PWD="+dir)
	if err != nil {
		cmd.Err = err
	}
	return cmd
}

func (e *Context) lookPath(dir, name string) (string, error) {
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, "~") {
		path := name
		if strings.HasPrefix(name, "~") {
			var err error
			path, err = e.ResolvePath(name)
			if err != nil {
				return "", err
			}
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		return e.executable(path)
	}
	path, _ := e.Lookup("PATH")
	for _, item := range filepath.SplitList(path) {
		relative := !filepath.IsAbs(item)
		if item == "" {
			item = dir
		}
		if !filepath.IsAbs(item) {
			item = filepath.Join(dir, item)
		}
		if found, err := e.executable(filepath.Join(item, name)); err == nil {
			if relative {
				return "", &exec.Error{Name: name, Err: exec.ErrDot}
			}
			return found, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func (e *Context) executable(path string) (string, error) {
	candidates := []string{path}
	if runtime.GOOS == "windows" && filepath.Ext(path) == "" {
		ext, _ := e.Lookup("PATHEXT")
		if ext == "" {
			ext = ".COM;.EXE;.BAT;.CMD"
		}
		for _, suffix := range strings.Split(ext, ";") {
			candidates = append(candidates, path+strings.ToLower(suffix))
		}
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0) {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: path, Err: exec.ErrNotFound}
}
