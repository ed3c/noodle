package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/guestprogress"
)

func TestProgressArchiveReadbackStreamsLargeOriginalArchives(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "progress-archive-original")
	if err := os.Mkdir(archiveDir, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := openShellDispatchReceipt{Version: 1, SessionID: filepath.Base(dir), Progress: &guestprogress.Receipt{}}
	for _, name := range []string{"noodle-result.tar", "noodle-session.tar"} {
		path := filepath.Join(archiveDir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		size := int64(17)
		if name == "noodle-session.tar" {
			size = 65 << 20
		}
		if err = f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		receipt.Progress.Archives = append(receipt.Progress.Archives, guestprogress.Archive{Name: name, Bytes: size, SHA256: hex.EncodeToString(hash[:])})
	}
	progressDir := filepath.Join(dir, "progress-result-original")
	if err := os.Mkdir(progressDir, 0700); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(progressDir, "result.json")
	writeReadbackJSON(t, progressPath, receipt.Progress)
	progressRef, err := ReadSessionReadbackFile(progressPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ProgressSource = &progressRef
	dispatch := filepath.Join(dir, "openshell-dispatch.json")
	writeReadbackJSON(t, dispatch, receipt)
	ref, err := ReadSessionReadbackFile(dispatch, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := progressArchiveSources(context.Background(), dir, receipt)
	if err != nil {
		t.Fatal(err)
	}
	refs["openshell-dispatch.json"] = ref
	refs["progress_receipt"] = progressRef
	if err = VerifySessionReadbackFiles(refs); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadSessionReadbackFile(refs["progress_archive_noodle-session.tar"].Path, nil); err == nil {
		t.Fatal("generic JSON/file bound was relaxed")
	}
	// Cleanup aggregates prefix each session's sources. Preserve the same binding.
	prefixed := map[string]SessionReadbackFileRef{}
	for key, value := range refs {
		prefixed["original-session/"+key] = value
	}
	if err = VerifySessionReadbackFiles(prefixed); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"unbound", "foreign-path", "missing-peer", "missing-progress", "ambiguous", "parent-symlink", "bad-receipt", "foreign-session", "duplicate-name", "wrong-size", "wrong-sha", "changed-bytes", "symlink"} {
		t.Run(variant, func(t *testing.T) {
			changed := map[string]SessionReadbackFileRef{}
			for k, v := range refs {
				changed[k] = v
			}
			key := "progress_archive_noodle-result.tar"
			path := changed[key].Path
			switch variant {
			case "unbound":
				delete(changed, "openshell-dispatch.json")
			case "foreign-path":
				value := changed[key]
				value.Path = filepath.Join(t.TempDir(), "noodle-result.tar")
				changed[key] = value
			case "missing-peer":
				delete(changed, "progress_archive_noodle-session.tar")
			case "missing-progress":
				delete(changed, "progress_receipt")
			case "ambiguous":
				duplicate := filepath.Join(dir, "progress-archive-duplicate")
				if err := os.Mkdir(duplicate, 0700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(duplicate) })
				if err := os.WriteFile(filepath.Join(duplicate, "noodle-result.tar"), make([]byte, 17), 0600); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				original := filepath.Join(dir, "original-archive-storage")
				if err := os.Rename(archiveDir, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original, archiveDir); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(archiveDir); _ = os.Rename(original, archiveDir) })
			case "bad-receipt", "foreign-session", "duplicate-name", "wrong-size", "wrong-sha":
				mutated := receipt
				progress := *receipt.Progress
				progress.Archives = append([]guestprogress.Archive{}, receipt.Progress.Archives...)
				mutated.Progress = &progress
				switch variant {
				case "bad-receipt":
					mutated.Progress = nil
				case "foreign-session":
					mutated.SessionID = "foreign"
				case "duplicate-name":
					progress.Archives[1].Name = progress.Archives[0].Name
				case "wrong-size":
					progress.Archives[0].Bytes++
				case "wrong-sha":
					progress.Archives[0].SHA256 = refs[key].SHA256[:63] + "x"
				}
				if mutated.Progress != nil {
					writeReadbackJSON(t, progressPath, mutated.Progress)
					changedProgress, err := ReadSessionReadbackFile(progressPath, nil)
					if err != nil {
						t.Fatal(err)
					}
					mutated.ProgressSource = &changedProgress
					changed["progress_receipt"] = changedProgress
					defer writeReadbackJSON(t, progressPath, receipt.Progress)
				}
				writeReadbackJSON(t, dispatch, mutated)
				defer writeReadbackJSON(t, dispatch, receipt)
				changed["openshell-dispatch.json"], err = ReadSessionReadbackFile(dispatch, nil)
				if err != nil {
					t.Fatal(err)
				}
			case "changed-bytes":
				if err := os.WriteFile(path, []byte("different"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dispatch, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := VerifySessionReadbackFiles(changed); err == nil {
				t.Fatal("accepted invalid archive evidence")
			}
		})
	}
}
