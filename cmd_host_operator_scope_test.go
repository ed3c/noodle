package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestOperatorScopeBindsOriginalResidentStart(t *testing.T) {
	uid := os.Getuid()
	resident := admission.Resident{PID: 42, ProcessGroup: 42, Started: "original-start"}
	row := admission.ProcessObservation{PID: 42, ProcessGroup: 42, ParentPID: 41, UID: &uid, Started: "original-start"}
	if !operatorResidentProcessMatches(row, resident, 41) {
		t.Fatal("original resident was refused")
	}
	for _, mutate := range []func(*admission.ProcessObservation){
		func(r *admission.ProcessObservation) { r.PID++ },
		func(r *admission.ProcessObservation) { r.Started = "reused-pid" },
		func(r *admission.ProcessObservation) { r.ParentPID++ },
		func(r *admission.ProcessObservation) { r.ProcessGroup++ },
		func(r *admission.ProcessObservation) { r.UID = nil },
		func(r *admission.ProcessObservation) { other := uid + 1; r.UID = &other },
	} {
		changed := row
		mutate(&changed)
		if operatorResidentProcessMatches(changed, resident, 41) {
			t.Fatal("replacement resident identity accepted")
		}
	}
}

func TestOperatorScopeUsesBinaryReaderWithoutRelaxingEvidenceBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(65 << 20); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = loopruntime.ReadSessionReadbackFile(path, nil); err == nil {
		t.Fatal("evidence size bound was relaxed")
	}
	ref, err := hostReadbackBinaryRef(path)
	if err != nil || ref.Path == "" || len(ref.SHA256) != 64 {
		t.Fatal("actual gateway-size binary cannot be bound", err)
	}
}

func TestOperatorDriverScopeKeepsExplicitNamespaceAndPeer(t *testing.T) {
	for _, args := range [][]string{
		{"/owned/driver", "--bind-socket", "/owned/state/run/compute-driver.sock", "--state-dir", "/owned/state", "--expected-peer-pid", "42", "--log-level", "info"},
		{"/owned/driver", "--bind-socket=/owned/state/run/compute-driver.sock", "--state-dir=/owned/state", "--expected-peer-pid=42", "--log-level=info"},
	} {
		before := append([]string{}, args...)
		state, socket, peer, err := operatorDriverLaunchScope(args)
		if err != nil || state != "/owned/state" || socket != "/owned/state/run/compute-driver.sock" || peer != 42 || !reflect.DeepEqual(before, args) {
			t.Fatal("explicit original driver scope changed", err)
		}
	}
}

func TestOperatorDriverScopeRefusesMissingOrOverriddenIdentity(t *testing.T) {
	valid := []string{"/owned/driver", "--bind-socket=/owned/state/run/compute-driver.sock", "--state-dir=/owned/state", "--expected-peer-pid=42"}
	for _, suffix := range [][]string{
		{"--state-dir=/other"}, {"--bind-socket=/other"}, {"--expected-peer-pid=43"},
		{"--launcher-bin", "/other/driver"}, {"--internal-run-vm"}, {"--internal-prepare-image=/other/image"}, {"--allow-same-uid-peer"},
	} {
		args := append(append([]string{}, valid...), suffix...)
		if _, _, _, err := operatorDriverLaunchScope(args); err == nil {
			t.Fatal("overridden driver identity accepted", suffix[0])
		}
	}
	for _, args := range [][]string{
		nil, valid[:3], {"/owned/driver", "--state-dir"},
		{"/owned/driver", "--state-dir=relative", "--bind-socket=/owned/socket", "--expected-peer-pid=42"},
		{"/owned/driver", "--state-dir=/owned/state", "--bind-socket=relative", "--expected-peer-pid=42"},
		{"/owned/driver", "--state-dir=/owned/state", "--bind-socket=/owned/socket", "--expected-peer-pid=1"},
	} {
		if _, _, _, err := operatorDriverLaunchScope(args); err == nil {
			t.Fatal("missing driver identity accepted")
		}
	}
}
