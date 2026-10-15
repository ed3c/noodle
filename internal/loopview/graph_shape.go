package loopview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Required graph fields stay explicit, including nullable evidence identities.
func graphShape(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	if raw, ok := root["graph"]; ok {
		return requiredShape(raw, reflect.TypeOf(Graph{}), "graph")
	}
	return nil
}
func requiredShape(raw json.RawMessage, t reflect.Type, label string) error {
	if t == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		return requiredShape(raw, t.Elem(), label)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%s is null", label)
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			key := tag[0]
			value, ok := fields[key]
			optional := len(tag) > 1 && tag[1] == "omitempty"
			if !ok {
				if !optional {
					return fmt.Errorf("%s.%s missing", label, key)
				}
				continue
			}
			if err := requiredShape(value, field.Type, label+"."+key); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for _, item := range items {
			if err := requiredShape(item, t.Elem(), label+"[]"); err != nil {
				return err
			}
		}
	}
	return nil
}
