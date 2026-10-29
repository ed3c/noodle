package workspaceartifacts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

func compactionRefPrefix(b *Baseline, r *Result) string {
	return "refs/noodle/retained/" + digest([]byte(b.Directory+"\x00"+r.PatchPath))
}

func compactionGit(ctx context.Context, root string, input []byte, args ...string) ([]byte, error) {
	return git(ctx, root, input, append([]string{"--no-lazy-fetch", "--no-replace-objects", "-c", "core.fsync=loose-object,reference", "-c", "core.fsyncMethod=fsync"}, args...)...)
}

func compactionRefs(ctx context.Context, root string, m *compactionManifest) (map[string]string, error) {
	prefix := compactionRefPrefix(&m.Baseline, &m.Result)
	raw, err := compactionGit(ctx, root, nil, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", prefix+"/")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("compaction ref is symbolic or malformed")
		}
		refs[fields[0]] = fields[1]
	}
	return refs, nil
}

func verifyCompactionRefs(ctx context.Context, root string, m *compactionManifest, raw []byte) error {
	refs, err := compactionRefs(ctx, root, m)
	if err != nil {
		return err
	}
	prefix := compactionRefPrefix(&m.Baseline, &m.Result)
	if len(refs) != 3 || refs[prefix+"/base"] != m.Parent || refs[prefix+"/head"] != m.Head || !matchesRawBlob(raw, refs[prefix+"/manifest"]) {
		return fmt.Errorf("compaction refs or manifest binding differ")
	}
	saved, err := compactionGit(ctx, root, nil, "cat-file", "blob", refs[prefix+"/manifest"])
	if err != nil {
		return err
	}
	if !bytes.Equal(saved, raw) {
		return fmt.Errorf("compaction manifest object differs")
	}
	return nil
}

func pinCompaction(ctx context.Context, root string, m *compactionManifest, raw []byte) error {
	refs, err := compactionRefs(ctx, root, m)
	if err != nil {
		return err
	}
	if len(refs) != 0 {
		if err := verifyCompactionRefs(ctx, root, m, raw); err != nil {
			return err
		}
		oid := refs[compactionRefPrefix(&m.Baseline, &m.Result)+"/manifest"]
		if err := syncCompactionSources(ctx, root, m, oid); err != nil {
			return err
		}
		return syncPinnedCompaction(ctx, root, m)
	}
	// No refs means deletion was never authorized. Require all original inputs
	// before finishing an intent interrupted between its fsync and ref creation.
	evidence := filepath.Dir(m.Result.PatchPath)
	copies := []string{m.Baseline.Directory, m.Result.ReturnedDirectory, filepath.Join(evidence, "baseline"), filepath.Join(evidence, "returned"), filepath.Join(evidence, "patch-repository")}
	fresh, err := prepareCompaction(ctx, root, &m.Baseline, &m.Result, m.Head, evidence, copies)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh, m) {
		return fmt.Errorf("unpinned compaction source or intent changed")
	}
	if err := syncCompactionIntent(filepath.Join(evidence, compactionManifestName)); err != nil {
		return err
	}
	oid, err := compactionGit(ctx, root, raw, "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		return err
	}
	if err := syncCompactionSources(ctx, root, m, strings.TrimSpace(string(oid))); err != nil {
		return err
	}
	prefix := compactionRefPrefix(&m.Baseline, &m.Result)
	transaction := fmt.Sprintf("start\noption no-deref\ncreate %s/base %s\ncreate %s/head %s\ncreate %s/manifest %s\nprepare\ncommit\n", prefix, m.Parent, prefix, m.Head, prefix, strings.TrimSpace(string(oid)))
	if _, err := compactionGit(ctx, root, []byte(transaction), "update-ref", "--stdin"); err != nil {
		return err
	}
	return syncPinnedCompaction(ctx, root, m)
}

