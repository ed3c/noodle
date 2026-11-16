package worktree

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/worktree/archive"
)

// incarnationPublication keeps fault models local to enrollment, without changing
// the App, Execution or Reader contracts.
type incarnationPublication struct {
	write   func(*os.File, []byte) (int, error)
	sync    func(*os.File) error
	link    func(string, string) error
	syncDir func(string) error
}

func nativeIncarnationPublication() incarnationPublication {
	return incarnationPublication{
		write:   (*os.File).Write,
		sync:    (*os.File).Sync,
		link:    os.Link,
		syncDir: filex.SyncDir,
	}
}

func (a *App) enrollIncarnation(path, branch string) error {
	return a.enrollIncarnationWith(path, branch, nativeIncarnationPublication())
}

// Called only after Git successfully creates a linked registration. Errors never
// undo Git effects or remove a published marker, even when durability is unknown.
func (a *App) enrollIncarnationWith(path, branch string, publication incarnationPublication) (err error) {
	var id archive.CheckoutIdentity
	defer func() {
		if err != nil {
			marker := "<unresolved>"
			if id.Registration != "" {
				marker = filepath.Join(id.Registration, "noodle-incarnation.json")
			}
			err = fmt.Errorf("worktree created at %s; incarnation enrollment incomplete (registration %q, marker %q); preserve Git effects for original Owner readback, do not replay Create: %w",
				path, id.Registration, marker, err)
		}
	}()
	id, err = a.custodyIdentity(path, false)
	if err != nil {
		return fmt.Errorf("validate new linked identity: %w", err)
	}
	if id.Ref != "refs/heads/"+branch {
		return fmt.Errorf("new linked branch differs: got %s, created %s", id.Ref, branch)
	}
	registration, err := os.Stat(id.Registration)
	if err != nil {
		return fmt.Errorf("stat new registration: %w", err)
	}
	gitFile, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil {
		return fmt.Errorf("stat linked Git metadata: %w", err)
	}
	var staged os.FileInfo
	check := func(required bool, token string) error {
		current, err := a.custodyIdentity(path, required)
		if err != nil {
			return fmt.Errorf("recheck new linked identity: %w", err)
		}
		currentRegistration, regErr := os.Stat(id.Registration)
		currentGitFile, gitErr := os.Lstat(filepath.Join(path, ".git"))
		if regErr != nil || gitErr != nil {
			return fmt.Errorf("new linked metadata unavailable: registration %v, checkout %v", regErr, gitErr)
		}
		if !os.SameFile(registration, currentRegistration) || !os.SameFile(gitFile, currentGitFile) {
			return fmt.Errorf("new linked registration or checkout replaced")
		}
		expected := id
		expected.Incarnation = token
		if current != expected {
			return fmt.Errorf("new linked identity or incarnation changed during enrollment")
		}
		if required {
			marker, err := os.Lstat(filepath.Join(id.Registration, "noodle-incarnation.json"))
			if err != nil || !marker.Mode().IsRegular() || !os.SameFile(staged, marker) {
				return fmt.Errorf("published incarnation file replaced or unavailable: %v", err)
			}
		}
		return nil
	}
	marker := filepath.Join(id.Registration, "noodle-incarnation.json")
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("incarnation marker already exists: %s", marker)
		}
		return fmt.Errorf("inspect incarnation marker: %w", err)
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("generate incarnation: %w", err)
	}
	token := hex.EncodeToString(random[:])
	data, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("encode incarnation: %w", err)
	}
	f, err := os.CreateTemp(id.Registration, ".noodle-incarnation-*")
	if err != nil {
		return fmt.Errorf("create incarnation staging file: %w", err)
	}
	// Keep staging bytes on failure too. Their name is not a Reader marker.
	temp := f.Name()
	n, writeErr := publication.write(f, data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		closeErr := f.Close()
		return fmt.Errorf("write incarnation staging file %s: %w (close: %v)", temp, writeErr, closeErr)
	}
	staged, err = f.Stat()
	if err != nil {
		closeErr := f.Close()
		return fmt.Errorf("stat incarnation staging file %s: %w (close: %v)", temp, err, closeErr)
	}
	syncErr := publication.sync(f)
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("sync incarnation staging file %s: %w (close: %v)", temp, syncErr, closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close incarnation staging file %s: %w", temp, closeErr)
	}
	if err := check(false, ""); err != nil {
		return err
	}
	// A hard link publishes complete bytes and refuses every existing target,
	// including symlinks. Rename would overwrite another incarnation.
	if err := publication.link(temp, marker); err != nil {
		return fmt.Errorf("publish incarnation exclusively from %s: %w", temp, err)
	}
	for _, dir := range []string{id.Registration, filepath.Dir(id.Registration), id.Repo} {
		if err := publication.syncDir(dir); err != nil {
			return fmt.Errorf("persist incarnation directory %s: %w", dir, err)
		}
	}
	if err := check(true, token); err != nil {
		return err
	}
	if err := os.Remove(temp); err != nil {
		return fmt.Errorf("remove incarnation staging link %s: %w", temp, err)
	}
	if err := publication.syncDir(id.Registration); err != nil {
		return fmt.Errorf("persist incarnation staging removal: %w", err)
	}
	return check(true, token)
}
