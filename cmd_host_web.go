package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/poteto/noodle/internal/loopview"
	"github.com/poteto/noodle/server"
)

func startHostWeb(dir string, resources func(context.Context) ([]loopview.Resource, error)) (*http.Server, string, <-chan error, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", nil, err
	}
	srv := &http.Server{Handler: server.LoopViewsHandler(dir, uiClientFS(), resources), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if err == http.ErrServerClosed {
			err = nil
		}
		done <- err
	}()
	return srv, "http://" + listener.Addr().String() + "/loops", done, nil
}

// Called only by serveHost's serialized calls queue.
func (h *noodleHost) viewResources() []loopview.Resource {
	resources := []loopview.Resource{
		{Label: "Host instance", Value: h.instanceID},
		{Label: "Attached contexts", Value: strconv.Itoa(len(h.contexts))},
	}
	ids := make([]string, 0, len(h.contexts))
	for id := range h.contexts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		receipt := h.contexts[id].receipt
		resources = append(resources, loopview.Resource{Label: "Context " + id, Value: receipt.ProjectDir + " · " + receipt.ContextState})
	}
	if h.budget != nil {
		b := h.budget.Snapshot()
		resources = append(resources,
			loopview.Resource{Label: "Host workers", Value: fmt.Sprintf("%d active / %d effective / %d maximum", b.Active, b.Limit, b.Maximum)},
			loopview.Resource{Label: "Memory pressure", Value: string(b.Pressure)},
			loopview.Resource{Label: "Admission observation", Value: b.Reason},
		)
	}
	return resources
}
