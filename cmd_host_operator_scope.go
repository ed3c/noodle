package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

type hostOperatorActor struct {
	Process procx.NativeIdentity               `json:"process"`
	Binary  loopruntime.SessionReadbackFileRef `json:"binary"`
}

// This readback selects the original deployment actors. It neither pauses them
// nor certifies RPC drain, an empty namespace, or permission to retire them.
type hostOperatorNativeScope struct {
	Schema         int                                `json:"schema"`
	GatewayConfig  loopruntime.SessionReadbackFileRef `json:"gateway_config"`
	StateDirectory string                             `json:"state_directory"`
	Socket         string                             `json:"socket"`
	Host           hostOperatorActor                  `json:"host"`
	Gateway        hostOperatorActor                  `json:"gateway"`
	Driver         hostOperatorActor                  `json:"driver"`
}

func observeHostOperatorNativeScope(ctx context.Context, plan hostOperatorPlan) (*hostOperatorNativeScope, error) {
	var source struct {
		PID       int    `json:"host_pid"`
		Binary    string `json:"binary_path"`
		BinarySHA string `json:"binary_sha256"`
	}
	if err := json.Unmarshal(plan.Source, &source); err != nil {
		return nil, err
	}
	spec, err := loopruntime.LoadOpenShellGatewaySpec(plan.Profile.Path)
	if err != nil {
		return nil, err
	}
	if spec.Binding != plan.Binding || spec.ComputeDriver != "vm" {
		return nil, errors.New("original Source VM deployment binding differs")
	}
	scope := &hostOperatorNativeScope{Schema: 1}
	scope.GatewayConfig, err = loopruntime.ReadSessionReadbackFile(spec.Binding.ConfigSource, nil)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(scope.GatewayConfig.Path)
	if err != nil || hostDigest(data) != scope.GatewayConfig.SHA256 {
		return nil, errors.New("native gateway configuration changed during scope readback")
	}
	var config struct {
		OpenShell struct {
			Gateway struct {
				ComputeDriver string `toml:"compute_driver"`
			} `toml:"gateway"`
			Drivers struct {
				VM struct {
					DriverDir string `toml:"driver_dir"`
					StateDir  string `toml:"state_dir"`
				} `toml:"vm"`
			} `toml:"drivers"`
		} `toml:"openshell"`
	}
	if _, err = toml.Decode(string(data), &config); err != nil || config.OpenShell.Gateway.ComputeDriver != "vm" {
		return nil, errors.New("original Source native VM configuration unavailable")
	}
	canonical := func(path string) (string, error) {
		if !filepath.IsAbs(path) {
			return "", errors.New("native VM deployment path is not explicitly absolute")
		}
		return filepath.EvalSymlinks(path)
	}
	scope.StateDirectory, err = canonical(config.OpenShell.Drivers.VM.StateDir)
	if err != nil {
		return nil, err
	}
	driverDir, err := canonical(config.OpenShell.Drivers.VM.DriverDir)
	if err != nil {
		return nil, err
	}
	driverBinary, err := hostReadbackBinaryRef(filepath.Join(driverDir, "openshell-driver-vm"))
	if err != nil {
		return nil, err
	}
	observe := func(pid int, expected string) (hostOperatorActor, procx.NativeLaunch, error) {
		identity, err := procx.ObserveNativeIdentity(pid)
		if err != nil {
			return hostOperatorActor{}, procx.NativeLaunch{}, err
		}
		if identity.UID != os.Getuid() || identity.Zombie {
			return hostOperatorActor{}, procx.NativeLaunch{}, errors.New("original native actor UID or lifetime differs")
		}
		launch, err := procx.ObserveNativeLaunch(identity)
		if err != nil {
			return hostOperatorActor{}, procx.NativeLaunch{}, err
		}
		binary, err := hostReadbackBinaryRef(launch.Executable)
		if err != nil {
			return hostOperatorActor{}, procx.NativeLaunch{}, err
		}
		expectedRef, err := hostReadbackBinaryRef(expected)
		if err != nil || binary != expectedRef {
			return hostOperatorActor{}, procx.NativeLaunch{}, errors.New("original native actor executable differs")
		}
		return hostOperatorActor{Process: identity, Binary: binary}, launch, nil
	}
	scope.Host, _, err = observe(source.PID, source.Binary)
	if err != nil {
		return nil, err
	}
	if scope.Host.Binary.SHA256 != source.BinarySHA {
		return nil, errors.New("original native Host executable digest differs")
	}
	scope.Gateway, _, err = observe(plan.Resident.PID, spec.Resident.Argv[0])
	if err != nil {
		return nil, err
	}
	if scope.Gateway.Process.ParentPID != scope.Host.Process.PID || scope.Gateway.Process.Group != plan.Resident.ProcessGroup {
		return nil, errors.New("original Source gateway ancestry differs")
	}
	rows, err := admission.ObserveVisibleProcesses(ctx)
	if err != nil {
		return nil, err
	}
	residentObserved := false
	for _, row := range rows {
		if row.PID == plan.Resident.PID {
			if !operatorResidentProcessMatches(row, plan.Resident, scope.Host.Process.PID) {
				return nil, errors.New("original Source resident start or ancestry differs")
			}
			residentObserved = true
		}
	}
	if !residentObserved {
		return nil, errors.New("original Source resident process metadata unavailable")
	}
	for _, row := range rows {
		if row.ParentPID != scope.Gateway.Process.PID {
			continue
		}
		actor, launch, err := observe(row.PID, driverBinary.Path)
		if err != nil {
			return nil, err
		}
		if scope.Driver.Process.PID != 0 || actor.Process.ParentPID != scope.Gateway.Process.PID {
			return nil, errors.New("original Source has another or changed gateway child")
		}
		state, socket, peer, err := operatorDriverLaunchScope(launch.Arguments)
		if err != nil {
			return nil, err
		}
		state, err = canonical(state)
		if err != nil || state != scope.StateDirectory || peer != scope.Gateway.Process.PID {
			return nil, errors.New("original VM driver namespace or expected gateway peer differs")
		}
		socket, err = canonical(socket)
		if err != nil || socket != filepath.Join(scope.StateDirectory, "run", "compute-driver.sock") {
			return nil, errors.New("original VM driver socket is outside its declared namespace")
		}
		scope.Socket, scope.Driver = socket, actor
	}
	if scope.Driver.Process.PID == 0 {
		return nil, errors.New("original Source VM driver child not observed")
	}
	for _, actor := range []hostOperatorActor{scope.Host, scope.Gateway, scope.Driver} {
		current, err := procx.ObserveNativeIdentity(actor.Process.PID)
		if err != nil || !actor.Process.SameGeneration(current) || current.Zombie || current.ParentPID != actor.Process.ParentPID {
			return nil, errors.New("original Source actor generation or ancestry changed during readback")
		}
	}
	for _, ref := range []loopruntime.SessionReadbackFileRef{scope.GatewayConfig, scope.Host.Binary, scope.Gateway.Binary, scope.Driver.Binary} {
		current, err := hostReadbackBinaryRef(ref.Path)
		if err != nil || current != ref {
			return nil, errors.New("original Source deployment changed during native scope readback")
		}
	}
	// Reuse the original reader to detect changes in Host/resident custody while
	// collecting kernel metadata; a newly observed PID is not the original lease.
	fresh, err := planHostOperatorMaintenance(ctx, plan.Request.Path)
	if err != nil {
		return nil, err
	}
	var currentSource struct {
		PID int `json:"host_pid"`
	}
	if err = json.Unmarshal(fresh.Source, &currentSource); err != nil {
		return nil, err
	}
	if currentSource.PID != source.PID || fresh.Request != plan.Request || fresh.Config != plan.Config || fresh.Profile != plan.Profile || fresh.History != plan.History || fresh.Pool != plan.Pool || fresh.Custody != plan.Custody || !reflect.DeepEqual(fresh.Contexts, plan.Contexts) || !reflect.DeepEqual(fresh.Resident, plan.Resident) {
		return nil, errors.New("original Source or resident custody changed during native scope readback")
	}
	return scope, nil
}

