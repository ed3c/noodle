package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/loop"
)

func TestHostOpenShellProfileRefusalPrecedesRuntimeOrContext(t *testing.T) {
	root := t.TempDir()
	req := hostTestRequest(t, root)
	data := []byte("mode='supervised'\n[server]\nenabled=false\n[runtime.openshell]\nprofile='missing-profile.json'\n")
	if err := os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	req.ConfigSHA256 = hostDigest(data)
	h := &noodleHost{dir: t.TempDir(), instanceID: "instance", binaryPath: "/native/noodle", binarySHA: "test-binary", budget: admission.NewBudget(2)}
	var err error
	h.residents, err = admission.OpenResidents(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime { called = true; return nil }
	response := h.attach(context.Background(), req)
	if response.Status != "refused" || called || len(h.contexts) != 0 || len(h.receipts) != 0 || h.openShell != nil {
		t.Fatalf("unsafe attach: %+v called=%t", response, called)
	}
	records, err := h.residents.Snapshot()
	if err != nil || len(records) != 0 {
		t.Fatalf("resident spawned on refusal: %v %v", records, err)
	}
}
