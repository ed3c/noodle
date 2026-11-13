package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
)

func TestGatewayHandoverProfileNeedsGatewayIdentityNotWorkerPayload(t *testing.T) {
	profile := hostDeferralProfile(t)
	full, err := loopruntime.LoadOpenShellProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	profileRef, err := loopruntime.ReadSessionReadbackFile(profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(profile)
	configPath := filepath.Join(root, "noodle.toml")
	config := []byte(fmt.Sprintf("[runtime.openshell]\nprofile=%q\n", profile))
	if err = os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(root, "payload")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(root, "spec.json")); err != nil {
		t.Fatal(err)
	}
	plan := hostMaintenancePlan{Handover: &hostGatewayHandoverPlan{}}
	if err = bindGatewayHandoverProfile(&plan, configPath, hostDigest(config)); err != nil {
		t.Fatal(err)
	}
	if plan.Handover.Binding != full.Gateway.Binding || plan.Handover.Profile != profileRef {
		t.Fatal("handover lost original gateway deployment or profile identity")
	}
	if _, err = loopruntime.LoadOpenShellProfile(profile); err == nil {
		t.Fatal("maintenance bypassed worker payload validation at admission")
	}
	if err = os.WriteFile(filepath.Join(root, "address"), []byte("changed deployment"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = bindGatewayHandoverProfile(&plan, configPath, hostDigest(config)); err == nil {
		t.Fatal("handover accepted different gateway configuration")
	}
	if err = bindGatewayHandoverProfile(&plan, configPath, hostDigest([]byte("different config"))); err == nil {
		t.Fatal("handover accepted different original context configuration")
	}
}
