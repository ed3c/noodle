package workspaceartifacts

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func retainedStreamFrame(kind string, body []byte, sha256Object bool) (string, []byte) {
	h := sha1.New()
	if sha256Object {
		h = sha256.New()
	}
	fmt.Fprintf(h, "%s %d\x00", kind, len(body))
	h.Write(body)
	oid := hex.EncodeToString(h.Sum(nil))
	return oid, append(append([]byte(fmt.Sprintf("%s %s %d\n", oid, kind, len(body))), body...), '\n')
}

type retainedStreamReader struct {
	io.Reader
	maxRead int
	cancel  context.CancelFunc
}

func (r *retainedStreamReader) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	n, err := r.Reader.Read(p)
	if r.cancel != nil {
		r.cancel()
	}
	return n, err
}

func TestRetainedObjectStreamRawObjectsAndBoundedReads(t *testing.T) {
	for _, sha256Object := range []bool{false, true} {
		t.Run(fmt.Sprintf("sha256=%t", sha256Object), func(t *testing.T) {
			var data bytes.Buffer
			var oids []string
			// Larger than a pipe and the body buffer; includes arbitrary raw bytes.
			body := bytes.Repeat([]byte("raw\x00\xff\r\n"), 300000)
			for _, kind := range []string{"commit", "tree", "blob", "tag"} {
				oid, frame := retainedStreamFrame(kind, body, sha256Object)
				oids = append(oids, oid)
				data.Write(frame)
			}
			oid, frame := retainedStreamFrame("blob", nil, sha256Object)
			oids = append(oids, oid)
			data.Write(frame)
			reader := &retainedStreamReader{Reader: &data}
			if err := readRetainedObjectStream(t.Context(), reader, oids); err != nil {
				t.Fatal(err)
			}
			if reader.maxRead > 256<<10 {
				t.Fatalf("object body read exceeds fixed chunk: %d", reader.maxRead)
			}
		})
	}
}

func TestRetainedObjectStreamRefusesUntrustedFrames(t *testing.T) {
	body := []byte("original\x00\xffbytes")
	oid, frame := retainedStreamFrame("blob", body, false)
	header := fmt.Sprintf("%s blob %d\n", oid, len(body))
	for _, tc := range []struct{ name, data string }{
		{"missing", oid + " missing\n"},
		{"wrong-id", strings.Repeat("a", 40) + " blob 0\n\n"},
		{"wrong-type", strings.Replace(string(frame), " blob ", " other ", 1)},
		{"changed-type", strings.Replace(string(frame), " blob ", " tree ", 1)},
		{"negative-length", oid + " blob -1\n"},
		{"overflow-length", oid + " blob 9223372036854775808\n"},
		{"unterminated-header", strings.Repeat("a", 8192)},
		{"truncated-body", header + string(body[:3])},
		{"long-body", header + string(body) + "extra\n"},
		{"corrupt-body", header + strings.Repeat("x", len(body)) + "\n"},
		{"missing-terminator", string(frame[:len(frame)-1])},
		{"wrong-terminator", string(frame[:len(frame)-1]) + "x"},
		{"trailing-data", string(frame) + "x"},
		{"early-EOF", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := readRetainedObjectStream(t.Context(), strings.NewReader(tc.data), []string{oid}); err == nil {
				t.Fatal("unverified raw object stream accepted")
			}
		})
	}
}

func TestRetainedObjectStreamCancellationDuringBody(t *testing.T) {
	body := bytes.Repeat([]byte("original raw bytes"), 100000)
	oid, frame := retainedStreamFrame("blob", body, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &retainedStreamReader{Reader: bytes.NewReader(frame), cancel: cancel}
	if err := readRetainedObjectStream(ctx, reader, []string{oid}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation disappeared: %v", err)
	}
}

type retainedStreamCancelWriter struct{ cancel context.CancelFunc }

func (w retainedStreamCancelWriter) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestRetainedObjectStreamCommandCompletion(t *testing.T) {
	for _, mode := range []string{"valid", "nonzero-exit", "truncated", "malformed-live-child", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRetainedObjectStreamChild$")
			cmd.Env = append(os.Environ(), "NOODLE_RETAINED_STREAM_CHILD="+mode)
			if mode == "cancel" {
				cmd.Stderr = retainedStreamCancelWriter{cancel: cancel}
			}
			oid, _ := retainedStreamFrame("blob", []byte("child original\x00\xff"), false)
			// More than the previous 32-object batch, through one actual child.
			oids := make([]string, 80)
			for i := range oids {
				oids[i] = oid
			}
			err := verifyRetainedObjectCommand(ctx, cmd, oids)
			if mode == "valid" && err != nil {
				t.Fatal(err)
			}
			if mode != "valid" && err == nil {
				t.Fatal("incomplete/failed child authorized retirement")
			}
			if cmd.ProcessState == nil {
				t.Fatal("child was not reaped")
			}
			if mode == "nonzero-exit" && cmd.ProcessState.ExitCode() != 42 {
				t.Fatalf("original exit status lost: %v", cmd.ProcessState)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("command cancellation disappeared: %v", err)
			}
			if mode == "malformed-live-child" && ctx.Err() != nil {
				t.Fatal("protocol refusal waited for the command deadline")
			}
		})
	}
}

// The subprocess is this test binary, not a shell or native runtime. Its stderr
// readiness notification makes cancellation deterministic while stdout blocks.
func TestRetainedObjectStreamChild(t *testing.T) {
	mode := os.Getenv("NOODLE_RETAINED_STREAM_CHILD")
	if mode == "" {
		return
	}
	requests, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(80)
	}
	_, frame := retainedStreamFrame("blob", []byte("child original\x00\xff"), false)
	switch mode {
	case "cancel":
		fmt.Fprintln(os.Stderr, "ready")
		time.Sleep(time.Minute)
	case "malformed-live-child":
		fmt.Fprint(os.Stdout, "malformed\n")
		time.Sleep(time.Minute)
	case "truncated":
		os.Stdout.Write(frame[:len(frame)-3])
	default:
		for range strings.Fields(string(requests)) {
			os.Stdout.Write(frame)
		}
		if mode == "nonzero-exit" {
			os.Exit(42)
		}
	}
	os.Exit(0)
}
