package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostReadonlyWebLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveHost(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("isolated test Host did not stop")
		}
	}()
	var conn net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		conn, err = net.DialTimeout("unix", filepath.Join(dir, "host.sock"), 50*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("isolated test Host socket unavailable")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(hostRequest{Operation: "resources"}); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		WebURL         string `json:"web_url"`
		Contexts       int    `json:"contexts"`
		HostInstanceID string `json:"host_instance_id"`
	}
	if err := json.NewDecoder(conn).Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(receipt.WebURL, "http://127.0.0.1:") || !strings.HasSuffix(receipt.WebURL, "/loops") || receipt.Contexts != 0 {
		t.Fatalf("serving receipt: %+v", receipt)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(strings.TrimSuffix(receipt.WebURL, "/loops") + "/api/loops")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !strings.Contains(string(body), receipt.HostInstanceID) || !strings.Contains(string(body), `"runs":[]`) {
		t.Fatalf("Host readback: %d %s", response.StatusCode, body)
	}
	response, err = client.Post(strings.TrimSuffix(receipt.WebURL, "/loops")+"/api/control", "application/json", strings.NewReader(`{"action":"resume"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal("Host web accepted lifecycle control")
	}
}
