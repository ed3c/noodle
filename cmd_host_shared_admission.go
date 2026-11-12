package main

import (
	"context"
	"errors"
	"path/filepath"
)

// Accepted original requests have already become status in bindHostRequest.
// Only new effect-free OpenShell attach/once calls reach this boundary.
func bindSharedOpenShellAdmission(ctx context.Context, req *hostRequest, binding *hostRequestBinding) error {
	if !filepath.IsAbs(req.HostDir) || filepath.Clean(req.HostDir) != req.HostDir {
		return errors.New("selected OpenShell Host root is not canonical")
	}
	root, err := filepath.EvalSymlinks(req.HostDir)
	if err != nil || root != req.HostDir {
		return errors.New("selected OpenShell Host root is unavailable or changed")
	}
	instance, err := handoverSharedReadback(ctx, root, 1)
	if err != nil {
		return err
	}
	if req.RequiredHostInstanceID != "" && req.RequiredHostInstanceID != instance {
		return errors.New("required selected OpenShell Host generation changed")
	}
	if binding == nil {
		return errors.New("selected OpenShell admission has no original request binding")
	}
	if binding.RequiredHostInstanceID != "" && binding.RequiredHostInstanceID != instance {
		return errors.New("original selected OpenShell Host requirement differs")
	}
	req.RequiredHostInstanceID = instance
	binding.RequiredHostInstanceID = instance
	return saveHostRequest(req.RequestFile, *binding)
}
