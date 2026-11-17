package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	loopruntime "github.com/poteto/noodle/runtime"
)

func readGoalHostTemplate(template string) (loopruntime.SessionReadbackFileRef, loopruntime.OpenShellProfile, error) {
	var loadedTemplate loopruntime.OpenShellProfile
	if template == "" {
		path, e := goalHostTemplatePath()
		if e != nil {
			return loopruntime.SessionReadbackFileRef{}, loadedTemplate, e
		}
		var installed goalHostTemplateRegistration
		raw, e := os.ReadFile(path)
		if e != nil {
			return loopruntime.SessionReadbackFileRef{}, loadedTemplate, fmt.Errorf("installed Host profile template is unavailable: %w", e)
		}
		if e = json.Unmarshal(raw, &installed); e != nil {
			return loopruntime.SessionReadbackFileRef{}, loadedTemplate, e
		}
		actual, e := loopruntime.ReadSessionReadbackFile(installed.Profile.Path, nil)
		if e != nil || actual != installed.Profile {
			return loopruntime.SessionReadbackFileRef{}, loadedTemplate, errors.New("installed Host profile template changed")
		}
		loaded, e := loopruntime.LoadOpenShellProfile(installed.Profile.Path)
		if e != nil {
			return loopruntime.SessionReadbackFileRef{}, loadedTemplate, e
		}
		if installed.Schema != 1 || loaded.SourceSHA256 != installed.PolicySHA256 {
			return loopruntime.SessionReadbackFileRef{}, loadedTemplate, errors.New("installed Host profile template dependencies changed")
		}
		loadedTemplate = loaded
		template = installed.Profile.Path
	}
	templateRef, e := loopruntime.ReadSessionReadbackFile(template, nil)
	if e != nil {
		return loopruntime.SessionReadbackFileRef{}, loadedTemplate, e
	}
	if loadedTemplate.SourcePath == "" {
		loadedTemplate, e = loopruntime.LoadOpenShellProfile(templateRef.Path)
		if e != nil {
			return loopruntime.SessionReadbackFileRef{}, loadedTemplate, e
		}
	}
	return templateRef, loadedTemplate, nil
}
