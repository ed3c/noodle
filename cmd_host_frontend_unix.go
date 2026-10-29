//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Existing workers and explicit binary introspection retain their version.
// New external CLI invocations use the native selected version before parsing.
func selectNativeFrontend() error {
	if os.Getenv("NOODLE_SESSION_ID") != "" {
		return nil
	}
	for i := 1; i+1 < len(os.Args); i++ {
		if os.Args[i] == "host" && (os.Args[i+1] == "serve" || os.Args[i+1] == "capabilities" || os.Args[i+1] == "switch") {
			return nil
		}
		if i+2 < len(os.Args) && os.Args[i] == "host" && os.Args[i+1] == "task" && (os.Args[i+2] == "capabilities" || os.Args[i+2] == "review-capabilities" || os.Args[i+2] == "session-readback") {
			return nil
		}
	}
	root, err := hostDirectory()
	if err != nil {
		return err
	}
	p, err := readHostPool(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	target, ok := p.Generations[p.Active]
	if !ok || target.State != "active" {
		return errors.New("selected Host version is unresolved")
	}
	current, err := os.Executable()
	if err != nil {
		return err
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return err
	}
	if current == target.Binary {
		return nil
	}
	known := false
	for _, g := range p.Generations {
		if g.Binary == current {
			data, err := os.ReadFile(current)
			if err != nil {
				return err
			}
			if hostDigest(data) != g.SHA {
				return errors.New("original CLI binary changed")
			}
			known = true
			break
		}
	}
	if !known {
		return nil
	}
	data, err := os.ReadFile(target.Binary)
	if err != nil {
		return err
	}
	if hostDigest(data) != target.SHA {
		return errors.New("selected CLI binary changed")
	}
	return syscall.Exec(target.Binary, append([]string{target.Binary}, os.Args[1:]...), os.Environ())
}
