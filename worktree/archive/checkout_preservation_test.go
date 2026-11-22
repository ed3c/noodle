package archive

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fixtureWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func fixtureJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	fixtureWrite(t, path, b)
}
func fixtureGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	if len(args) != 0 && args[0] == "commit" {
		// Fixtures also use normal hooks. The archive helper is only for the
		// read/restore operations which must not execute source-owned hooks.
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("normal fixture commit: %s (%v)", out, err)
		}
		return strings.TrimSpace(string(out))
	}
	out, err := git(root, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}
func fixtureRef(t *testing.T, path string) FileRef {
	t.Helper()
	hash, err := FileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	return FileRef{Path: path, SHA256: hash}
}

func checkoutFixture(t *testing.T, namespace string) (CheckoutRequest, Config) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("original archive metadata requires macOS cgo")
	}
	codec, err := exec.LookPath("zstd")
	if err != nil {
		t.Fatal("required original zstd unavailable", err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMetadata(base); err != nil {
		t.Fatal("original metadata capability unavailable", err)
	}
	repo := filepath.Join(base, "repo")
	if err = os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "init", "-b", "main")
	fixtureGit(t, repo, "config", "user.name", "Archive Fixture")
	fixtureGit(t, repo, "config", "user.email", "archive@example.invalid")
	fixtureWrite(t, filepath.Join(repo, "code.txt"), []byte("committed code\n"))
	fixtureWrite(t, filepath.Join(repo, ".gitignore"), []byte("artifact\nnode_modules/\n"))
	fixtureWrite(t, filepath.Join(repo, "package.json"), []byte("{}\n"))
	fixtureWrite(t, filepath.Join(repo, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"))
	fixtureGit(t, repo, "add", ".")
	fixtureGit(t, repo, "commit", "-m", "fixture source")
	roots := filepath.Join(base, "checkouts")
	if err = os.Mkdir(roots, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(roots, "candidate")
	fixtureGit(t, repo, "worktree", "add", "-b", "candidate", root)
	common := fixtureGit(t, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	admin := fixtureGit(t, root, "rev-parse", "--absolute-git-dir")
	token := strings.Repeat("a", 64)
	fixtureJSON(t, filepath.Join(admin, "noodle-incarnation.json"), token)
	c := Config{Roots: []string{roots}, ArchiveRoot: filepath.Join(base, "archive"), BlobRoot: filepath.Join(base, "cas"), StateDir: filepath.Join(base, "state"), Codec: codec, MaxItems: 1, ItemTimeoutSeconds: 30}
	for _, p := range []string{c.ArchiveRoot, c.BlobRoot, c.StateDir} {
		if err = os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixtureWrite(t, filepath.Join(c.StateDir, "owner.key"), []byte(strings.Repeat("k", 32)))
	configPath := filepath.Join(base, "config.json")
	fixtureJSON(t, configPath, c)
	r := CheckoutRequest{Operation: "fixture-operation", Identity: CheckoutIdentity{Repo: common, Path: root, Registration: admin, Head: fixtureGit(t, root, "rev-parse", "HEAD"), Ref: "refs/heads/candidate", Incarnation: token}, Config: fixtureRef(t, configPath)}
	inv := TestInventory{Owner: "test-manager", Status: "observed", Namespace: filepath.Join(admin, "soodles-test-requests.noindex"), NamespaceObservation: "observed_absent", Disposition: "OBSERVED_ABSENCE", Next: json.RawMessage("null")}
	inv.Origin.Repository = root
	inv.Origin.GitDir = admin
	inv.Origin.CommonDir = common
	for _, p := range []string{filepath.Join(root, ".git"), filepath.Join(admin, "gitdir"), filepath.Join(admin, "commondir")} {
		inv.Origin.BindingFiles = append(inv.Origin.BindingFiles, fixtureRef(t, p))
	}
	if namespace == "empty" {
		if err = os.Mkdir(inv.Namespace, 0700); err != nil {
			t.Fatal(err)
		}
		inv.NamespaceObservation = "observed_empty"
		inv.Disposition = "PRESERVE"
	}
	inventoryPath := filepath.Join(base, "inventory.json")
	fixtureJSON(t, inventoryPath, inv)
	r.Inventory = fixtureRef(t, inventoryPath)
	return r, c
}

func TestCheckoutPreservationIndependentRestore(t *testing.T) {
	for _, ns := range []string{"absent", "empty"} {
		t.Run(ns, func(t *testing.T) {
			r, c := checkoutFixture(t, ns)
			fixtureWrite(t, filepath.Join(r.Identity.Path, "artifact"), []byte("ignored irreplaceable bytes\n"))
			fixtureWrite(t, filepath.Join(r.Identity.Path, "node_modules", "unhashed"), []byte("rebuild from tracked lockfile"))
			hash, err := FileHash(filepath.Join(r.Identity.Path, "artifact"))
			if err != nil {
				t.Fatal(err)
			}
			r.Ignored = []CheckoutDisposition{{Path: "artifact", Owner: "worktree", Disposition: "PRESERVE", SHA256: hash}}
			before, err := Capture(r.Identity.Path, []string{"node_modules"})
			if err != nil {
				t.Fatal(err)
			}
			p, err := PrepareCheckout(r)
			if err != nil {
				t.Fatal(err)
			}
			if p.AuthorizesGC || p.RegistrationInstalled || p.Phase != "verified" {
				t.Fatal("authority/completion changed")
			}
			if err = VerifyTree(r.Identity.Path, before); err != nil {
				t.Fatal("source changed", err)
			}
			if err = verifyRecovery(c, p); err != nil {
				t.Fatal(err)
			}
			refs, ok := p.Source.AdminTree.Entries["refs"]
			if !ok || refs.Kind != "dir" || !refs.HasMetadata {
				t.Fatal("native empty Git refs directory is absent from registration recovery manifest")
			}
			refPath := filepath.Join(p.RegistrationRecovery, "refs")
			refEntries, err := os.ReadDir(refPath)
			if err != nil || len(refEntries) != 0 {
				t.Fatal("native Git refs directory was not restored empty", err)
			}
			refInfo, err := os.Stat(refPath)
			if err != nil {
				t.Fatal(err)
			}
			refMetadata, err := ReadMetadata(refPath)
			if err != nil || uint32(refInfo.Mode().Perm()) != refs.Mode || !reflect.DeepEqual(refMetadata, refs.Metadata) {
				t.Fatal("restored Git refs directory mode or metadata differs", err)
			}
			raw, err := os.ReadFile(filepath.Join(p.Recovery, "artifact"))
			if err != nil || string(raw) != "ignored irreplaceable bytes\n" {
				t.Fatal("actual restored bytes missing", err)
			}
			if _, err = os.Lstat(filepath.Join(p.Recovery, "node_modules")); !os.IsNotExist(err) {
				t.Fatal("dependency environment was archived")
			}
			if _, err = os.Stat(filepath.Join(p.RegistrationRecovery, "index")); err != nil {
				t.Fatal("index recovery bytes missing", err)
			}
			prior, err := os.ReadFile(packetPath(c, r.Operation))
			if err != nil {
				t.Fatal(err)
			}
			again, err := PrepareCheckout(r)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(p, again) {
				t.Fatal("immutable reuse differs")
			}
			if _, err = InspectCheckout(p); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(packetPath(c, r.Operation))
			if err != nil || string(prior) != string(after) {
				t.Fatal("inspect mutated packet")
			}
		})
	}
}

func fixtureCommitRecord(t *testing.T, r *CheckoutRequest) string {
	t.Helper()
	fixtureWrite(t, filepath.Join(r.Identity.Path, "code.txt"), []byte("later committed code\n"))
	fixtureGit(t, r.Identity.Path, "add", "code.txt")
	fixtureGit(t, r.Identity.Path, "commit", "-m", "original commit record\n\nBody retained by Git.")
	r.Identity.Head = fixtureGit(t, r.Identity.Path, "rev-parse", "HEAD")
	path := filepath.Join(r.Identity.Registration, "COMMIT_EDITMSG")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("original Git commit did not produce a plain COMMIT_EDITMSG", err)
	}
	return path
}

func TestCheckoutPreservationCommitRecordRestore(t *testing.T) {
	r, c := checkoutFixture(t, "empty")
	path := fixtureCommitRecord(t, &r)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	for _, command := range []*exec.Cmd{
		exec.Command("/usr/bin/xattr", "-w", "com.noodle.archive.control", "original commit metadata", path),
		exec.Command("/bin/chmod", "+a", "everyone allow read", path),
	} {
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("set original commit metadata: %v: %s", err, out)
		}
	}
	stamp := time.Unix(1700000200, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ReadMetadata(path)
	if err != nil || len(metadata.Xattrs) == 0 || metadata.ACL == "" {
		t.Fatal("commit record extended metadata fixture is unavailable", err)
	}
	source, err := InspectSource(c, r.Identity.Path)
	if err != nil {
		t.Fatal("normal committed source refused", err)
	}
	entry, ok := source.AdminTree.Entries["COMMIT_EDITMSG"]
	if !ok || entry.Kind != "file" || !entry.HasMetadata || entry.Hash != fixtureRef(t, path).SHA256 || entry.Size != int64(len(raw)) || entry.Mode != 0640 || entry.MtimeNS != stamp.UnixNano() || !reflect.DeepEqual(entry.Metadata, metadata) {
		t.Fatal("actual commit record bytes or metadata missing from AdminTree", entry)
	}
	p, err := PrepareCheckout(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthorizesGC || p.RegistrationInstalled || p.Phase != "verified" {
		t.Fatal("commit record preservation granted authority")
	}
	destination := filepath.Join(filepath.Dir(r.Config.Path), "commit-record-recovery")
	if err := RestoreCheckoutRecovery(p, destination); err != nil {
		t.Fatal(err)
	}
	for _, registration := range []string{p.RegistrationRecovery, filepath.Join(destination, "registration")} {
		if err := VerifyTree(registration, source.AdminTree); err != nil {
			t.Fatal("restored registration differs", err)
		}
		restored, err := os.ReadFile(filepath.Join(registration, "COMMIT_EDITMSG"))
		if err != nil || string(restored) != string(raw) {
			t.Fatal("actual commit record bytes were not restored", err)
		}
	}
	again, err := PrepareCheckout(r)
	if err != nil || !reflect.DeepEqual(p, again) {
		t.Fatal("same operation lost commit record or changed packet", err)
	}
	if _, err := InspectCheckout(p); err != nil {
		t.Fatal("commit record readback failed", err)
	}
}

func TestCheckoutPreservationCommitRecordRefusals(t *testing.T) {
	type refusal struct {
		name, want string
		mutate     func(*testing.T, CheckoutRequest, string)
	}
	cases := []refusal{
		{"directory", "Git commit record is not a plain file: COMMIT_EDITMSG", func(t *testing.T, r CheckoutRequest, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", "registration symlink requires retention: COMMIT_EDITMSG", func(t *testing.T, r CheckoutRequest, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("HEAD", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", "Git commit record is not a plain file: COMMIT_EDITMSG", func(t *testing.T, r CheckoutRequest, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("/usr/bin/mkfifo", path).CombinedOutput(); err != nil {
				t.Fatalf("create nonregular commit record: %v: %s", err, out)
			}
		}},
	}
	for _, name := range []string{"index.lock", "HEAD.lock", "other.lock", "MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "sequencer", "unknown.sqlite", "foreign"} {
		cases = append(cases, refusal{name, name, func(t *testing.T, r CheckoutRequest, _ string) {
			fixtureWrite(t, filepath.Join(r.Identity.Registration, name), []byte("unresolved"))
		}})
	}
	cases = append(cases, refusal{"nonempty-refs", "nonempty Git registration refs", func(t *testing.T, r CheckoutRequest, _ string) {
		fixtureGit(t, r.Identity.Path, "update-ref", "refs/worktree/retained", r.Identity.Head)
	}})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := checkoutFixture(t, "empty")
			path := fixtureCommitRecord(t, &r)
			p, err := PrepareCheckout(r)
			if err != nil {
				t.Fatal("same-fixture legal commit baseline refused", err)
			}
			tc.mutate(t, r, path)
			_, err = InspectCheckout(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q refusal after valid commit baseline, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckoutPreservationCommitRecordDrift(t *testing.T) {
	for _, mutation := range []string{"source-bytes", "source-mode", "source-mtime", "source-xattr", "source-acl", "restore-bytes", "restore-mode", "restore-mtime", "restore-xattr", "restore-acl"} {
		t.Run(mutation, func(t *testing.T) {
			r, _ := checkoutFixture(t, "empty")
			path := fixtureCommitRecord(t, &r)
			p, err := PrepareCheckout(r)
			if err != nil {
				t.Fatal("same-fixture legal commit baseline refused", err)
			}
			if strings.HasPrefix(mutation, "restore-") {
				path = filepath.Join(p.RegistrationRecovery, "COMMIT_EDITMSG")
			}
			switch mutation {
			case "source-bytes", "restore-bytes":
				fixtureWrite(t, path, []byte("changed record"))
			case "source-mode", "restore-mode":
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, info.Mode().Perm()^0040); err != nil {
					t.Fatal(err)
				}
			case "source-mtime", "restore-mtime":
				stamp := time.Unix(1700000300, 0)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			case "source-xattr", "source-acl", "restore-xattr", "restore-acl":
				command := exec.Command("/usr/bin/xattr", "-w", "com.noodle.archive.control", "drift", path)
				if strings.HasSuffix(mutation, "-acl") {
					command = exec.Command("/bin/chmod", "+a", "everyone allow read", path)
				}
				if out, err := command.CombinedOutput(); err != nil {
					t.Fatalf("change commit record metadata: %v: %s", err, out)
				}
			}
			_, err = InspectCheckout(p)
			want := "drift"
			if strings.HasPrefix(mutation, "restore-") {
				want = "differs"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want %q refusal after commit record mutation, got %v", want, err)
			}
		})
	}
}

func TestCheckoutPreservationIndependentRefusals(t *testing.T) {
	cases := []struct {
		name, want string
		mutate     func(*testing.T, *CheckoutRequest, Config)
	}{
		{"wrong-ref", "identity/ref", func(t *testing.T, r *CheckoutRequest, c Config) { r.Identity.Ref = "refs/heads/foreign" }},
		{"wrong-incarnation", "incarnation", func(t *testing.T, r *CheckoutRequest, c Config) { r.Identity.Incarnation = strings.Repeat("b", 64) }},
		{"missing-index", "required Git registration", func(t *testing.T, r *CheckoutRequest, c Config) {
			if err := os.Remove(filepath.Join(r.Identity.Registration, "index")); err != nil {
				t.Fatal(err)
			}
		}},
		{"foreign-registration", "identity/ref", func(t *testing.T, r *CheckoutRequest, c Config) { r.Identity.Registration += "-foreign" }},
		{"unknown-owner", "unclassified", func(t *testing.T, r *CheckoutRequest, c Config) {
			fixtureWrite(t, filepath.Join(r.Identity.Path, "artifact"), []byte("unknown"))
		}},
		{"unknown-registration", "separate preservation", func(t *testing.T, r *CheckoutRequest, c Config) {
			fixtureWrite(t, filepath.Join(r.Identity.Registration, "unknown.sqlite"), []byte("unknown"))
		}},
		{"extra-metadata", "extra/unclassified", func(t *testing.T, r *CheckoutRequest, c Config) {
			fixtureWrite(t, filepath.Join(r.Identity.Registration, "soodles-test-requests.noindex", "extra"), []byte("new"))
		}},
		{"symlink", "symlink", func(t *testing.T, r *CheckoutRequest, c Config) {
			path := filepath.Join(r.Identity.Registration, "ORIG_HEAD")
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() {
				t.Fatal("fixture ORIG_HEAD is not an ordinary file")
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != r.Identity.Head+"\n" {
				t.Fatal("fixture ORIG_HEAD differs from controlled source HEAD", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("HEAD", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"dirty", "uncommitted", func(t *testing.T, r *CheckoutRequest, c Config) {
			fixtureWrite(t, filepath.Join(r.Identity.Path, "code.txt"), []byte("dirty"))
		}},
		{"tracked-node-modules", "tracked content", func(t *testing.T, r *CheckoutRequest, c Config) {
			fixtureWrite(t, filepath.Join(r.Identity.Path, "node_modules", "bad"), []byte("bad"))
			fixtureGit(t, r.Identity.Path, "add", "-f", "node_modules/bad")
			fixtureGit(t, r.Identity.Path, "commit", "-m", "tracked dependency")
			r.Identity.Head = fixtureGit(t, r.Identity.Path, "rev-parse", "HEAD")
		}},
		{"bad-inventory-hash", "hash differs", func(t *testing.T, r *CheckoutRequest, c Config) { r.Inventory.SHA256 = strings.Repeat("0", 64) }},
		{"wrong-origin", "origin differs", func(t *testing.T, r *CheckoutRequest, c Config) {
			mutateFixtureInventory(t, r, func(i *TestInventory) { i.Origin.Repository += "-foreign" })
		}},
		{"unknown-inventory", "HOLD", func(t *testing.T, r *CheckoutRequest, c Config) {
			mutateFixtureInventory(t, r, func(i *TestInventory) { i.Disposition = "HOLD" })
		}},
		{"missing-inventory", "no such file", func(t *testing.T, r *CheckoutRequest, c Config) {
			if err := os.Remove(r.Inventory.Path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := checkoutFixture(t, "empty")
			tc.mutate(t, &r, c)
			_, err := PrepareCheckout(r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q refusal, got %v", tc.want, err)
			}
		})
	}
}

func mutateFixtureInventory(t *testing.T, r *CheckoutRequest, mutate func(*TestInventory)) {
	t.Helper()
	raw, err := os.ReadFile(r.Inventory.Path)
	if err != nil {
		t.Fatal(err)
	}
	var inv TestInventory
	if err = DecodeCheckoutJSON(raw, &inv); err != nil {
		t.Fatal(err)
	}
	mutate(&inv)
	fixtureJSON(t, r.Inventory.Path, inv)
	r.Inventory = fixtureRef(t, r.Inventory.Path)
}

func TestCheckoutPreservationReadbackCorruption(t *testing.T) {
	cases := []struct {
		name, want string
		mutate     func(*testing.T, CheckoutPacket, Config)
	}{
		{"source-drift", "drift", func(t *testing.T, p CheckoutPacket, c Config) {
			fixtureWrite(t, filepath.Join(p.Request.Identity.Path, "artifact"), []byte("drift"))
		}},
		{"missing-manifest", "no such file", func(t *testing.T, p CheckoutPacket, c Config) {
			if err := os.Remove(p.Manifest.Path); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt-manifest", "hash differs", func(t *testing.T, p CheckoutPacket, c Config) { fixtureWrite(t, p.Manifest.Path, []byte("corrupt")) }},
		{"missing-object", "No such file", func(t *testing.T, p CheckoutPacket, c Config) {
			path, err := (Store{Root: c.BlobRoot, Codec: c.Codec}).blob(p.CheckoutLink.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt-object", "decode blob", func(t *testing.T, p CheckoutPacket, c Config) {
			path, err := (Store{Root: c.BlobRoot, Codec: c.Codec}).blob(p.CheckoutLink.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			fixtureWrite(t, path, []byte("corrupt zstd"))
		}},
		{"candidate-ref", "candidate ref", func(t *testing.T, p CheckoutPacket, c Config) {
			fixtureGit(t, p.Pool, "update-ref", "-d", p.CandidateRef)
		}},
		{"restore-drift", "differs", func(t *testing.T, p CheckoutPacket, c Config) {
			fixtureWrite(t, filepath.Join(p.RegistrationRecovery, "HEAD"), []byte("foreign"))
		}},
		{"missing-inventory-source", "no such file", func(t *testing.T, p CheckoutPacket, c Config) {
			if err := os.Remove(p.Request.Inventory.Path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := checkoutFixture(t, "empty")
			p, err := PrepareCheckout(r)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, p, c)
			_, err = InspectCheckout(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q refusal, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckoutPreservationPartialRetainsSource(t *testing.T) {
	r, c := checkoutFixture(t, "empty")
	if err := VerifyCheckoutUnreserved(r); err != nil {
		t.Fatal("fresh checkout was blocked", err)
	}
	// Block only the actual independent restore destination, after CAS/pool writes.
	dir := filepath.Join(c.ArchiveRoot, "checkout-preservation", rootKey(r.Operation)+".recovery", "checkout")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureWrite(t, filepath.Join(dir, "occupied"), []byte("protected"))
	_, err := PrepareCheckout(r)
	if err == nil || !strings.Contains(err.Error(), "independent restore failed") {
		t.Fatal("restore failure not surfaced", err)
	}
	if _, err = os.Stat(filepath.Join(r.Identity.Path, "code.txt")); err != nil {
		t.Fatal("source lost", err)
	}
	if _, err = os.Stat(filepath.Join(c.ArchiveRoot, "git-pools", rootKey(r.Identity.Repo)+".git")); err != nil {
		t.Fatal("external data lost", err)
	}
	if _, err = ReadCheckoutPacket(r.Config, r.Operation); !os.IsNotExist(err) {
		t.Fatal("partial archive unexpectedly published its final packet", err)
	}
	if err = VerifyCheckoutUnreserved(r); err == nil || !strings.Contains(err.Error(), "pending readback") {
		t.Fatal("a missing packet masked the active archive reservation", err)
	}
	if _, err = PrepareCheckout(r); err == nil || !strings.Contains(err.Error(), "already reserved") {
		t.Fatal("partial replay allowed", err)
	}
	r.Operation = "changed-operation"
	if err = VerifyCheckoutUnreserved(r); err == nil || !strings.Contains(err.Error(), "pending readback") {
		t.Fatal("another operation masked the original checkout reservation", err)
	}
	if _, err = PrepareCheckout(r); err == nil || !strings.Contains(err.Error(), "already reserved") {
		t.Fatal("renamed replay allowed", err)
	}
}

func TestCheckoutSourceRejectsPendingBranchRefWrite(t *testing.T) {
	r, c := checkoutFixture(t, "empty")
	before, err := InspectSource(c, r.Identity.Path)
	if err != nil || before.Branch == "" {
		t.Fatal("original linked branch unavailable", err)
	}
	fixtureWrite(t, filepath.Join(r.Identity.Repo, before.Branch+".lock"), []byte(before.Head+"\n"))
	if _, err = InspectSource(c, r.Identity.Path); err == nil || !strings.Contains(err.Error(), "Git branch lock") {
		t.Fatal("unchanged HEAD masked a pending branch update", err)
	}
}

func TestCheckoutPreservationStrictJSON(t *testing.T) {
	for _, raw := range []string{`{"unexpected":true}`, `{} {}`} {
		if err := DecodeCheckoutJSON([]byte(raw), new(CheckoutRequest)); err == nil {
			t.Fatalf("accepted invalid input %s", raw)
		}
	}
}

// This fixture freezes the accepted Consumer's schema. It does not substitute
// for original Test Manager adoption or execution of its native controls.
func TestCheckoutPreservationTerminalSchema(t *testing.T) {
	for _, state := range []string{"completed", "failed", "refused", "unknown"} {
		t.Run(state, func(t *testing.T) {
			r, _ := checkoutFixture(t, "empty")
			selection := map[string]any{}
			request := map[string]any{"schema": 1, "repository": r.Identity.Path, "selection": selection, "source": map[string]any{}, "implementation": map[string]any{}, "demand": map[string]any{}}
			requestRaw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			id := rootKey(string(requestRaw))
			ns := filepath.Join(r.Identity.Registration, "soodles-test-requests.noindex")
			fixtureWrite(t, filepath.Join(ns, id, "request.json"), requestRaw)
			exit := 0
			if state == "failed" {
				exit = 1
			}
			fixtureJSON(t, filepath.Join(ns, id, "receipt.json"), map[string]any{"exit": exit, "selection": selection, "authorizes_landing": false})
			receipt := fixtureRef(t, filepath.Join(ns, id, "receipt.json"))
			fixtureJSON(t, filepath.Join(ns, id, "state.json"), map[string]any{"status": state, "receipt_sha256": receipt.SHA256})
			fixtureWrite(t, filepath.Join(ns, id, "execution.lock"), nil)
			lookup := map[string]any{"repository": r.Identity.Path, "source": map[string]any{}, "implementation": map[string]any{}, "demand": map[string]any{}}
			lookupRaw, err := json.Marshal(lookup)
			if err != nil {
				t.Fatal(err)
			}
			key := rootKey(string(lookupRaw))
			fixtureJSON(t, filepath.Join(ns, key+".json"), map[string]any{"request_id": id})
			fixtureWrite(t, filepath.Join(ns, key+".lock"), nil)
			mutateFixtureInventory(t, &r, func(i *TestInventory) {
				i.NamespaceObservation = "observed_present"
				for _, name := range []string{"request.json", "receipt.json", "state.json", "execution.lock"} {
					i.Files = append(i.Files, fixtureRef(t, filepath.Join(ns, id, name)))
				}
				for _, name := range []string{key + ".json", key + ".lock"} {
					i.Files = append(i.Files, fixtureRef(t, filepath.Join(ns, name)))
				}
				i.Requests = append(i.Requests, TestRequestDisposition{RequestID: id, State: state, Disposition: "PRESERVE", Gaps: []string{}, Receipt: &receipt})
			})
			_, err = PrepareCheckout(r)
			if state == "unknown" {
				if err == nil || !strings.Contains(err.Error(), "nonterminal/UNKNOWN") {
					t.Fatal("UNKNOWN accepted", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckoutPreservationOperationConflict(t *testing.T) {
	r, _ := checkoutFixture(t, "empty")
	p, err := PrepareCheckout(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Ignored = []CheckoutDisposition{{Path: "foreign", Owner: "worktree", Disposition: "PRESERVE"}}
	if _, err = PrepareCheckout(r); err == nil || !strings.Contains(err.Error(), "operation conflict") {
		t.Fatal("operation conflict accepted", err)
	}
	p.Signature = strings.Repeat("0", 64)
	if _, err = InspectCheckout(p); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatal("corrupt packet accepted", err)
	}
}

func TestCheckoutPreservationRecoveryUsesExternalBytes(t *testing.T) {
	r, _ := checkoutFixture(t, "empty")
	p, err := PrepareCheckout(r)
	if err != nil {
		t.Fatal(err)
	}
	fixtureWrite(t, filepath.Join(r.Identity.Path, "code.txt"), []byte("later source drift\n"))
	destination := filepath.Join(filepath.Dir(r.Config.Path), "manual-recovery")
	if err = RestoreCheckoutRecovery(p, destination); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(destination, "checkout", "code.txt"))
	if err != nil || string(raw) != "committed code\n" {
		t.Fatal("recovery depended on original checkout", err)
	}
	raw, err = os.ReadFile(filepath.Join(r.Identity.Path, "code.txt"))
	if err != nil || string(raw) != "later source drift\n" {
		t.Fatal("recovery changed source", err)
	}
	if _, err = InspectCheckout(p); err == nil {
		t.Fatal("source drift became GC evidence")
	}
	if err = RestoreCheckoutRecovery(p, destination); err == nil {
		t.Fatal("existing destination overwritten")
	}
}

func TestCheckoutPreservationNativeEmptyRefsBoundary(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, CheckoutRequest)
		want   string
	}{
		{"private-ref", func(t *testing.T, r CheckoutRequest) {
			fixtureGit(t, r.Identity.Path, "update-ref", "refs/worktree/retained", r.Identity.Head)
		}, "nonempty Git registration refs require separate preservation: refs/worktree"},
		{"misplaced-public-ref", func(t *testing.T, r CheckoutRequest) {
			fixtureWrite(t, filepath.Join(r.Identity.Registration, "refs", "heads", "foreign"), []byte(r.Identity.Head+"\n"))
		}, "nonempty Git registration refs require separate preservation: refs/heads"},
		{"unclassified-directory", func(t *testing.T, r CheckoutRequest) {
			if err := os.Mkdir(filepath.Join(r.Identity.Registration, "refs", "unclassified"), 0700); err != nil {
				t.Fatal(err)
			}
		}, "nonempty Git registration refs require separate preservation: refs/unclassified"},
		{"unclassified-file", func(t *testing.T, r CheckoutRequest) {
			fixtureWrite(t, filepath.Join(r.Identity.Registration, "refs", "unknown-owner"), []byte("unclassified bytes"))
		}, "nonempty Git registration refs require separate preservation: refs/unknown-owner"},
		{"private-ref-lock", func(t *testing.T, r CheckoutRequest) {
			fixtureWrite(t, filepath.Join(r.Identity.Registration, "refs", "worktree", "retained.lock"), nil)
		}, "nonempty Git registration refs require separate preservation: refs/worktree"},
		{"refs-is-file", func(t *testing.T, r CheckoutRequest) {
			path := filepath.Join(r.Identity.Registration, "refs")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			fixtureWrite(t, path, []byte("foreign bytes"))
		}, "Git registration refs is not a plain directory: refs"},
		{"refs-is-symlink", func(t *testing.T, r CheckoutRequest) {
			path := filepath.Join(r.Identity.Registration, "refs")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("logs", path); err != nil {
				t.Fatal(err)
			}
		}, "registration symlink requires retention: refs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := checkoutFixture(t, "empty")
			refsPath := filepath.Join(r.Identity.Registration, "refs")
			entries, err := os.ReadDir(refsPath)
			if err != nil || len(entries) != 0 {
				t.Fatal("original Git worktree add did not create the expected empty registration refs directory", err)
			}
			privateNamespace := fixtureGit(t, r.Identity.Path, "rev-parse", "--path-format=absolute", "--git-path", "refs/worktree")
			if filepath.Dir(privateNamespace) != refsPath {
				t.Fatal("original Git private ref namespace does not belong to this registration", privateNamespace)
			}
			baseline, err := InspectSource(c, r.Identity.Path)
			if err != nil {
				t.Fatal("same-fixture legal baseline refused before mutation", err)
			}
			entry, ok := baseline.AdminTree.Entries["refs"]
			metadata, err := ReadMetadata(refsPath)
			if err != nil || !ok || entry.Kind != "dir" || !entry.HasMetadata || !reflect.DeepEqual(entry.Metadata, metadata) {
				t.Fatal("legal Git refs directory metadata was not preserved", err)
			}
			if err := VerifyTree(r.Identity.Registration, baseline.AdminTree); err != nil {
				t.Fatal("legal baseline registration tree differs", err)
			}
			tc.mutate(t, r)
			_, err = InspectSource(c, r.Identity.Path)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("want exact refs disposition %q after a valid baseline, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckoutConfigParsesCheckedBytes(t *testing.T) {
	r, original := checkoutFixture(t, "empty")
	baseline, err := checkoutConfig(r)
	if err != nil || !reflect.DeepEqual(baseline, original) {
		t.Fatal("valid Config baseline differs", err)
	}
	checked, err := readRef(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	replacement := original
	replacement.ArchiveRoot = filepath.Join(filepath.Dir(r.Config.Path), "replacement-archive")
	if err := os.Mkdir(replacement.ArchiveRoot, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureJSON(t, r.Config.Path, replacement)
	fromPath, err := LoadConfig(r.Config.Path)
	if err != nil || !reflect.DeepEqual(fromPath, replacement) {
		t.Fatal("replacement is not a distinct valid Config", err)
	}
	fromChecked, err := parseConfig(checked)
	if err != nil || !reflect.DeepEqual(fromChecked, original) {
		t.Fatal("checked Config bytes were replaced by pathname bytes", err)
	}
	if _, err := checkoutConfig(r); err == nil || err.Error() != "FileRef hash differs: "+r.Config.Path {
		t.Fatal("persistent Config hash drift was not refused", err)
	}
	invalid := original
	invalid.MaxItems = 0
	raw, err := json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig(raw); err == nil || err.Error() != "archive roots or processing limits are missing" {
		t.Fatal("checked-byte parser bypassed original limits validation", err)
	}
	invalid = original
	invalid.ArchiveRoot = original.Roots[0]
	raw, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig(raw); err == nil || err.Error() != "archive storage overlaps a managed root" {
		t.Fatal("checked-byte parser bypassed original storage validation", err)
	}
}

func TestCheckoutPreservationOwnedBindingRoot(t *testing.T) {
	r, _ := checkoutFixture(t, "empty")
	p, err := PrepareCheckout(r)
	if err != nil {
		t.Fatal("coherent restore baseline failed", err)
	}
	binding := filepath.Join(filepath.Dir(p.Recovery), "binding")
	observed, err := ReadMetadata(binding)
	if err != nil || !reflect.DeepEqual(observed, p.OwnedBindingRoot) {
		t.Fatal("packet did not record the newly owned container metadata", err)
	}
	originalLink, err := checkoutLink(r.Identity.Path)
	if err != nil || !reflect.DeepEqual(originalLink, p.CheckoutLink) {
		t.Fatal("original Git link bytes/mode/metadata changed", err)
	}
	if err := VerifyTree(binding, linkTree(p)); err != nil {
		t.Fatal("owned container baseline readback failed", err)
	}
	if _, err := InspectCheckout(p); err != nil {
		t.Fatal("packet baseline readback failed", err)
	}
	command := exec.Command("/usr/bin/xattr", "-w", "com.noodle.archive.control", "changed", binding)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("change owned container xattr: %v: %s", err, out)
	}
	if _, err := InspectCheckout(p); err == nil || !strings.Contains(err.Error(), "restored root permissions or extended metadata differs") {
		t.Fatal("owned container metadata corruption was not refused", err)
	}
}

func TestCheckoutManifestContentIdentity(t *testing.T) {
	hash := rootKey("same bytes")
	first := Entry{Kind: "file", Hash: hash, Size: 10, Mode: 0600, MtimeNS: 1, HasMetadata: true, Metadata: Metadata{Xattrs: map[string]string{"control": "YQ=="}}}
	second := first
	second.Mode, second.MtimeNS = 0640, 2
	second.Metadata = Metadata{Xattrs: map[string]string{"control": "Yg=="}}
	packet := CheckoutPacket{Tree: Tree{Entries: map[string]Entry{"first": first, "second": second}}, Source: Source{AdminTree: Tree{Entries: map[string]Entry{"lock": second}}}, CheckoutLink: first}
	before, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := checkoutManifest(packet)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Entry{hash: {Kind: "file", Hash: hash, Size: 10, HasMetadata: true, Metadata: Metadata{Xattrs: map[string]string{}}}}
	if !reflect.DeepEqual(manifest, want) {
		t.Fatal("content manifest contains path metadata", manifest)
	}
	after, err := json.Marshal(packet)
	if err != nil || string(before) != string(after) {
		t.Fatal("manifest producer altered per-path metadata", err)
	}
	packet.Tree.Entries["first"], packet.Tree.Entries["second"] = second, first
	packet.Source.AdminTree.Entries["lock"], packet.CheckoutLink = first, second
	reversed, err := checkoutManifest(packet)
	if err != nil || !reflect.DeepEqual(reversed, manifest) {
		t.Fatal("manifest depends on the metadata of the last same-content path", err)
	}
	packet.CheckoutLink.Size++
	if _, err := checkoutManifest(packet); err == nil || err.Error() != "CAS content size conflict: "+hash {
		t.Fatal("same-hash size conflict was not refused", err)
	}
}

func TestCheckoutPreservationSharedContentAndTerminalLocks(t *testing.T) {
	for _, state := range []string{"completed", "failed"} {
		t.Run(state, func(t *testing.T) {
			r, _ := checkoutFixture(t, "empty")
			selection := map[string]any{}
			request := map[string]any{"schema": 1, "repository": r.Identity.Path, "selection": selection, "source": map[string]any{}, "implementation": map[string]any{}, "demand": map[string]any{}}
			requestRaw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			id := rootKey(string(requestRaw))
			ns := filepath.Join(r.Identity.Registration, "soodles-test-requests.noindex")
			fixtureWrite(t, filepath.Join(ns, id, "request.json"), requestRaw)
			exit := 0
			if state == "failed" {
				exit = 1
			}
			fixtureJSON(t, filepath.Join(ns, id, "receipt.json"), map[string]any{"exit": exit, "selection": selection, "authorizes_landing": false})
			receipt := fixtureRef(t, filepath.Join(ns, id, "receipt.json"))
			fixtureJSON(t, filepath.Join(ns, id, "state.json"), map[string]any{"status": state, "receipt_sha256": receipt.SHA256})
			executionLock := filepath.Join(ns, id, "execution.lock")
			fixtureWrite(t, executionLock, nil)
			lookup := map[string]any{"repository": r.Identity.Path, "source": map[string]any{}, "implementation": map[string]any{}, "demand": map[string]any{}}
			lookupRaw, err := json.Marshal(lookup)
			if err != nil {
				t.Fatal(err)
			}
			key := rootKey(string(lookupRaw))
			fixtureJSON(t, filepath.Join(ns, key+".json"), map[string]any{"request_id": id})
			prepareLock := filepath.Join(ns, key+".lock")
			fixtureWrite(t, prepareLock, nil)
			for i, path := range []string{executionLock, prepareLock} {
				stamp := time.Unix(1700000000+int64(i), 0)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			mutateFixtureInventory(t, &r, func(inv *TestInventory) {
				inv.NamespaceObservation = "observed_present"
				for _, name := range []string{"request.json", "receipt.json", "state.json", "execution.lock"} {
					inv.Files = append(inv.Files, fixtureRef(t, filepath.Join(ns, id, name)))
				}
				for _, name := range []string{key + ".json", key + ".lock"} {
					inv.Files = append(inv.Files, fixtureRef(t, filepath.Join(ns, name)))
				}
				inv.Requests = append(inv.Requests, TestRequestDisposition{RequestID: id, State: state, Disposition: "PRESERVE", Gaps: []string{}, Receipt: &receipt})
			})
			for i, name := range []string{"first", "second"} {
				rel := filepath.Join("artifact", name)
				path := filepath.Join(r.Identity.Path, rel)
				fixtureWrite(t, path, []byte("identical ignored bytes\n"))
				mode := os.FileMode(0600)
				if i == 1 {
					mode = 0640
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				command := exec.Command("/usr/bin/xattr", "-w", "com.noodle.archive.control", name, path)
				if out, err := command.CombinedOutput(); err != nil {
					t.Fatalf("set distinct source xattrs: %v: %s", err, out)
				}
				stamp := time.Unix(1700000100+int64(i), 0)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				r.Ignored = append(r.Ignored, CheckoutDisposition{Path: rel, Owner: "worktree", Disposition: "PRESERVE", SHA256: fixtureRef(t, path).SHA256})
			}
			r.Ignored = append(r.Ignored, CheckoutDisposition{Path: "artifact", Owner: "worktree", Disposition: "PRESERVE"})
			before, err := Capture(r.Identity.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			p, err := PrepareCheckout(r)
			if err != nil {
				t.Fatal("same-content/terminal coherent baseline failed", err)
			}
			if err := VerifyTree(p.Recovery, before); err != nil {
				t.Fatal("per-path source metadata was lost during deduplication", err)
			}
			first, second := p.Tree.Entries["artifact/first"], p.Tree.Entries["artifact/second"]
			if first.Hash != second.Hash || first.Mode == second.Mode || first.MtimeNS == second.MtimeNS || reflect.DeepEqual(first.Metadata, second.Metadata) {
				t.Fatal("same-content distinct-metadata control is not discriminating")
			}
			execution := p.Source.AdminTree.Entries[filepath.Join("soodles-test-requests.noindex", id, "execution.lock")]
			prepare := p.Source.AdminTree.Entries[filepath.Join("soodles-test-requests.noindex", key+".lock")]
			if execution.Hash != prepare.Hash || execution.Size != 0 || prepare.Size != 0 || execution.MtimeNS == prepare.MtimeNS {
				t.Fatal("original terminal empty-lock control is not discriminating")
			}
			expected, err := checkoutManifest(p)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := ReadManifest(p.Manifest.Path, "")
			if err != nil || !reflect.DeepEqual(persisted, expected) {
				t.Fatal("producer/readback content manifests differ", err)
			}
			if _, err := InspectCheckout(p); err != nil {
				t.Fatal("stable packet readback failed", err)
			}
			if _, err := PrepareCheckout(r); err != nil {
				t.Fatal("immutable packet reuse failed", err)
			}
			if err := os.Chmod(filepath.Join(p.Recovery, "artifact", "second"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectCheckout(p); err == nil || !strings.Contains(err.Error(), "artifact/second") {
				t.Fatal("per-path restored metadata corruption was not refused", err)
			}
		})
	}
}

func TestCheckoutPreservationForeignManifestHash(t *testing.T) {
	r, _ := checkoutFixture(t, "empty")
	p, err := PrepareCheckout(r)
	if err != nil {
		t.Fatal("manifest corruption control needs a coherent restore baseline", err)
	}
	if _, err := InspectCheckout(p); err != nil {
		t.Fatal("manifest baseline readback failed", err)
	}
	entries, err := ReadManifest(p.Manifest.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	original, ok := entries[p.CheckoutLink.Hash]
	if !ok {
		t.Fatal("binding blob missing from valid baseline manifest")
	}
	foreignHash := rootKey("foreign manifest content")
	if _, ok := entries[foreignHash]; ok {
		t.Fatal("foreign-hash control collides with baseline")
	}
	delete(entries, original.Hash)
	original.Hash = foreignHash
	entries[foreignHash] = original
	fixtureJSON(t, p.Manifest.Path, entries)
	if _, err := InspectCheckout(p); err == nil || err.Error() != "FileRef hash differs: "+p.Manifest.Path {
		t.Fatal("single-fault foreign manifest hash was not refused", err)
	}
	if err := VerifyTree(r.Identity.Path, p.Tree); err != nil {
		t.Fatal("manifest refusal changed original source", err)
	}
}
