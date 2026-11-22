package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree/archive"
)

func TestExitedArchiveRecoveryUsesExactOriginalConfiguredPolicy(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := archive.Config{Roots: []string{filepath.Join(root, "checkouts")}, ArchiveRoot: filepath.Join(root, "archives"),
		BlobRoot: filepath.Join(root, "blobs"), StateDir: filepath.Join(root, "owner"), Codec: filepath.Join(root, "zstd"),
		MaxItems: 1, ItemTimeoutSeconds: 120}
	for _, path := range []string{cfg.Roots[0], cfg.ArchiveRoot, cfg.BlobRoot, cfg.StateDir} {
		if err = os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "original-policy.json")
	save := func(path string, raw []byte) loopruntime.SessionReadbackFileRef {
		t.Helper()
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	policy := save(configPath, raw)
	codec := save(cfg.Codec, []byte("original codec fixture identity"))
	binding := map[string]string{"archive": policy.Path + "/" + policy.SHA256, "codec": codec.Path + "/" + codec.SHA256}
	if selected, err := exitedArchivePolicy(binding, nil); err != nil || selected != archive.FileRef(policy) {
		t.Fatalf("exact original configured policy unavailable: %+v %v", selected, err)
	}
	for _, alternative := range []string{"native-default/" + policy.SHA256, configPath + "/" + codec.SHA256} {
		binding["archive"] = alternative
		if _, err := exitedArchivePolicy(binding, nil); err == nil {
			t.Fatal("unbound archive policy accepted")
		}
	}
	binding["archive"] = policy.Path + "/" + policy.SHA256
	save(cfg.Codec, []byte("replacement at original codec path"))
	if _, err = exitedArchivePolicy(binding, nil); err == nil {
		t.Fatal("changed original archive codec accepted")
	}
}

func TestExitedArchiveRecoveryReplaysOnlyFrozenNativeDefaultPolicy(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storage, policyDir := filepath.Join(root, "storage"), filepath.Join(root, "policies")
	base := filepath.Join(storage, ".worktrees")
	cfg := archive.Config{Roots: []string{base}, ArchiveRoot: filepath.Join(root, "archive"), BlobRoot: filepath.Join(root, "blobs"),
		StateDir: filepath.Join(root, "owner"), Codec: filepath.Join(root, "zstd"), MaxItems: 1, ItemTimeoutSeconds: 120}
	for _, dir := range []string{base, cfg.ArchiveRoot, cfg.BlobRoot, cfg.StateDir, policyDir} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(cfg.Codec, []byte("original codec fixture identity"), 0600); err != nil {
		t.Fatal(err)
	}
	codec, err := loopruntime.ReadSessionReadbackFile(cfg.Codec, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(policyDir, hostDigest([]byte(base))+".json")
	original, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(hostArchivePlan{Config: cfg, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	binding := map[string]string{"archive": "native-default/" + hostDigest(plan), "codec": codec.Path + "/" + codec.SHA256}
	roots := []string{root, root, root, storage, root, cfg.ArchiveRoot, cfg.BlobRoot, cfg.StateDir, policyDir}
	if checked, err := exitedArchivePolicy(binding, roots); err != nil || checked.Path != path {
		t.Fatalf("native-default policy was not read from its frozen plan: %+v %v", checked, err)
	}
	cfg.MaxItems++
	changed, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = exitedArchivePolicy(binding, roots); err == nil {
		t.Fatal("changed native-default archive policy passed frozen plan check")
	}
}
