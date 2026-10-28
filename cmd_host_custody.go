package main

import (
	"errors"
	"os"

	"github.com/poteto/noodle/internal/filex"
)

func saveHostStorageBytes(path string, data []byte) error {
	before, err := os.ReadFile(path)
	if err == nil {
		if string(before) != string(data) {
			return errors.New("original Host storage bytes changed")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return filex.WriteFileAtomicDurable(path, data)
}
