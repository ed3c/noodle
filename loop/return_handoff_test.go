package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/state"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReturnHandoffProducerGuards(t *testing.T) {
	dir := t.TempDir()
	save := func(name string, v any) config.RuntimeSource {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return config.RuntimeSource{Path: path, SHA256: hex.EncodeToString(sum[:])}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	producer := config.RuntimeSource{Path: exe, SHA256: hex.EncodeToString(sum[:])}
	binding := map[string]any{"schema": 1, "request": config.RuntimeSource{Path: "/fixture/request", SHA256: strings.Repeat("b", 64)},
		"goal": "fixture", "main": map[string]string{"threadId": "Main", "hostId": "local"}, "connection_instance": "original", "consumer_revision": "v1", "producer": producer, "host_instance_id": "host"}
	pin := save("binding.json", binding)
	target := returnDescriptor{Schema: 1, Binding: pin, Endpoint: filepath.Join(dir, "never-opened.sock"), Producer: producer, HostInstanceID: "host"}
	l := &Loop{config: config.DefaultConfig(), deps: Dependencies{ReturnHostInstance: "host"}}
	cook := &cookHandle{cookIdentity: cookIdentity{orderID: "o", stageIndex: 0}}
	if got, err := l.prepareReturnHandoff(cook); err != nil || got != nil {
		t.Fatal("legacy default", err)
	}
	l.config.Runtime.AdmittedOrders = []string{"o"}
	l.config.Runtime.ReturnHandoff = save("descriptor.json", target)
	if got, err := l.prepareReturnHandoff(cook); err != nil || got == nil {
		t.Fatal("valid descriptor refused", err)
	}
	for _, field := range []string{"host", "order", "stage", "digest"} {
		original := l.config.Runtime.ReturnHandoff
		switch field {
		case "host":
			l.deps.ReturnHostInstance = "foreign"
		case "order":
			cook.orderID = "foreign"
		case "stage":
			cook.stageIndex = 1
		case "digest":
			l.config.Runtime.ReturnHandoff.SHA256 = strings.Repeat("0", 64)
		}
		if _, err := l.prepareReturnHandoff(cook); err == nil {
			t.Fatal("foreign guard missing", field)
		}
		l.deps.ReturnHostInstance = "host"
		cook.orderID = "o"
		cook.stageIndex = 0
		l.config.Runtime.ReturnHandoff = original
	}
	l.canonical.PendingReviews = map[string]state.PendingReviewNode{"o": {ReturnHandoff: &state.ReturnHandoffIntent{Status: "unknown"}}}
	if _, err := l.prepareReturnHandoff(cook); err == nil {
		t.Fatal("unknown intent replay")
	}
}
