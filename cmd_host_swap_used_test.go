package main

import (
	"github.com/poteto/noodle/internal/admission"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHostSwapUsedLimitConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
		want  uint64
	}{
		{"0", true, 0}, {"100", true, 100}, {"9007199254740991", true, 9007199254740991},
		{"", false, 0}, {"-1", false, 0}, {"1.5", false, 0}, {"NaN", false, 0}, {"Inf", false, 0}, {"9007199254740992", false, 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(hostSwapUsedLimitEnv, tc.value)
			n, err := hostSwapUsedLimit()
			if (err == nil) != tc.ok || (tc.ok && n != tc.want) {
				t.Fatalf("value=%q limit=%d error=%v", tc.value, n, err)
			}
		})
	}
	t.Setenv(hostSwapUsedLimitEnv, "100")
	found := false
	for _, entry := range hostBaselineEnvironment(t.TempDir()) {
		if strings.HasPrefix(entry, hostSwapUsedLimitEnv+"=") {
			found = entry == hostSwapUsedLimitEnv+"=100"
		}
	}
	if !found {
		t.Fatal("launch lost policy")
	}
	if err := os.Unsetenv(hostSwapUsedLimitEnv); err != nil {
		t.Fatal(err)
	}
	if n, err := hostSwapUsedLimit(); err != nil || n != 15_000_000_000 {
		t.Fatal("unset must use 15 GB capacity default")
	}
}

func TestHostWorkerAdmissionPolicyReadback(t *testing.T) {
	budget := admission.NewBudgetWithSwapUsedLimit(4, 15_000_000_000)
	started := time.Now().Add(-3 * admission.PollInterval)
	for i := 0; i < 4; i++ {
		budget.Observe(admission.Sample{At: started.Add(time.Duration(i) * admission.PollInterval), Pressure: admission.Warning, SwapUsedBytes: 14_999_999_999}, nil)
	}
	host := &noodleHost{budget: budget}
	read := func() admission.Snapshot { return host.resources().(map[string]any)["resources"].(admission.Snapshot) }
	state := read()
	if state.AdmissionPolicy != admission.WorkerAdmissionPolicy || state.SwapUsedLimit != 15_000_000_000 || state.Pressure != admission.Warning || state.Limit < 1 {
		t.Fatalf("warning below threshold not reflected by native Host: %+v", state)
	}
	budget.Observe(admission.Sample{At: time.Now(), Pressure: admission.Warning, SwapUsedBytes: 15_000_000_000}, nil)
	state = read()
	if state.Limit != 0 || state.Reason != "swap_used_limit" || state.Active != 0 {
		t.Fatalf("reached threshold did not stop new admission: %+v", state)
	}
	if hostCapabilities("darwin")["worker_admission_policy"] != admission.WorkerAdmissionPolicy {
		t.Fatal("binary capabilities hide the worker policy version")
	}
}

func observeKnownHostFixtureSamples(budget *admission.Budget) {
	started := time.Now().Add(-3 * admission.PollInterval)
	for i := 0; i < 4; i++ {
		budget.Observe(admission.Sample{At: started.Add(time.Duration(i) * admission.PollInterval), Pressure: admission.Normal, SwapUsedBytes: 0}, nil)
	}
}
