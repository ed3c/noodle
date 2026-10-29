package loopview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

// DetailRef identifies immutable projection data, never an executable input.
// Counts describe unloaded content; empty summary arrays do not imply empty data.
type DetailRef struct {
	SHA256      string   `json:"sha256"`
	Bytes       int64    `json:"bytes"`
	Tracked     int      `json:"tracked"`
	Selected    int      `json:"selected"`
	ArtifactIDs []string `json:"artifact_ids"`
}

type registry struct {
	Version int  `json:"registry_version"`
	View    View `json:"view"`
}

func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func boundedRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return nil, fmt.Errorf("projection document is not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("projection document exceeds %d bytes", MaxBytes)
	}
	return data, nil
}

func decodeRegistry(data []byte) (View, error) {
	var header struct {
		Version int `json:"registry_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return View{}, err
	}
	if header.Version != 1 {
		return View{}, fmt.Errorf("loop registry migration required; run host views migrate before importing or reading")
	}

	var stored registry
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&stored); err != nil {
		return View{}, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return View{}, fmt.Errorf("registry contains trailing data")
	}
	if stored.Version != 1 {
		return View{}, fmt.Errorf("loop registry migration required")
	}
	raw, err := json.Marshal(stored.View)
	if err != nil {
		return View{}, err
	}
	v, err := Decode(raw)
	if err != nil {
		return View{}, err
	}
	check := func(ref *DetailRef) error {
		if ref == nil || !validDigest(ref.SHA256) || ref.Bytes < 1 || ref.Bytes > MaxBytes || ref.Tracked < 0 || ref.Selected < 0 || ref.ArtifactIDs == nil {
			return fmt.Errorf("stored detail reference invalid")
		}
		return nil
	}
	for _, r := range v.Runs {
		if err := check(r.Detail); err != nil {
			return View{}, err
		}
	}
	if v.Graph != nil {
		for _, r := range v.Graph.Repositories {
			if err := check(r.Detail); err != nil {
				return View{}, err
			}
			if len(r.TrackedFiles) != 0 || len(r.SelectedFiles) != 0 {
				return View{}, fmt.Errorf("repository summary contains index bodies")
			}
		}
	}
	return v, nil
}

// LoadSummary reads only the committed manifest, without opening detail files.
func LoadSummary(dir string) (View, error) {
	if err := checkMigrationCompletion(dir); err != nil {
		return View{}, err
	}
	data, err := boundedRead(Path(dir))
	if os.IsNotExist(err) {
		return Empty(), nil
	}
	if err != nil {
		return View{}, err
	}
	return decodeRegistry(data)
}

func detailPath(dir, digest string) string {
	return filepath.Join(dir, "loop-view-details", digest+".json")
}
func readDetail(dir string, ref *DetailRef, value any) error {
	if ref == nil || !validDigest(ref.SHA256) || ref.Bytes < 1 || ref.Bytes > MaxBytes {
		return fmt.Errorf("detail reference invalid")
	}
	raw, err := boundedRead(detailPath(dir, ref.SHA256))
	if err != nil {
		return fmt.Errorf("detail unavailable: %w", err)
	}
	if int64(len(raw)) != ref.Bytes || fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.SHA256 {
		return fmt.Errorf("detail source changed")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("detail contains trailing data")
	}
	return nil
}
func writeImmutable(path string, data []byte) error {
	if len(data) > MaxBytes {
		return fmt.Errorf("projection document exceeds %d bytes", MaxBytes)
	}
	if old, err := boundedRead(path); err == nil {
		if !bytes.Equal(old, data) {
			return fmt.Errorf("immutable projection changed")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := filex.WriteFileAtomicDurable(path, data); err != nil {
		return err
	}
	return filex.SyncDir(filepath.Dir(filepath.Dir(path)))
}
func writeDetail(dir string, value any) (*DetailRef, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	ref := &DetailRef{SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Bytes: int64(len(data)), ArtifactIDs: []string{}}
	if err = writeImmutable(detailPath(dir, ref.SHA256), data); err != nil {
		return nil, err
	}
	return ref, nil
}

// RunDetail reads exactly the run selected from a particular manifest.
func RunDetail(dir string, summary Run) (Run, error) {
	var run Run
	if err := readDetail(dir, summary.Detail, &run); err != nil {
		return Run{}, err
	}
	if summary.Detail.Tracked != 0 || summary.Detail.Selected != 0 || len(summary.Detail.ArtifactIDs) != 0 || run.Detail != nil || !reflect.DeepEqual(summarizeRun(run, summary.Detail), summary) {
		return Run{}, fmt.Errorf("run detail identity differs from manifest")
	}
	return run, nil
}
func summarizeRun(run Run, ref *DetailRef) Run {
	run.Detail = ref
	run.Stages = append([]Stage{}, run.Stages...)
	for i := range run.Stages {
		run.Stages[i].Prompt = ""
	}
	run.Cards = append([]Card{}, run.Cards...)
	for i := range run.Cards {
		run.Cards[i].Body = ""
	}
	run.Notes = append([]Note{}, run.Notes...)
	for i := range run.Notes {
		run.Notes[i].Body = ""
	}
	return run
}
func summarizeRepository(repo Repository, ref *DetailRef) Repository {
	repo.Detail = ref
	repo.TrackedFiles = []IndexFile{}
	repo.SelectedFiles = []IndexFile{}
	return repo
}
func RepositoryDetail(dir string, summary Repository) (Repository, error) {
	var repo Repository
	if err := readDetail(dir, summary.Detail, &repo); err != nil {
		return Repository{}, err
	}
	artifactIDs := []string{}
	for _, file := range repo.SelectedFiles {
		artifactIDs = unionRefs(artifactIDs, file.ArtifactIDs)
	}
	if !reflect.DeepEqual(artifactIDs, summary.Detail.ArtifactIDs) || repo.Detail != nil || summary.Detail.Tracked != len(repo.TrackedFiles) || summary.Detail.Selected != len(repo.SelectedFiles) || !reflect.DeepEqual(summarizeRepository(repo, summary.Detail), summary) {
		return Repository{}, fmt.Errorf("repository detail identity differs from manifest")
	}
	return repo, nil
}

func storeProjection(dir string, view View) (View, []byte, error) {
	summary := view
	summary.Runs = append([]Run{}, view.Runs...)
	for i, run := range view.Runs {
		ref, err := writeDetail(dir, run)
		if err != nil {
			return View{}, nil, err
		}
		summary.Runs[i] = summarizeRun(run, ref)
	}
	if view.Graph != nil {
		g := *view.Graph
		g.Repositories = append([]Repository{}, g.Repositories...)
		summary.Graph = &g
		for i, repo := range view.Graph.Repositories {
			ref, err := writeDetail(dir, repo)
			if err != nil {
				return View{}, nil, err
			}
			ref.Tracked = len(repo.TrackedFiles)
			ref.Selected = len(repo.SelectedFiles)
			for _, file := range repo.SelectedFiles {
				ref.ArtifactIDs = unionRefs(ref.ArtifactIDs, file.ArtifactIDs)
			}
			g.Repositories[i] = summarizeRepository(repo, ref)
		}
	}
	data, err := json.Marshal(registry{Version: 1, View: summary})
	if err != nil {
		return View{}, nil, err
	}
	if len(data) > MaxBytes {
		return View{}, nil, fmt.Errorf("loop metadata registry exceeds %d bytes", MaxBytes)
	}
	if _, err = decodeRegistry(data); err != nil {
		return View{}, nil, err
	}
	return summary, data, nil
}

func versionPath(dir, version string) string {
	return filepath.Join(dir, "loop-view-versions", version+".json")
}
func retainVersion(dir string, summary View, data []byte) error {
	return writeImmutable(versionPath(dir, ReviewVersion(summary)), data)
}
func LoadVersion(dir, version string) (View, error) {
	if !validDigest(version) {
		return View{}, fmt.Errorf("invalid projection version")
	}
	data, err := boundedRead(versionPath(dir, version))
	if err != nil {
		return View{}, err
	}
	view, err := decodeRegistry(data)
	if err != nil {
		return View{}, err
	}
	if ReviewVersion(view) != version {
		return View{}, fmt.Errorf("projection version changed")
	}
	return view, nil
}

// Migrate makes the single-writer cutover explicit. Reads never migrate data.
// Exact prior bytes remain in history; old selection versions retain their source.
func Migrate(dir string) (bool, error) {
	lock, err := lockfile.TryLock(filepath.Join(dir, "loop-views.lock"))
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := checkMigrationCompletion(dir); err != nil {
		return false, err
	}
	prior, err := boundedRead(Path(dir))
	if err != nil {
		return false, err
	}
	if _, err = decodeRegistry(prior); err == nil {
		return false, nil
	}
	full, err := Decode(prior)
	if err != nil {
		return false, err
	}
	if err = archiveView(dir, prior); err != nil {
		return false, err
	}
	oldVersion := ReviewVersion(full)
	summary, data, err := storeProjection(dir, referencedArtifacts(full))
	if err != nil {
		return false, err
	}
	// Preserve the legacy graph version with its exact source, without changing receipts.
	if err = writeImmutable(filepath.Join(dir, "loop-view-legacy-versions", oldVersion+".json"), prior); err != nil {
		return false, err
	}
	if err = retainVersion(dir, summary, data); err != nil {
		return false, err
	}
	receipt, _ := json.Marshal(map[string]any{"version": 1, "before_sha256": fmt.Sprintf("%x", sha256.Sum256(prior)), "after_sha256": fmt.Sprintf("%x", sha256.Sum256(data)), "original_review_version": oldVersion})
	prepared := filepath.Join(dir, "loop-view-migrations", fmt.Sprintf("%x.prepared.json", sha256.Sum256(prior)))
	if err = writeImmutable(prepared, receipt); err != nil {
		return false, err
	}
	if err = filex.WriteFileAtomicDurable(Path(dir), data); err != nil {
		return false, err
	}
	observed, err := boundedRead(Path(dir))
	if err != nil {
		return false, err
	}
	if !bytes.Equal(observed, data) {
		return false, fmt.Errorf("migrated registry readback changed")
	}
	if err = writeImmutable(strings.TrimSuffix(prepared, ".prepared.json")+".completed.json", receipt); err != nil {
		return false, err
	}
	return true, nil
}

func checkMigrationCompletion(dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "loop-view-migrations", "*.prepared.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		prepared, err := boundedRead(path)
		if err != nil {
			return err
		}
		completed, err := boundedRead(strings.TrimSuffix(path, ".prepared.json") + ".completed.json")
		if err != nil || !bytes.Equal(prepared, completed) {
			return fmt.Errorf("loop projection migration outcome unresolved; retained receipt requires inspection")
		}
	}
	return nil
}
