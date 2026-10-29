package main

import (
	"context"
	"path/filepath"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
)

func TestRollingVersionRemainsQueuedWhileGatewayContextIsLive(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = id
		p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), State: "active"}
		p.Queue = []hostVersionRequest{{ID: "version", Binary: "/never-launch-before-drain", State: "queued"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{dir: filepath.Join(root, "g", id), instanceID: id, rolling: &hostRolling{root: root, carry: map[string]rollingCarry{}}, openShell: &loopruntime.OpenShellGateway{}, contexts: map[string]*hostContext{"task": {openShell: true, done: make(chan error)}}}
	if h.advanceRolling(context.Background()) {
		t.Fatal("live gateway caused generation retirement")
	}
	p, err := readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Active != id || len(p.Generations) != 1 || p.Queue[0].State != "queued" || h.rolling.retiring || h.contexts["task"].done == nil {
		t.Fatal("live task, version queue or original owner changed")
	}
}
