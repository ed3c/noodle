//go:build !darwin && !linux

package admission

func preparationWaitLockHeld(string, int, []byte) bool { return false }
