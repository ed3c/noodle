package main

import (
	"encoding/json"
	"strings"
)

// Pool maintenance carries metadata from other binary versions without
// discarding fields the current typed consumer does not interpret.
func hostMetadataUnknown(data []byte, known ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		for _, name := range known {
			if strings.EqualFold(key, name) {
				delete(fields, key)
				break
			}
		}
	}
	return fields, nil
}

func marshalHostMetadata(value any, unknown map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil || len(unknown) == 0 {
		return data, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for key, raw := range unknown {
		if _, exists := fields[key]; !exists {
			fields[key] = raw
		}
	}
	return json.Marshal(fields)
}

func (p *hostExecutionProfile) UnmarshalJSON(data []byte) error {
	type fields hostExecutionProfile
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	unknown, err := hostMetadataUnknown(data, "kind", "threads")
	if err != nil {
		return err
	}
	*p = hostExecutionProfile(value)
	p.unknown = unknown
	return nil
}

func (p hostExecutionProfile) MarshalJSON() ([]byte, error) {
	type fields hostExecutionProfile
	return marshalHostMetadata(fields(p), p.unknown)
}

func (p *hostThreadProfileSource) UnmarshalJSON(data []byte) error {
	type fields hostThreadProfileSource
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	unknown, err := hostMetadataUnknown(data, "thread_id", "path", "metadata_sha256")
	if err != nil {
		return err
	}
	*p = hostThreadProfileSource(value)
	p.unknown = unknown
	return nil
}

func (p hostThreadProfileSource) MarshalJSON() ([]byte, error) {
	type fields hostThreadProfileSource
	return marshalHostMetadata(fields(p), p.unknown)
}

func (p *hostContextInputs) UnmarshalJSON(data []byte) error {
	type fields hostContextInputs
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	unknown, err := hostMetadataUnknown(data, "storage_root", "home", "native_home", "tools", "environment_sha256", "credential_environment", "native_auth", "native_config_sha256")
	if err != nil {
		return err
	}
	*p = hostContextInputs(value)
	p.unknown = unknown
	return nil
}

func (p hostContextInputs) MarshalJSON() ([]byte, error) {
	type fields hostContextInputs
	return marshalHostMetadata(fields(p), p.unknown)
}

func (p *hostPool) UnmarshalJSON(data []byte) error {
	type fields hostPool
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	unknown, err := hostMetadataUnknown(data, "schema", "active", "generations", "owners", "queue")
	if err != nil {
		return err
	}
	*p = hostPool(value)
	p.unknown = unknown
	return nil
}

func (p hostPool) MarshalJSON() ([]byte, error) {
	type fields hostPool
	return marshalHostMetadata(fields(p), p.unknown)
}

func (p *hostGeneration) UnmarshalJSON(data []byte) error {
	type fields hostGeneration
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	unknown, err := hostMetadataUnknown(data, "source_service", "id", "directory", "binary_path", "binary_sha256", "state", "process", "error")
	if err != nil {
		return err
	}
	*p = hostGeneration(value)
	p.unknown = unknown
	return nil
}

func (p hostGeneration) MarshalJSON() ([]byte, error) {
	type fields hostGeneration
	return marshalHostMetadata(fields(p), p.unknown)
}

func (p *hostTaskOwner) UnmarshalJSON(data []byte) error {
	type fields hostTaskOwner
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	unknown, err := hostMetadataUnknown(data, "not_admitted", "maintenance_hold", "execution_profile", "controller_only", "context_inputs", "rebuild_error", "last_admission", "round_state", "admission_only", "original", "current", "state", "environment_sha256", "runtime_environment_sha256", "previous")
	if err != nil {
		return err
	}
	*p = hostTaskOwner(value)
	p.unknown = unknown
	return nil
}

func (p hostTaskOwner) MarshalJSON() ([]byte, error) {
	type fields hostTaskOwner
	return marshalHostMetadata(fields(p), p.unknown)
}

func (p *hostVersionRequest) UnmarshalJSON(data []byte) error {
	type fields hostVersionRequest
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	unknown, err := hostMetadataUnknown(data, "maintenance_previous_state", "id", "binary_path", "binary_sha256", "state", "generation", "error")
	if err != nil {
		return err
	}
	*p = hostVersionRequest(value)
	p.unknown = unknown
	return nil
}

func (p hostVersionRequest) MarshalJSON() ([]byte, error) {
	type fields hostVersionRequest
	return marshalHostMetadata(fields(p), p.unknown)
}
