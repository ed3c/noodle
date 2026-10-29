package guestprogress

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureIdentity() (Identity, string) {
	token := strings.Repeat("a", 64)
	return Identity{SessionID: "native-session", CommandSHA256: strings.Repeat("b", 64), TokenSHA256: digest([]byte(token))}, token
}
func canonicalTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(p, 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompletedStopReadsExactEvidenceWithoutSocket(t *testing.T) {
	dir := canonicalTemp(t)
	identity, token := fixtureIdentity()
	request := Request{Schema: 1, Identity: identity, Token: token, RequestID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	if err := writeJSON(dir, "identity.json", identity); err != nil {
		t.Fatal(err)
	}
	result := Receipt{Schema: 1, Identity: identity, RequestID: request.RequestID, Status: "archived", ChildrenReaped: true, ExitCode: 143, Archives: []Archive{{Name: "noodle-result.tar", SHA256: strings.Repeat("a", 64), Bytes: 1024}, {Name: "noodle-session.tar", SHA256: strings.Repeat("b", 64), Bytes: 1024}}}
	if err := writeJSON(dir, "result.json", result); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := Stop(context.Background(), dir, request)
		if err != nil || got.RequestID != request.RequestID {
			t.Fatalf("repeat read: %+v %v", got, err)
		}
	}
	for _, change := range []func(*Request){func(r *Request) { r.Token = strings.Repeat("c", 64) }, func(r *Request) { r.Identity.SessionID = "another" }, func(r *Request) { r.RequestID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" }} {
		changed := request
		change(&changed)
		if _, err := Stop(context.Background(), dir, changed); err == nil {
			t.Fatal("different stop identity accepted")
		}
	}
	result.ChildrenReaped = false
	if err := writeJSON(dir, "result.json", result); err != nil {
		t.Fatal(err)
	}
	if _, err := Stop(context.Background(), dir, request); err == nil {
		t.Fatal("unreaped result accepted")
	}
}

func TestArchiveExcludesDedicatedCredentialFile(t *testing.T) {
	root, output := canonicalTemp(t), canonicalTemp(t)
	if err := os.Mkdir(filepath.Join(root, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"progress.txt": "saved progress", "codex/auth.json": "fixture-private-value"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := archiveTree(root, output, "noodle-session.tar", map[string]bool{"codex/auth.json": true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, result.Name))
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 != digest(data) || result.Bytes != int64(len(data)) || strings.Contains(string(data), "fixture-private-value") {
		t.Fatal("archive evidence or credential exclusion differs")
	}
	reader := tar.NewReader(strings.NewReader(string(data)))
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "progress.txt" {
			value, err := io.ReadAll(reader)
			if err != nil || string(value) != "saved progress" {
				t.Fatal("progress lost")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("progress absent")
	}
	if _, err := archiveTree(root, output, "noodle-session.tar", nil); err == nil {
		t.Fatal("archive was overwritten")
	}
}

func TestControlReadinessDoesNotReplayOrInventCompletion(t *testing.T) {
	dir := canonicalTemp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := waitControlReady(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing original endpoint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stop.json")); !os.IsNotExist(err) {
		t.Fatal("readiness check created a stop intent")
	}
	if err := writeJSON(dir, "failure.json", Receipt{Status: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := waitControlReady(context.Background(), dir); err == nil {
		t.Fatal("retained supervisor failure accepted")
	}
	if err := os.Remove(filepath.Join(dir, "failure.json")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	go func() { done <- waitControlReady(ctx2, dir) }()
	if err := writeJSON(dir, "process.json", processBinding{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
