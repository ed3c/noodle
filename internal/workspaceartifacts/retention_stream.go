package workspaceartifacts

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Keep one original Git reader for the whole ancestry. Object bodies are hashed
// incrementally; neither a large blob nor a long history grows the body buffer.
func verifyRetainedObjectStream(ctx context.Context, root string, oids []string) error {
	cmd := gitCommand(ctx, root, "--no-lazy-fetch", "--no-replace-objects", "cat-file", "--batch")
	// Protocol errors identify the object; exit errors identify the command. Raw
	// stderr is not accumulated alongside an otherwise bounded object stream.
	cmd.Stderr = io.Discard
	return verifyRetainedObjectCommand(ctx, cmd, oids)
}

func verifyRetainedObjectCommand(ctx context.Context, cmd *exec.Cmd, oids []string) (err error) {
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return errors.Join(err, stdout.Close())
	}
	defer func() {
		if err != nil {
			// A malformed response can leave Git blocked on its next write. Always
			// terminate and reap this reader before returning the refusal.
			killErr := cmd.Process.Kill()
			if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				err = errors.Join(err, killErr)
			}
		}
		closeErr := stdout.Close()
		waitErr := cmd.Wait()
		if waitErr != nil {
			waitErr = fmt.Errorf("retained Git object reader failed: %w", waitErr)
		}
		err = errors.Join(err, closeErr, waitErr, ctx.Err())
	}()
	return readRetainedObjectStream(ctx, stdout, oids)
}

func readRetainedObjectStream(ctx context.Context, source io.Reader, oids []string) error {
	reader := bufio.NewReaderSize(source, 4096)
	buffer := make([]byte, 256<<10)
	for _, oid := range oids {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Slice bounds the untrusted header too; an unterminated/malicious header
		// cannot allocate memory proportional to a claimed object size.
		header, err := reader.ReadSlice('\n')
		fields := strings.Fields(string(header))
		if err != nil || len(fields) != 3 || fields[0] != oid || !validObjectID(oid) ||
			(fields[1] != "commit" && fields[1] != "tree" && fields[1] != "blob" && fields[1] != "tag") {
			return fmt.Errorf("retained reachable Git object is missing or malformed: %s", oid)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("retained reachable Git object size is malformed: %s", oid)
		}
		var digest hash.Hash = sha1.New()
		if len(oid) == sha256.Size*2 {
			digest = sha256.New()
		}
		fmt.Fprintf(digest, "%s %d\x00", fields[1], size)
		for remaining := size; remaining > 0; {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk := buffer[:min(remaining, int64(len(buffer)))]
			if _, err := io.ReadFull(reader, chunk); err != nil {
				return fmt.Errorf("retained reachable Git object body is truncated: %s: %w", oid, err)
			}
			digest.Write(chunk)
			remaining -= int64(len(chunk))
		}
		newline, err := reader.ReadByte()
		if err != nil || newline != '\n' || hex.EncodeToString(digest.Sum(nil)) != oid {
			return fmt.Errorf("retained reachable Git object bytes differ: %s", oid)
		}
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("retained Git object stream has no EOF: %w", err)
		}
		return fmt.Errorf("retained Git object stream has trailing data")
	}
	return ctx.Err()
}
