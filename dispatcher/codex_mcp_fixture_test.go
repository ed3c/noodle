//go:build !windows

package dispatcher

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestNativeMCPFixture(t *testing.T) {
	path := os.Getenv("NOODLE_TEST_MCP_IDENTITY")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(20)
	}
	if _, err := fmt.Fprintln(file, os.Getpid()); err != nil {
		os.Exit(21)
	}
	if err := file.Close(); err != nil {
		os.Exit(22)
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var request struct {
			ID     json.RawMessage
			Method string
			Params struct{ ProtocolVersion string }
		}
		if decoder.Decode(&request) != nil {
			os.Exit(0)
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": request.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "offline-native-fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		case "resources/templates/list":
			result = map[string]any{"resourceTemplates": []any{}}
		case "prompts/list":
			result = map[string]any{"prompts": []any{}}
		case "ping":
			result = map[string]any{}
		default:
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": request.Method}})
			continue
		}
		if encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}) != nil {
			os.Exit(23)
		}
	}
}
