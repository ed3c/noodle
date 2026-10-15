package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/poteto/noodle/internal/admission"
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

func TestHostViewResourceSampleObservations(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		setup                  func(*admission.Budget, time.Time)
		rate, age, sampleError string
	}{
		{name: "awaiting", rate: "unknown", age: "unknown", sampleError: "none reported"},
		{name: "baseline", setup: func(b *admission.Budget, now time.Time) {
			b.Observe(admission.Sample{At: now, Pressure: admission.Normal, Swapouts: 1 << 20}, nil)
		}, rate: "unknown", sampleError: "none reported"},
		{name: "observed zero", setup: func(b *admission.Budget, now time.Time) {
			b.Observe(admission.Sample{At: now.Add(-time.Second), Pressure: admission.Normal, Swapouts: 1 << 20}, nil)
			b.Observe(admission.Sample{At: now, Pressure: admission.Normal, Swapouts: 1 << 20}, nil)
		}, rate: "0 MiB/s", sampleError: "none reported"},
		{name: "observed rate", setup: func(b *admission.Budget, now time.Time) {
			b.Observe(admission.Sample{At: now.Add(-time.Second), Pressure: admission.Normal}, nil)
			b.Observe(admission.Sample{At: now, Pressure: admission.Normal, Swapouts: 2621440}, nil)
		}, rate: "2.5 MiB/s", sampleError: "none reported"},
		{name: "sample failure", setup: func(b *admission.Budget, now time.Time) {
			b.Observe(admission.Sample{At: now, Pressure: admission.Normal}, nil)
			b.Observe(admission.Sample{}, errors.New("vm_stat timed out"))
		}, rate: "unknown", age: "unknown", sampleError: "vm_stat timed out"},
		{name: "stale", setup: func(b *admission.Budget, now time.Time) {
			b.Observe(admission.Sample{At: now.Add(-2 * time.Minute), Pressure: admission.Normal}, nil)
		}, rate: "unknown", sampleError: "none reported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := admission.NewBudget(4)
			if tc.setup != nil {
				tc.setup(b, time.Now())
			}
			h := &noodleHost{budget: b}
			got := map[string]string{}
			for _, r := range h.viewResources() {
				got[r.Label] = r.Value
			}
			if got["System swapout rate"] != tc.rate || got["Resource sample error"] != tc.sampleError {
				t.Fatalf("resource observations: %+v", got)
			}
			age := got["Resource sample age"]
			if tc.age != "" {
				if age != tc.age {
					t.Fatalf("age = %q, want %q", age, tc.age)
				}
			} else {
				if !strings.HasSuffix(age, " s") {
					t.Fatalf("age has no seconds unit: %q", age)
				}
				value, err := strconv.ParseFloat(strings.TrimSuffix(age, " s"), 64)
				if err != nil || value < 0 || (tc.name == "stale" && value < 120) {
					t.Fatalf("age observation: %q (%v)", age, err)
				}
			}
		})
	}
}