func operatorResidentProcessMatches(row admission.ProcessObservation, resident admission.Resident, hostPID int) bool {
	return row.PID == resident.PID && row.Started != "" && row.Started == resident.Started && row.ParentPID == hostPID && row.ProcessGroup == resident.ProcessGroup && row.UID != nil && *row.UID == os.Getuid()
}

func operatorDriverLaunchScope(args []string) (string, string, int, error) {
	values := map[string]string{}
	for i := 1; i < len(args); i++ {
		key, value, assigned := strings.Cut(args[i], "=")
		switch key {
		case "--internal-run-vm", "--internal-prepare-image", "--launcher-bin", "--allow-same-uid-peer":
			return "", "", 0, errors.New("native VM driver launch mode or peer policy is not qualified")
		case "--bind-socket", "--state-dir", "--expected-peer-pid":
			if _, duplicate := values[key]; duplicate {
				return "", "", 0, errors.New("native VM driver scope flag duplicated")
			}
			if !assigned {
				i++
				if i == len(args) {
					return "", "", 0, errors.New("native VM driver scope value missing")
				}
				value = args[i]
			}
			values[key] = value
		}
	}
	peer, err := strconv.Atoi(values["--expected-peer-pid"])
	if err != nil || peer <= 1 || !filepath.IsAbs(values["--state-dir"]) || !filepath.IsAbs(values["--bind-socket"]) {
		return "", "", 0, errors.New("native VM driver namespace or gateway peer is incomplete")
	}
	return values["--state-dir"], values["--bind-socket"], peer, nil
}
