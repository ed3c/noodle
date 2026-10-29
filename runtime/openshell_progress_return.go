package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/guestprogress"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

func progressToken() (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return hex.EncodeToString(token), nil
}

// The stop-purpose stream is independent of the original supervisor stream.
// Neither a control JSON assertion nor a closed stream proves native exit.
func (s *openShellSession) watchProgressStop(supervisorDone <-chan struct{}) {
	defer func() {
		if s.progressStopError != nil {
			s.retainProgress(s.progressStopError)
		}
		close(s.progressStopDone)
	}()
	select {
	case <-supervisorDone:
		return
	case <-s.stopRequested:
	}
	id, err := openShellUUID()
	if err != nil {
		s.progressStopError = err
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	control, err := s.custody.StartProgressStop(ctx, s.worker.Record().Identity.Sandbox, id)
	if err != nil {
		s.progressStopError = err
		return
	}
	if control == nil {
		s.progressStopError = fmt.Errorf("progress-stop native handle missing")
		return
	}
	inputErr := control.CloseWrite()
	log, logErr := os.OpenFile(filepath.Join(s.dir, "progress-stop-output.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	remaining := 64 << 10
	for output := range control.Output() {
		if len(output.Data) > remaining {
			if logErr == nil {
				logErr = fmt.Errorf("progress-stop output exceeded limit")
			}
			remaining = 0
			continue
		}
		remaining -= len(output.Data)
		if log != nil && logErr == nil {
			_, logErr = log.Write(output.Data)
		}
	}
	result, waitErr := control.Wait()
	if log != nil {
		logErr = errors.Join(logErr, log.Sync(), log.Close())
	}
	s.progressStopRequestID = id
	s.progressStopError = errors.Join(err, inputErr, waitErr, logErr)
	if !result.ExitObserved || result.ExitCode != 0 || result.InputError != "" || result.StreamError != "" {
		s.progressStopError = errors.Join(s.progressStopError, fmt.Errorf("progress-stop native completion unresolved or unsuccessful"))
	}
}

func (s *openShellSession) receiveProgress(command OpenShellCommand) error {
	_, binding, err := unwrapOpenShellProgress(command)
	if err != nil {
		return err
	}
	if binding.Binary != s.owner.config.Profile.ProgressSupervisor || binding.SessionID != s.id {
		return fmt.Errorf("progress supervisor source differs")
	}
	staging, err := os.MkdirTemp(s.dir, "progress-result-")
	if err != nil {
		return err
	}
	if err = s.transfer("download", binding.Directory+"/result.json", staging); err != nil {
		return err
	}
	receiptPath := filepath.Join(staging, "result.json")
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 1 || entries[0].Name() != "result.json" {
		return errors.Join(err, fmt.Errorf("progress result download has unexpected entries"))
	}
	var receipt guestprogress.Receipt
	ref, err := ReadSessionReadbackFile(receiptPath, &receipt)
	if err != nil {
		return err
	}
	tokenHash := sha256.Sum256([]byte(binding.Token))
	identity := guestprogress.Identity{SessionID: s.id, CommandSHA256: binding.CommandSHA256, TokenSHA256: hex.EncodeToString(tokenHash[:])}
	if err = guestprogress.ValidateReceipt(receipt, identity, s.progressStopRequestID); err != nil {
		return err
	}
	for _, archive := range receipt.Archives {
		if err = s.receiveProgressArchive(binding.Directory, archive); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.receipt.Progress = &receipt
	s.receipt.ProgressSource = &ref
	s.mu.Unlock()
	return s.persist()
}

func (s *openShellSession) receiveProgressArchive(directory string, archive guestprogress.Archive) error {
	staging, err := os.MkdirTemp(s.dir, "progress-archive-")
	if err != nil {
		return err
	}
	if err = s.transfer("download", directory+"/"+archive.Name, staging); err != nil {
		return err
	}
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 1 || entries[0].Name() != archive.Name {
		return errors.Join(err, fmt.Errorf("progress archive download has unexpected entries"))
	}
	path := filepath.Join(staging, archive.Name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != archive.Bytes {
		return fmt.Errorf("progress archive type or size differs")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	if err = errors.Join(copyErr, file.Close()); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != archive.SHA256 {
		return fmt.Errorf("progress archive content digest differs")
	}
	// Native session state is retained as the exact archive. Its guest-local
	// executable links are evidence, not paths to materialize on the Host.
	if archive.Name == "noodle-session.tar" {
		return nil
	}
	return workspaceartifacts.Extract(context.Background(), path, filepath.Join(s.dir, "returned"))
}

func readSavedProgress(dir string, receipt openShellDispatchReceipt, command OpenShellCommand, stopRequestID string) (SessionReadbackFileRef, error) {
	if receipt.Progress == nil || receipt.ProgressSource == nil {
		return SessionReadbackFileRef{}, fmt.Errorf("saved progress evidence missing")
	}
	ref := *receipt.ProgressSource
	relative, err := filepath.Rel(dir, ref.Path)
	if err != nil || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Base(ref.Path) != "result.json" || !strings.HasPrefix(filepath.Base(filepath.Dir(ref.Path)), "progress-result-") {
		return SessionReadbackFileRef{}, fmt.Errorf("saved progress source is outside its original session")
	}
	var saved guestprogress.Receipt
	observed, err := ReadSessionReadbackFile(ref.Path, &saved)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if observed != ref || !reflect.DeepEqual(saved, *receipt.Progress) {
		return SessionReadbackFileRef{}, fmt.Errorf("saved progress source changed")
	}
	_, binding, err := unwrapOpenShellProgress(command)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	tokenHash := sha256.Sum256([]byte(binding.Token))
	identity := guestprogress.Identity{SessionID: receipt.SessionID, CommandSHA256: binding.CommandSHA256, TokenSHA256: hex.EncodeToString(tokenHash[:])}
	if err := guestprogress.ValidateReceipt(saved, identity, stopRequestID); err != nil {
		return SessionReadbackFileRef{}, err
	}
	return ref, nil
}

func (s *openShellSession) retainProgress(err error) {
	s.fail(err)
	if persistErr := s.phase("progress-unknown"); persistErr != nil {
		s.fail(persistErr)
	}
}
