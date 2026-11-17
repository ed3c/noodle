package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/poteto/noodle/internal/admission"
)

// VerifySmallQualificationRepository measures the selected immutable tree and
// actual original repository/owner metadata. It performs no checkout, hook,
// dependency installation or file hashing. Existing unknown data is retained.
func VerifySmallQualificationRepository(ctx context.Context, control TaskControl) error {
	const limit = uint64(64 << 20)
	fail := func(reason string) error {
		return fmt.Errorf("%w: small qualification repository: %s", admission.ErrDeferred, reason)
	}
	common, err := taskControlGit(ctx, control.Repository, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(control.Repository, common)
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	var footprint uint64
	count := func(root string, skipGit bool) error {
		return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if skipGit && path == filepath.Join(root, ".git") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// This class never adopts installed or active dependency/build trees. Do
			// not descend or hash node_modules; preserve it for its original owner.
			if skipGit && entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == ".venv" || entry.Name() == "target") {
				return fail("existing dependency/build tree outside zero-install class")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size := uint64(4096)
			if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				if info.Size() < 0 {
					return errors.New("negative repository source size")
				}
				size += uint64(info.Size())
			}
			if size > limit-footprint {
				return fail("initial files and Git owner metadata exceed 64MiB")
			}
			footprint += size
			return nil
		})
	}
	if err = count(control.Repository, true); err != nil {
		return err
	}
	if err = count(common, false); err != nil {
		return err
	}
	// A small working HEAD cannot hide a large selected base_ref checkout.
	raw, err := exec.CommandContext(ctx, "git", "-C", control.Repository, "ls-tree", "-r", "-l", "-z", control.Head).Output()
	if err != nil {
		return err
	}
	var tree uint64
	var treePaths bytes.Buffer
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		parts := bytes.SplitN(entry, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return fail("selected tree path unavailable")
		}
		treePaths.Write(parts[1])
		treePaths.WriteByte(0)
		fields := strings.Fields(strings.SplitN(string(entry), "\t", 2)[0])
		if len(fields) != 4 || fields[1] != "blob" {
			return fail("selected tree has unsupported submodule or unknown entry")
		}
		size, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return fail("selected blob size unavailable")
		}
		if size > limit-tree || 4096 > limit-tree-size {
			return fail("selected base tree exceeds 64MiB")
		}
		tree += size + 4096
	}
	hookCommand := exec.CommandContext(ctx, "git", "-C", control.Repository, "config", "--path", "--get", "core.hooksPath")
	hookBytes, err := hookCommand.Output()
	hooks := strings.TrimSpace(string(hookBytes))
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return err
		}
		hooks = filepath.Join(common, "hooks")
	} else if !filepath.IsAbs(hooks) {
		// Git resolves this in the new checkout, not the current repository.
		// Absence here cannot qualify the selected base before worktree add.
		return fail("relative core.hooksPath cannot be qualified before selected checkout")
	}
	entries, err := os.ReadDir(hooks)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sample") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0) {
			return fail("active Git hook has no zero-install qualification")
		}
	}
	// A globally configured filter (for example LFS) is harmless when no
	// selected path uses it. Read actual attributes from the bound tree.
	command := exec.CommandContext(ctx, "git", "-C", control.Repository, "check-attr", "--source="+control.Head, "-z", "--stdin", "filter")
	command.Stdin = &treePaths
	raw, err = command.Output()
	if err != nil {
		return fail("selected-tree checkout attributes unavailable")
	}
	attributes := bytes.Split(raw, []byte{0})
	for index := 2; index < len(attributes); index += 3 {
		value := string(attributes[index])
		if value != "unspecified" && value != "unset" {
			return fail("selected checkout filter has no bounded qualification")
		}
	}
	return nil
}
