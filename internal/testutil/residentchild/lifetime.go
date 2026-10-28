package residentchild

import (
	"fmt"
	"os"
	"time"
)

// Wait bounds a test fixture to its original parent and cooperative exit marker.
func Wait() error {
	parent := os.Getppid()
	for parent > 1 && os.Getppid() == parent {
		_, err := os.Stat("exit")
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("resident fixture exit marker: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}
