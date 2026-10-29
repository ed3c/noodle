package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestManagedGatewayBorrowedCustodyRejectsReplacement(t *testing.T) {
	f := newManagedFixture(t, true)
	owner := f.open(t)
	borrowed, err := owner.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	managedMarker(t, f.root, "exit")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.residents.WaitClosed(ctx); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(f.root, "child.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg managedChildConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParsePKCS8PrivateKey(cfg.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	ca := x509.NewCertPool()
	client, err := x509.ParseCertificate(cfg.ClientCA)
	if err != nil {
		t.Fatal(err)
	}
	ca.AddCert(client)
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		t.Fatal(err)
	}
	replacement, native := custodyServer(t)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: cfg.ServerCert, PrivateKey: key}},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: ca,
	})))
	pb.RegisterOpenShellServer(server, native)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Stop(); <-done }()

	name, err := borrowed.Create(ctx, OpenShellTask{"review-order", "review-attempt", "review-session"}, &pb.SandboxSpec{Command: []string{"hold"}})
	if err == nil || name != "" || !strings.Contains(err.Error(), "original") {
		t.Fatalf("retained capability reached replacement gateway: name=%q err=%v", name, err)
	}
	creates, stops, execs := replacement.counts()
	if creates != 0 || stops != 0 || execs != 0 {
		t.Fatalf("replacement effects: create=%d stop=%d exec=%d", creates, stops, execs)
	}
	if len(borrowed.Snapshot().Records) != 0 {
		t.Fatal("unowned new intent persisted")
	}
}

func TestManagedGatewayLateQuiesceAllowsFreshRelease(t *testing.T) {
	f := newManagedFixture(t, true)
	owner := f.open(t)
	late := make(chan struct{})
	var calls atomic.Int32
	guard, err := admission.NewResidentGuard("review-delayed-quiesce-delivery", 250*time.Millisecond, func(ctx context.Context) error {
		result := owner.quiesce(ctx)
		if calls.Add(1) == 1 {
			<-late
		}
		return result
	})
	if err != nil {
		t.Fatal(err)
	}
	owner.guard = guard
	if _, err := owner.Acquire(t.Context(), "a"); err != nil {
		close(late)
		t.Fatal(err)
	}
	if _, err := owner.Release("a"); err == nil {
		close(late)
		t.Fatal("expired result released gateway")
	}
	close(late)
	if _, err := owner.custody.Create(t.Context(), OpenShellTask{"sealed", "attempt", "session"}, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
		t.Fatal("late quiesce reopened mutations")
	}
	if _, err := owner.Readback(t.Context()); err == nil || !strings.Contains(err.Error(), "quiesced") {
		t.Fatalf("sealed gateway readback: %v", err)
	}
	managedMarker(t, f.root, "stall")
	readCtx, stopRead := context.WithTimeout(t.Context(), 50*time.Millisecond)
	_, readErr := owner.Readback(readCtx)
	stopRead()
	if readErr == nil || !strings.Contains(readErr.Error(), "health unavailable") {
		t.Fatalf("unavailable sealed gateway accepted: %v", readErr)
	}
	if _, err := owner.Release("a"); err == nil {
		t.Fatal("unavailable sealed gateway released")
	}
	if err := os.Remove(filepath.Join(f.root, "stall")); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Readback(t.Context()); err == nil || !strings.Contains(err.Error(), "quiesced") {
		t.Fatalf("fresh readback did not preserve sealed dispatch: %v", err)
	}
	rows, err := f.residents.Snapshot()
	if err != nil || len(rows) != 1 || rows[0].State != "running" || len(rows[0].Contexts) != 1 {
		t.Fatalf("timeout lost original lease: %+v %v", rows, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err = owner.Release("a")
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "check in progress") || time.Now().After(deadline) {
			t.Fatalf("fresh original release cannot recover late quiesce result: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.residents.WaitClosed(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("fresh release did not recheck native state: %d calls", calls.Load())
	}
}
