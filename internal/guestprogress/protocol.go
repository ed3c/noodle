// Package guestprogress owns one Linux harness process tree through archival.
// It does not own the VM, publish candidates, or grant task acceptance.
package guestprogress

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/filex"
)

type Identity struct {
	SessionID     string `json:"session_id"`
	CommandSHA256 string `json:"command_sha256"`
	TokenSHA256   string `json:"token_sha256"`
}

type Config struct {
	Directory, Repository, SessionDirectory string
	Identity                                Identity
	Token                                   string
	Argv                                    []string
	Grace                                   time.Duration
}

type Request struct {
	Schema    int      `json:"schema"`
	Identity  Identity `json:"identity"`
	RequestID string   `json:"request_id"`
	Token     string   `json:"token"`
}

type Archive struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type Receipt struct {
	Schema         int       `json:"schema"`
	Identity       Identity  `json:"identity"`
	RequestID      string    `json:"request_id"`
	Status         string    `json:"status"`
	ExitCode       int       `json:"exit_code"`
	ChildrenReaped bool      `json:"children_reaped"`
	Archives       []Archive `json:"archives"`
	Error          string    `json:"error"`
}

type processBinding struct {
	Identity        Identity `json:"identity"`
	SupervisorPID   int      `json:"supervisor_pid"`
	SupervisorStart string   `json:"supervisor_start"`
	HarnessPID      int      `json:"harness_pid"`
	HarnessStart    string   `json:"harness_start"`
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func (i Identity) valid() bool {
	_, err := hex.DecodeString(i.CommandSHA256)
	_, tokenErr := hex.DecodeString(i.TokenSHA256)
	return i.SessionID != "" && filepath.Base(i.SessionID) == i.SessionID && i.SessionID != "." && i.SessionID != ".." && len(i.CommandSHA256) == 64 && err == nil && len(i.TokenSHA256) == 64 && tokenErr == nil
}

func (r Request) validate(i Identity) error {
	if r.Schema != 1 || !i.valid() || r.Identity != i || len(r.Token) != 64 || subtle.ConstantTimeCompare([]byte(digest([]byte(r.Token))), []byte(i.TokenSHA256)) != 1 {
		return fmt.Errorf("progress control identity differs")
	}
	if len(r.RequestID) != 36 || r.RequestID[8] != '-' || r.RequestID[13] != '-' || r.RequestID[18] != '-' || r.RequestID[23] != '-' {
		return fmt.Errorf("progress stop request ID invalid")
	}
	if b, err := hex.DecodeString(strings.ReplaceAll(r.RequestID, "-", "")); err != nil || len(b) != 16 || strings.Trim(strings.ReplaceAll(r.RequestID, "-", ""), "0") == "" {
		return fmt.Errorf("progress stop request ID invalid")
	}
	return nil
}

func writeJSON(directory, name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(directory, name), b)
}

func readJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return fmt.Errorf("progress evidence is not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err = d.Decode(value); err != nil {
		return err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("progress evidence has trailing content")
	}
	return nil
}

func privateDirectory(directory string) error {
	if !filepath.IsAbs(directory) {
		return fmt.Errorf("progress directory is not absolute")
	}
	actual, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	if actual != filepath.Clean(directory) {
		return fmt.Errorf("progress directory has a symlink alias")
	}
	i, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !i.IsDir() || i.Mode().Perm() != 0700 {
		return fmt.Errorf("progress directory is not private")
	}
	return nil
}

func validateReceipt(result Receipt, identity Identity, requestID string) error {
	if result.Schema != 1 || result.Identity != identity || (result.RequestID != "" && result.RequestID != requestID) || !result.ChildrenReaped || result.Status != "archived" || result.Error != "" || len(result.Archives) != 2 {
		return fmt.Errorf("progress result does not match this completed stop")
	}
	names := map[string]bool{"noodle-result.tar": false, "noodle-session.tar": false}
	for _, archive := range result.Archives {
		seen, ok := names[archive.Name]
		_, err := hex.DecodeString(archive.SHA256)
		if !ok || seen || len(archive.SHA256) != 64 || err != nil || archive.Bytes <= 0 {
			return fmt.Errorf("progress archive receipt invalid")
		}
		names[archive.Name] = true
	}
	return nil
}

// Stop authenticates an exact one-shot request. A completed same-ID request is
// read back, never re-signalled. A different ID cannot take over the operation.
func Stop(ctx context.Context, directory string, request Request) (Receipt, error) {
	var result Receipt
	if err := privateDirectory(directory); err != nil {
		return result, err
	}
	var identity Identity
	if err := readJSON(filepath.Join(directory, "identity.json"), &identity); err != nil {
		return result, err
	}
	if err := request.validate(identity); err != nil {
		return result, err
	}
	if err := readJSON(filepath.Join(directory, "result.json"), &result); err == nil {
		return result, validateReceipt(result, identity, request.RequestID)
	} else if !os.IsNotExist(err) {
		return result, err
	}
	socket := filepath.Join(directory, "control.sock")
	info, err := os.Lstat(socket)
	if err != nil {
		return result, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return result, fmt.Errorf("progress control socket identity unavailable")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	if err = verifyControlPeer(conn, directory, identity); err != nil {
		return result, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err = conn.SetDeadline(deadline); err != nil {
			return result, err
		}
	}
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		return result, err
	}
	d := json.NewDecoder(io.LimitReader(conn, 64<<10))
	d.DisallowUnknownFields()
	if err = d.Decode(&result); err != nil {
		// The original supervisor may have exited immediately after durable
		// completion. Read its exact result; never repeat a signal operation.
		if readErr := readJSON(filepath.Join(directory, "result.json"), &result); readErr != nil {
			return result, err
		}
	}
	return result, validateReceipt(result, identity, request.RequestID)
}
