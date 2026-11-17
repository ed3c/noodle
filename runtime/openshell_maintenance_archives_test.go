package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/guestprogress"
)

func maintenanceArchiveFixture(t *testing.T) (ProgressMaintenance, guestprogress.Receipt) {
	t.Helper()
	p := ProgressMaintenance{SessionDirectory: t.TempDir(), Archives: map[string]SessionReadbackFileRef{}}
	saved := guestprogress.Receipt{}
	directory := t.TempDir()
	for _, name := range []string{"noodle-result.tar", "noodle-session.tar"} {
		data := progressFixtureArchive(t, name)
		file := filepath.Join(directory, name)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := ReadSessionReadbackFile(file, nil)
		if err != nil {
			t.Fatal(err)
		}
		p.Archives[name] = ref
		saved.Archives = append(saved.Archives, guestprogress.Archive{Name: name, SHA256: ref.SHA256, Bytes: int64(len(data))})
	}
	if err := maintenanceWrite(filepath.Join(directory, "result.json"), saved); err != nil {
		t.Fatal(err)
	}
	ref, err := ReadSessionReadbackFile(filepath.Join(directory, "result.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Archives["result.json"] = ref
	return p, saved
}

func maintenanceRetainedNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	if err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err == nil {
			names = append(names, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestMaintenanceArchivesReuseOriginalUniqueRetainedBytes(t *testing.T) {
	p, saved := maintenanceArchiveFixture(t)
	if err := retainMaintenanceArchives(t.Context(), p, saved); err != nil {
		t.Fatal(err)
	}
	before := maintenanceRetainedNames(t, p.SessionDirectory)
	for range 2 {
		if err := retainMaintenanceArchives(t.Context(), p, saved); err != nil {
			t.Fatal("verified original archive was copied again", err)
		}
		if after := maintenanceRetainedNames(t, p.SessionDirectory); !reflect.DeepEqual(before, after) {
			t.Fatal("warm retention added another directory or archive")
		}
	}
	if err := verifyCleanupProgressArchives(t.Context(), p.SessionDirectory, openShellDispatchReceipt{Progress: &saved}); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceArchivesRejectPartialAmbiguousAndChangedRetentionBeforeCopy(t *testing.T) {
	for _, kind := range []string{"partial", "ambiguous", "changed", "cancelled", "bad-source", "warm-source-change"} {
		t.Run(kind, func(t *testing.T) {
			p, saved := maintenanceArchiveFixture(t)
			ctx := t.Context()
			if kind == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			} else if kind == "bad-source" {
				ref := p.Archives[saved.Archives[0].Name]
				if err := os.WriteFile(ref.Path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := retainMaintenanceArchives(ctx, p, saved); err != nil {
					t.Fatal(err)
				}
				files, err := filepath.Glob(filepath.Join(p.SessionDirectory, "progress-archive-*", saved.Archives[0].Name))
				if err != nil || len(files) != 1 {
					t.Fatal(files, err)
				}
				switch kind {
				case "warm-source-change":
					if err := os.WriteFile(p.Archives[saved.Archives[0].Name].Path, []byte("changed source"), 0600); err != nil {
						t.Fatal(err)
					}
				case "partial":
					if err := os.Remove(files[0]); err != nil {
						t.Fatal(err)
					}
				case "changed":
					if err := os.WriteFile(files[0], []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
				case "ambiguous":
					dir := filepath.Join(p.SessionDirectory, "progress-archive-other")
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(files[0])
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, saved.Archives[0].Name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := maintenanceRetainedNames(t, p.SessionDirectory)
			if err := retainMaintenanceArchives(ctx, p, saved); err == nil {
				t.Fatal("uncertain archives allowed another retention copy")
			}
			if after := maintenanceRetainedNames(t, p.SessionDirectory); !reflect.DeepEqual(before, after) {
				t.Fatal("refusal added archive effects")
			}
		})
	}
}

func TestMaintenanceArchivesStreamLargeFilesWithoutRelaxingJSONBound(t *testing.T) {
	for _, size := range []int64{32 << 20, 65 << 20} {
		t.Run(fmt.Sprint(size>>20), func(t *testing.T) {
			p, saved := maintenanceArchiveFixture(t)
			archive := &saved.Archives[0]
			path := p.Archives[archive.Name].Path
			if err := os.Truncate(path, size); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			if err := errors.Join(copyErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			archive.Bytes, archive.SHA256 = size, hex.EncodeToString(hash.Sum(nil))
			p.Archives[archive.Name] = SessionReadbackFileRef{Path: path, SHA256: archive.SHA256}
			result := p.Archives["result.json"].Path
			data, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(result, data, 0600); err != nil {
				t.Fatal(err)
			}
			ref, err := ReadSessionReadbackFile(result, nil)
			if err != nil {
				t.Fatal(err)
			}
			p.Archives["result.json"] = ref
			refs, err := ReadProgressMaintenanceArchives(t.Context(), filepath.Dir(result))
			if err != nil || !reflect.DeepEqual(refs, p.Archives) {
				t.Fatal("maintenance producer did not bind the original receipt and archive bytes", err)
			}
			goruntime.GC()
			var before, after goruntime.MemStats
			goruntime.ReadMemStats(&before)
			if err := retainMaintenanceArchives(t.Context(), p, saved); err != nil {
				t.Fatal(err)
			}
			goruntime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("%d MiB archive retained with %d allocated bytes", size>>20, allocated)
			if allocated > 16<<20 {
				t.Fatal("archive retention allocated whole-file storage", allocated)
			}
			if size > 64<<20 {
				if _, err := ReadSessionReadbackFile(path, nil); err == nil {
					t.Fatal("generic small-file evidence bound was relaxed")
				}
			}
		})
	}
}

type maintenanceArchiveWriteFunc func([]byte) (int, error)

func (write maintenanceArchiveWriteFunc) Write(data []byte) (int, error) { return write(data) }

func TestMaintenanceArchivesStreamRefusesDriftCancellationAndWriteFailure(t *testing.T) {
	for _, kind := range []string{"growth", "replacement", "cancel", "write", "short-write"} {
		t.Run(kind, func(t *testing.T) {
			p, saved := maintenanceArchiveFixture(t)
			archive := saved.Archives[0]
			ref := p.Archives[archive.Name]
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			writeErr := errors.New("isolated destination write failure")
			writer := maintenanceArchiveWriteFunc(func(data []byte) (int, error) {
				switch kind {
				case "growth":
					if err := os.Truncate(ref.Path, archive.Bytes+1); err != nil {
						t.Fatal(err)
					}
				case "replacement":
					if err := os.Rename(ref.Path, ref.Path+".before"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(ref.Path, data, 0600); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					cancel()
				case "write":
					return 0, writeErr
				case "short-write":
					return len(data) - 1, nil
				}
				return len(data), nil
			})
			err := streamMaintenanceArchive(ctx, ref, archive, writer)
			if err == nil {
				t.Fatal("archive stream accepted an uncertain source or destination")
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if kind == "write" && !errors.Is(err, writeErr) {
				t.Fatal(err)
			}
			if kind == "short-write" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}
		})
	}
}

func TestMaintenanceArchivesProducerRefusesInvalidReceiptAndSources(t *testing.T) {
	for _, kind := range []string{"duplicate", "traversal", "missing", "digest", "zero-size", "source", "symlink", "cancelled", "oversized-json"} {
		t.Run(kind, func(t *testing.T) {
			p, saved := maintenanceArchiveFixture(t)
			path := p.Archives["result.json"].Path
			archivePath := p.Archives[saved.Archives[0].Name].Path
			ctx := t.Context()
			switch kind {
			case "duplicate":
				saved.Archives[1] = saved.Archives[0]
			case "traversal":
				saved.Archives[0].Name = "../foreign.tar"
			case "missing":
				saved.Archives = saved.Archives[:1]
			case "digest":
				saved.Archives[0].SHA256 = strings.Repeat("z", 64)
			case "zero-size":
				saved.Archives[0].Bytes = 0
			case "source":
				if err := os.WriteFile(archivePath, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(archivePath, archivePath+".before"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(archivePath+".before", archivePath); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			data, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "oversized-json" {
				if err := os.Truncate(path, 65<<20); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadProgressMaintenanceArchives(ctx, filepath.Dir(path)); err == nil {
				t.Fatal("maintenance producer accepted an invalid archive packet")
			}
		})
	}
}
