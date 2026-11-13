package procx

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeLaunchSeparatesArgumentsFromEnvironment(t *testing.T) {
	data := binary.NativeEndian.AppendUint32(nil, 4)
	data = append(data, []byte("/owned/driver\x00\x00\x00/owned/driver\x00--state-dir\x00/owned/state\x00\x00PRIVATE_KEY=never-return-this\x00")...)
	launch, err := parseNativeLaunch(data, "/owned/driver", 8)
	if err != nil || len(launch.Arguments) != 4 || launch.Arguments[2] != "/owned/state" || launch.Arguments[3] != "" {
		t.Fatal("kernel argument boundary lost", err)
	}
	for _, arg := range launch.Arguments {
		if strings.Contains(arg, "PRIVATE_KEY") {
			t.Fatal("environment suffix escaped into argv")
		}
	}
	encoded, err := json.Marshal(launch)
	if err != nil || strings.Contains(string(encoded), "owned/state") || strings.Contains(string(encoded), "Arguments") {
		t.Fatal("raw argv was published", err)
	}
}

func TestNativeLaunchRefusesMalformedOrAmbiguousKernelInput(t *testing.T) {
	for _, value := range []struct {
		argc uint32
		data string
	}{
		{0, "/owned/driver\x00"},
		{1000, "/owned/driver\x00"},
		{1, "/different/driver\x00/owned/driver\x00"},
		{1, "/owned/driver\x00/owned/driver"},
		{1, "/owned/driver\x00\x00PRIVATE_KEY=not-argv\x00"},
	} {
		data := binary.NativeEndian.AppendUint32(nil, value.argc)
		data = append(data, []byte(value.data)...)
		if _, err := parseNativeLaunch(data, "/owned/driver", 8); err == nil {
			t.Fatal("unqualified kernel buffer accepted")
		}
	}
}

func TestNativeLaunchKeepsEmptyArgumentZeroSeparateFromEnvironment(t *testing.T) {
	for _, wordSize := range []int{4, 8} {
		for _, environment := range []string{"--state-dir=/env-only", "/PRIVATE_KEY=never-return", "PRIVATE KEY=never-return", "opaque-environment-entry"} {
			data := binary.NativeEndian.AppendUint32(nil, 2)
			prefix := []byte("/owned/bin\x00")
			for len(prefix)%wordSize != 0 {
				prefix = append(prefix, 0)
			}
			data = append(data, prefix...)
			// Real argv is ["", "/owned/bin"], followed by envp.
			data = append(data, []byte("\x00/owned/bin\x00"+environment+"\x00")...)
			if launch, err := parseNativeLaunch(data, "/owned/bin", wordSize); err == nil || len(launch.Arguments) != 0 {
				t.Fatal("empty argument zero exposed environment suffix")
			}
		}
	}
}

func TestNativeLaunchUsesTheTargetWordSize(t *testing.T) {
	for _, wordSize := range []int{4, 8} {
		data := binary.NativeEndian.AppendUint32(nil, 2)
		prefix := []byte("/owned/bin\x00") // 11 bytes: 4 aligns to 12, 8 to 16.
		for len(prefix)%wordSize != 0 {
			prefix = append(prefix, 0)
		}
		data = append(data, prefix...)
		data = append(data, []byte("/owned/bin\x00--flag=value\x00PRIVATE_KEY=never-return\x00")...)
		launch, err := parseNativeLaunch(data, "/owned/bin", wordSize)
		if err != nil || len(launch.Arguments) != 2 || launch.Arguments[1] != "--flag=value" {
			t.Fatal("target alignment lost argument boundary", wordSize, err)
		}
		other := 12 - wordSize
		if _, err = parseNativeLaunch(data, "/owned/bin", other); err == nil {
			t.Fatal("launch accepted the other target word size", wordSize)
		}
	}
}
