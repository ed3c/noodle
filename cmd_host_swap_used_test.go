package main

import (
	"errors"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
)

func TestHostWorkerCeilingDoesNotDependOnPressureObservation(t *testing.T) {
	for _, condition := range []string{"critical", "failed", "stale", "unsupported"} {
		t.Run(condition, func(t *testing.T) {
			budget := admission.NewBudget(0)
			budget.Register("context")
			sample := admission.Sample{At: time.Now(), Pressure: admission.Critical, SwapUsedBytes: 99_000_000_000}
			var err error
			switch condition {
			case "failed":
				err = errors.New("monitor unavailable")
			case "stale":
				sample.At = time.Now().Add(-time.Hour)
			case "unsupported":
				sample.Unsupported = true
			}
			budget.Observe(sample, err)
			host := &noodleHost{budget: budget}
			state := host.resources().(map[string]any)["resources"].(admission.Snapshot)
			if state.AdmissionPolicy != admission.WorkerAdmissionPolicy || state.Maximum != admission.DefaultWorkerLimit || state.Limit != admission.DefaultWorkerLimit {
				t.Fatalf("Host ceiling changed with observation: %+v", state)
			}
			var permits []*admission.Permit
			for range admission.DefaultWorkerLimit {
				permit, err := budget.TryAcquire("context")
				if err != nil {
					t.Fatal(err)
				}
				permits = append(permits, permit)
			}
			if _, err := budget.TryAcquire("context"); err == nil {
				t.Fatal("tenth worker admitted")
			}
			for _, permit := range permits {
				permit.Release()
			}
		})
	}
	if hostCapabilities("darwin")["worker_admission_policy"] != admission.WorkerAdmissionPolicy {
		t.Fatal("capability policy differs")
	}
}

func observeKnownHostFixtureSamples(budget *admission.Budget) {
	budget.Observe(admission.Sample{At: time.Now(), Pressure: admission.Normal}, nil)
}
