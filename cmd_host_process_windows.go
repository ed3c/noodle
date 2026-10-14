//go:build windows

package main

import "os/exec"

func detachHostProcess(cmd *exec.Cmd) {}