// Enumerate commits and all their ancestors, trees and blobs, including roots
// that are themselves manifest blobs. Read and hash the original bytes, rather
// than treating an object ID from a tree or index as proof the object exists.
func retainedReachableObjects(ctx context.Context, root string, roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("retained Git object roots missing")
	}
	for _, oid := range roots {
		if !validObjectID(oid) {
			return nil, fmt.Errorf("retained Git object root is malformed")
		}
	}
	args := append([]string{"-c", "core.commitGraph=false", "rev-list", "--objects", "--no-object-names", "--missing=error"}, roots...)
	raw, err := compactionGit(ctx, root, nil, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	oids := strings.Fields(string(raw))
	seen := map[string]bool{}
	for _, oid := range oids {
		if !validObjectID(oid) || seen[oid] {
			return nil, fmt.Errorf("retained Git object traversal is malformed")
		}
		seen[oid] = true
	}
	for _, oid := range roots {
		if !seen[oid] {
			return nil, fmt.Errorf("retained Git object traversal omitted a root")
		}
	}
	if err := verifyRetainedObjectStream(ctx, root, oids); err != nil {
		return nil, err
	}
	return oids, nil
}

// Batch raw object reads so large repositories do not spawn Git once per file.
// Hash the returned bytes locally: cat-file alone can read a loose object whose
// contents were replaced under an existing object ID.
func retainedObjects(ctx context.Context, root, head string) (map[string]compactedObject, error) {
	commit, err := compactionGit(ctx, root, nil, "cat-file", "commit", head)
	if err != nil {
		return nil, err
	}
	if !matchesRetainedObject(commit, "commit", head) {
		return nil, fmt.Errorf("retained Git commit bytes differ")
	}
	first, _, _ := strings.Cut(string(commit), "\n")
	rootTree := strings.TrimPrefix(first, "tree ")
	if !strings.HasPrefix(first, "tree ") || !validObjectID(rootTree) {
		return nil, fmt.Errorf("retained commit tree is malformed")
	}
	listing, err := compactionGit(ctx, root, nil, "ls-tree", "-r", "-t", "-z", "--full-tree", head)
	if err != nil {
		return nil, err
	}
	tree := map[string]publicationTreeEntry{".": {Mode: "040000", Object: rootTree}}
	types := map[string]string{rootTree: "tree"}
	for _, record := range strings.Split(string(listing), "\x00") {
		if record == "" {
			continue
		}
		metadata, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || (fields[1] != "blob" && fields[1] != "tree") {
			return nil, fmt.Errorf("retained tree record is unsupported")
		}
		tree[name] = publicationTreeEntry{Mode: fields[0], Object: fields[2]}
		types[fields[2]] = fields[1]
	}
	var input strings.Builder
	objects := map[string]compactedObject{}
	for _, entry := range tree {
		if _, ok := objects[entry.Object]; ok {
			continue
		}
		if !validObjectID(entry.Object) {
			return nil, fmt.Errorf("retained tree object ID is malformed")
		}
		objects[entry.Object] = compactedObject{}
		input.WriteString(entry.Object + "\n")
	}
	raw, err := compactionGit(ctx, root, []byte(input.String()), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(raw))
	for _, oid := range strings.Split(strings.TrimSuffix(input.String(), "\n"), "\n") {
		if oid == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != oid || fields[1] != types[oid] {
			return nil, fmt.Errorf("retained Git object is missing or malformed: %s", oid)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size > int64(len(raw)) {
			return nil, fmt.Errorf("retained Git object size is malformed")
		}
		body := make([]byte, int(size))
		if _, err := io.ReadFull(reader, body); err != nil {
			return nil, err
		}
		newline, err := reader.ReadByte()
		if err != nil || newline != '\n' {
			return nil, fmt.Errorf("retained Git object terminator differs")
		}
		if !matchesRetainedObject(body, types[oid], oid) {
			return nil, fmt.Errorf("retained Git object bytes differ: %s", oid)
		}
		objects[oid] = compactedObject{Entry: Entry{SHA256: digest(body)}, Bytes: size, Object: oid}
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		return nil, fmt.Errorf("retained Git batch has trailing data: %v", err)
	}
	result := map[string]compactedObject{}
	for name, entry := range tree {
		object := objects[entry.Object]
		object.Mode = entry.Mode
		result[name] = object
	}
	return result, nil
}

func matchesRetainedObject(data []byte, kind, expected string) bool {
	var h hash.Hash
	switch len(expected) {
	case sha1.Size * 2:
		h = sha1.New()
	case sha256.Size * 2:
		h = sha256.New()
	default:
		return false
	}
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)) == expected
}
