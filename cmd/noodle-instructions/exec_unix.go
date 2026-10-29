//go:build unix

package main

import (
	"os"
	"syscall"
)

func replaceProcess(argv []string) error { return syscall.Exec(argv[0], argv, os.Environ()) }
