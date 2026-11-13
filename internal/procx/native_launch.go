package procx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"path/filepath"
)

// NativeLaunch binds a kernel image to its user-stack argument snapshot. Argv
// can change in process memory; it is observation, not control authority.
// Arguments stay in memory and omit the environment suffix.
type NativeLaunch struct {
	Executable string   `json:"executable"`
	Arguments  []string `json:"-"`
}

func parseNativeLaunch(data []byte, executable string, wordSize int) (NativeLaunch, error) {
	if len(data) < 5 || !filepath.IsAbs(executable) || (wordSize != 4 && wordSize != 8) {
		return NativeLaunch{}, errors.New("native launch buffer or executable unavailable")
	}
	argc := int(int32(binary.NativeEndian.Uint32(data)))
	if argc <= 0 || argc > len(data)-4 {
		return NativeLaunch{}, errors.New("native launch argument count invalid")
	}
	rest := data[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return NativeLaunch{}, errors.New("native launch executable differs from kernel image")
	}
	stackImage := string(rest[:end])
	if stackImage != executable {
		stackPath, stackErr := filepath.EvalSymlinks(stackImage)
		imagePath, imageErr := filepath.EvalSymlinks(executable)
		if !filepath.IsAbs(stackImage) || stackErr != nil || imageErr != nil || stackPath != imagePath {
			return NativeLaunch{}, errors.New("native launch executable differs from kernel image")
		}
	}
	// XNU aligns argv to the target pointer width after executable_path=.
	// That removed prefix is 16 bytes, so it does not alter 4/8-byte alignment.
	start := (end + 1 + wordSize - 1) & ^(wordSize - 1)
	if start >= len(rest) {
		return NativeLaunch{}, errors.New("native launch argument start unavailable")
	}
	for _, value := range rest[end+1 : start] {
		if value != 0 {
			return NativeLaunch{}, errors.New("native launch alignment invalid")
		}
	}
	rest = rest[start:]
	arguments := make([]string, 0, argc)
	for range argc {
		end = bytes.IndexByte(rest, 0)
		if end < 0 {
			return NativeLaunch{}, errors.New("native launch arguments incomplete")
		}
		arg := string(rest[:end])
		if len(arguments) == 0 && arg == "" {
			return NativeLaunch{}, errors.New("empty native launch argument zero is not qualified")
		}
		arguments = append(arguments, arg)
		rest = rest[end+1:]
	}
	// Empty/rewritten argv[0] can make Darwin's initial NUL padding ambiguous.
	// Qualify only an explicit executable or its basename, before exposing argv.
	if arguments[0] != executable && arguments[0] != filepath.Base(executable) {
		resolvedArg, argErr := filepath.EvalSymlinks(arguments[0])
		resolvedImage, imageErr := filepath.EvalSymlinks(executable)
		if !filepath.IsAbs(arguments[0]) || argErr != nil || imageErr != nil || resolvedArg != resolvedImage {
			return NativeLaunch{}, errors.New("native launch argument zero is not qualified")
		}
	}
	return NativeLaunch{Executable: executable, Arguments: arguments}, nil
}
