//go:build windows

package archive

import "os/exec"

func isolateCommand(*exec.Cmd) {}
