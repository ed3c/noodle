package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceServiceUpgradeRequiresActualConsumerCapability(t *testing.T) {
	for _, caps := range []string{`{"source_service_restore":1}`, `{"source_service_restore":1,"source_service_upgrade":1}`} {
		path := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+caps+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		ref, err := hostReadbackBinaryRef(path)
		if err != nil {
			t.Fatal(err)
		}
		p := hostMaintenancePlan{Target: ref.Path, TargetSHA: ref.SHA256, SourcePolicy: &hostSourcePolicyPlan{Operation: "operator-service-upgrade"}}
		err = verifySourceServiceMaintenanceTarget(context.Background(), p)
		if (caps == `{"source_service_restore":1,"source_service_upgrade":1}`) != (err == nil) {
			t.Fatal("upgrade target capability boundary", caps, err)
		}
	}
}

func TestSourceServiceUpgradeMarkerUsesCurrentNativeExecutableWithoutBusinessContext(t *testing.T) {
	p, _ := sourcePolicyTestPlan(t)
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	p.Contexts = nil
	p.Source.Contexts = 0
	p.SourcePolicy.Operation = "operator-service-upgrade"
	g, err := restrictedSource(p)
	if err != nil {
		t.Fatal(err)
	}
	if g.Binary != op.NativeScope.Host.Binary.Path || g.SHA != op.NativeScope.Host.Binary.SHA256 || g.ID != p.Source.Instance || g.Process.PID != p.Source.PID {
		t.Fatal("Source-only marker substituted current generation")
	}
	p.SourcePolicy.Operation = "operator-service-restore"
	if _, err = restrictedSource(p); err == nil {
		t.Fatal("ordinary restore accepted missing business contexts")
	}
}
