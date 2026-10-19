package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostShutdownPersistsCancelledContext(t *testing.T) {
	dir, err := os.MkdirTemp("", "n-shutdown-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("NOODLE_HOST_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveHost(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("isolated Host shutdown was not observed")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated Host socket unavailable", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	req := hostTestRequest(t, t.TempDir())
	req.RequestFile = filepath.Join(dir, "request.json")
	original := callHost(ctx, req)
	if original.Status != "ok" || original.ContextState != "active" {
		t.Fatalf("original attach: %+v", original)
	}
	requestBytes, err := os.ReadFile(req.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("isolated Host did not finish")
	}
	data, err := os.ReadFile(filepath.Join(dir, "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipts []hostReceipt
	if err := json.Unmarshal(data, &receipts); err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].ContextState != "failed" || receipts[0].TerminalCause != "" {
		t.Fatalf("shutdown lost the cancelled outcome: %+v", receipts)
	}
	if receipts[0].RequestID != original.RequestID || receipts[0].ContextID != original.ContextID {
		t.Fatal("shutdown replaced the original request or context")
	}
	after, err := os.ReadFile(req.RequestFile)
	if err != nil || string(after) != string(requestBytes) {
		t.Fatal("shutdown changed the original request", err)
	}
}
