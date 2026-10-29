package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Accepted original requests have already become status in bindHostRequest.
// Only new effect-free OpenShell attach/once calls reach this boundary.
func bindSharedOpenShellAdmission(ctx context.Context, req *hostRequest, binding *hostRequestBinding) error {
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	shared, err := filepath.EvalSymlinks(filepath.Join(cache, "noodle", "host"))
	if err != nil {
		return errors.New("existing shared OpenShell Host is unavailable; retain original request")
	}
	root, err := filepath.EvalSymlinks(req.HostDir)
	if err != nil || root != shared {
		return errors.New("new OpenShell admission requires the existing shared Host; private routing refused")
	}
	instance, err := handoverSharedReadback(ctx, shared, 1)
	if err != nil {
		return err
	}
	if req.RequiredHostInstanceID != "" && req.RequiredHostInstanceID != instance {
		return errors.New("required shared OpenShell Host generation changed")
	}
	if binding == nil {
		return errors.New("shared OpenShell admission has no original request binding")
	}
	if binding.RequiredHostInstanceID != "" && binding.RequiredHostInstanceID != instance {
		return errors.New("original shared OpenShell Host requirement differs")
	}
	req.RequiredHostInstanceID = instance
	binding.RequiredHostInstanceID = instance
	return saveHostRequest(req.RequestFile, *binding)
}
