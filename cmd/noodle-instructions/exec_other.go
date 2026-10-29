//go:build !unix

package main

import "fmt"

func replaceProcess([]string) error {
	return fmt.Errorf("instruction verifier process replacement unsupported on this platform")
}
