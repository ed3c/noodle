package instructionbundle

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Prepared struct {
	Directory      string    `json:"directory"`
	GuestDirectory string    `json:"guest_directory"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	Manifest       *Manifest `json:"manifest"`
}

// Prepare finishes all source validation before creating the output directory.
// The caller owns that directory exclusively; this is not an arbitrary-writer CAS.
func Prepare(ctx context.Context, ref FileRef, orderID, parentDirectory string) (*Prepared, error) {
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	data, err := readRegular(ctx, ref.Path, MaxManifestBytes, nil)
	if err != nil {
		return nil, err
	}
	m, err := decodeManifest(data, ref.SHA256, orderID)
	if err != nil {
		return nil, err
	}
	contents := make([][]byte, len(m.Files))
	total := 0
	for i, f := range m.Files {
		b, err := readRegular(ctx, f.Source.Path, MaxFileBytes, f.Mode)
		if err != nil {
			return nil, fmt.Errorf("read frozen instruction %q: %w", f.Destination, err)
		}
		if digest(b) != f.Source.SHA256 {
			return nil, fmt.Errorf("frozen instruction hash differs: %q", f.Destination)
		}
		total += len(b)
		if total > MaxTotalBytes {
			return nil, fmt.Errorf("instruction total bytes exceed limit")
		}
		contents[i] = b
	}
	if err := noSymlinks(parentDirectory); err != nil {
		return nil, err
	}
	directory := filepath.Join(parentDirectory, "instructions-"+ref.SHA256)
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, fmt.Errorf("create instruction directory: %w", err)
	}
	if err := writeNew(filepath.Join(directory, ManifestName), data, 0600); err != nil {
		return nil, err
	}
	for i, f := range m.Files {
		name := filepath.Join(directory, filepath.FromSlash(f.Destination))
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return nil, err
		}
		if err := noSymlinks(filepath.Dir(name)); err != nil {
			return nil, err
		}
		if err := writeNew(name, contents[i], os.FileMode(*f.Mode)); err != nil {
			return nil, err
		}
	}
	if _, err := Verify(ctx, directory, ref.SHA256, orderID); err != nil {
		return nil, err
	}
	return &Prepared{Directory: directory, GuestDirectory: path.Join("/sandbox", filepath.Base(directory)), ManifestSHA256: ref.SHA256, Manifest: m}, nil
}

// Verify reads only the transported files, never source or original_source paths.
func Verify(ctx context.Context, directory, manifestSHA256, orderID string) (*Manifest, error) {
	if filepath.Base(directory) != "instructions-"+manifestSHA256 {
		return nil, fmt.Errorf("instruction directory identity differs")
	}
	data, err := readRegular(ctx, filepath.Join(directory, ManifestName), MaxManifestBytes, nil)
	if err != nil {
		return nil, err
	}
	m, err := decodeManifest(data, manifestSHA256, orderID)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{ManifestName: false}
	total := 0
	for _, f := range m.Files {
		allowed[f.Destination] = false
		for p := path.Dir(f.Destination); p != "."; p = path.Dir(p) {
			allowed[p] = true
		}
		b, err := readRegular(ctx, filepath.Join(directory, filepath.FromSlash(f.Destination)), MaxFileBytes, f.Mode)
		if err != nil {
			return nil, fmt.Errorf("verify instruction %q: %w", f.Destination, err)
		}
		if digest(b) != f.Source.SHA256 {
			return nil, fmt.Errorf("materialized instruction hash differs: %q", f.Destination)
		}
		total += len(b)
		if total > MaxTotalBytes {
			return nil, fmt.Errorf("instruction total bytes exceed limit")
		}
	}
	err = filepath.WalkDir(directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == directory {
			return nil
		}
		rel, err := filepath.Rel(directory, name)
		if err != nil {
			return err
		}
		isDir, ok := allowed[filepath.ToSlash(rel)]
		if !ok || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() != isDir {
			return fmt.Errorf("unexpected materialized instruction entry")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (p *Prepared) PromptSection() string {
	var b strings.Builder
	b.WriteString("# Materialized task instructions\n\nUse the guest paths below to read the admitted instructions and their relative files. Original source paths identify frozen provenance; they are not guest filesystem paths.\n")
	byPath := map[string]File{}
	for _, f := range p.Manifest.Files {
		byPath[f.Destination] = f
	}
	for _, binding := range p.Manifest.Bindings {
		f := byPath[binding.Path]
		fmt.Fprintf(&b, "\n- kind=%s name=%q guest=%q original_source=%q original_sha256=%s", binding.Kind, binding.Name, path.Join(p.GuestDirectory, binding.Path), f.OriginalSource.Path, f.OriginalSource.SHA256)
	}
	return b.String()
}
