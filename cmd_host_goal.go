package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type goalHostSelection struct {
	Schema    int                                 `json:"schema"`
	Goal      loopruntime.SessionReadbackFileRef  `json:"goal"`
	Operation *loopruntime.SessionReadbackFileRef `json:"operation,omitempty"`
	Root      string                              `json:"host_root"`
	Template  loopruntime.SessionReadbackFileRef  `json:"template"`
	Profile   loopruntime.SessionReadbackFileRef  `json:"profile"`
	Noodle    loopruntime.SessionReadbackFileRef  `json:"noodle"`
	Phase     string                              `json:"phase"`
	Previous  []goalHostSelection                 `json:"previous,omitempty"`
	Fault     string                              `json:"replacement_reason,omitempty"`
}

type goalHostResult struct {
	Schema           int                                `json:"schema"`
	Status           string                             `json:"status"`
	Goal             loopruntime.SessionReadbackFileRef `json:"goal"`
	HostRoot         string                             `json:"host_root"`
	HostInstanceID   string                             `json:"host_instance_id"`
	Profile          loopruntime.SessionReadbackFileRef `json:"profile"`
	Noodle           loopruntime.SessionReadbackFileRef `json:"noodle"`
	Policy           string                             `json:"policy"`
	RuntimeReadiness string                             `json:"runtime_readiness"`
	Next             map[string]any                     `json:"next"`
	Error            string                             `json:"error,omitempty"`
}

type goalHostTemplateRegistration struct {
	Schema       int                                `json:"schema"`
	Profile      loopruntime.SessionReadbackFileRef `json:"profile"`
	PolicySHA256 string                             `json:"policy_sha256"`
}

var errGoalHostProfileReadback = errors.New("original Goal Host profile readback unavailable")

func newHostGoalEnsureCmd() *cobra.Command {
	var goal, profile, request string
	var fresh bool
	cmd := &cobra.Command{Use: "ensure", Short: "Prepare or read the isolated Host for one original Goal", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if fresh && request == "" {
			return errors.New("explicit new Host has no original operation reference")
		}
		if !fresh && request != "" {
			return errors.New("Host operation reference has no explicit new-Host selection")
		}
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		binary, err = filepath.EvalSymlinks(binary)
		if err != nil {
			return err
		}
		result, err := ensureGoalHost(cmd.Context(), filepath.Join(cache, "noodle"), goal, profile, binary, request, ensureRollingHost)
		if encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); encodeErr != nil {
			return encodeErr
		}
		return err
	}}
	cmd.Flags().StringVar(&goal, "goal-file", "", "Original immutable Goal source record")
	cmd.Flags().StringVar(&profile, "profile", "", "Explicit installed OpenShell profile template override")
	cmd.Flags().BoolVar(&fresh, "new-host", false, "Select a new Host for a fresh, original operation")
	cmd.Flags().StringVar(&request, "request-file", "", "Original operation source for idempotent explicit Host creation")
	return cmd
}

func goalHostTemplatePath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "noodle", "host-template.json"), nil
}

func newHostGoalTemplateCmd() *cobra.Command {
	var profile string
	cmd := &cobra.Command{Use: "template", Short: "Register the installed OpenShell template once for normal Goal dispatch", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := goalHostTemplatePath()
		if err != nil {
			return err
		}
		ref, err := registerGoalHostTemplate(cmd.Context(), path, profile)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema": 1, "status": "registered", "profile": ref})
	}}
	cmd.Flags().StringVar(&profile, "profile", "", "Installed original OpenShell template source")
	return cmd
}

func registerGoalHostTemplate(ctx context.Context, path, profile string) (loopruntime.SessionReadbackFileRef, error) {
	var empty loopruntime.SessionReadbackFileRef
	if !filepath.IsAbs(profile) {
		return empty, errors.New("Host profile template is not absolute")
	}
	ref, err := loopruntime.ReadSessionReadbackFile(profile, nil)
	if err != nil {
		return empty, err
	}
	loaded, err := loopruntime.LoadOpenShellProfile(ref.Path)
	if err != nil {
		return empty, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return empty, err
	}
	lock, err := lockfile.Acquire(ctx, path+".lock")
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	raw, err := json.Marshal(goalHostTemplateRegistration{Schema: 1, Profile: ref, PolicySHA256: loaded.SourceSHA256})
	if err != nil {
		return empty, err
	}
	return ref, filex.WriteFileAtomicDurable(path, raw)
}

func ensureGoalHost(ctx context.Context, root, goalFile, template, binary, newOperation string, launch func(context.Context, string, string) error) (goalHostResult, error) {
	result := goalHostResult{Schema: 1, Status: "refused", Policy: admission.WorkerAdmissionPolicy, RuntimeReadiness: "not_observed"}
	fail := func(err error) (goalHostResult, error) { result.Error = err.Error(); return result, err }
	goal, err := loopruntime.ReadSessionReadbackFile(goalFile, nil)
	if err != nil {
		return fail(err)
	}
	if info, statErr := os.Stat(goal.Path); statErr != nil || info.Size() == 0 || info.Size() > 4<<20 {
		return fail(errors.New("original Goal source is empty or exceeds its bounded input"))
	}
	result.Goal = goal
	if !filepath.IsAbs(goal.Path) {
		return fail(errors.New("original Goal source is not absolute"))
	}
	var operation *loopruntime.SessionReadbackFileRef
	key := hostDigest([]byte(goal.Path + "\x00" + goal.SHA256))
	if newOperation != "" {
		ref, e := loopruntime.ReadSessionReadbackFile(newOperation, nil)
		if e != nil {
			return fail(e)
		}
		if !filepath.IsAbs(ref.Path) {
			return fail(errors.New("original Host operation source is not absolute"))
		}
		operation = &ref
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fail(errors.New("native Host owner root is not absolute"))
	}
	selections := filepath.Join(root, "goals")
	if err = os.MkdirAll(selections, 0700); err != nil {
		return fail(err)
	}
	if canonical, e := filepath.EvalSymlinks(selections); e != nil || canonical != selections {
		return fail(errors.New("native Goal Host custody path contains a symlink"))
	}
	selectionFile := filepath.Join(selections, key+".json")
	lock, err := lockfile.Acquire(ctx, selectionFile+".lock")
	if err != nil {
		return fail(err)
	}
	defer lock.Close()
	var selected goalHostSelection
	historical := false
	raw, err := os.ReadFile(selectionFile)
	exists := err == nil
	if exists {
		if err = json.Unmarshal(raw, &selected); err != nil {
			return fail(err)
		}
		if selected.Schema != 1 || selected.Goal != goal {
			return fail(errors.New("original Goal Host selection binding changed"))
		}
		if operation != nil && !sameGoalOperation(selected.Operation, operation) {
			for _, previous := range selected.Previous {
				if sameGoalOperation(previous.Operation, operation) {
					selected, historical = previous, true
					break
				}
			}
			if !historical {
				if selected.Phase != "ready" {
					return fail(errors.New("original Host creation is unresolved; new selection refused"))
				}
				previous := selected
				previous.Previous = nil
				selected = goalHostSelection{Previous: append(selected.Previous, previous)}
				exists = false
			}
		}
		if exists && template != "" {
			override, e := loopruntime.ReadSessionReadbackFile(template, nil)
			if e != nil {
				return fail(e)
			}
			if override != selected.Template {
				return fail(errors.New("explicit profile differs from the original Goal Host template"))
			}
		}
		result.HostRoot, result.Profile, result.Noodle = selected.Root, selected.Profile, selected.Noodle
		if exists && selected.Phase != "materializing" && selected.Phase != "prepared" && selected.Phase != "starting" && selected.Phase != "ready" {
			return fail(errors.New("original Goal Host preparation is incomplete; inspect its retained intent"))
		}
		if selected.Phase == "ready" || selected.Phase == "starting" {
			observation, readErr := goalHostResources(ctx, selected)
			if readErr != nil {
				if errors.Is(readErr, errGoalHostProfileReadback) || selected.Phase != "ready" || historical || !goalHostProcessAbsent(selected.Root) {
					result.Status = "unknown"
					result.Next = goalHostNext(selected.Noodle.Path, goalFile, newOperation)
					return fail(fmt.Errorf("original Goal Host readback unavailable; no creation replay: %w", readErr))
				}
			}
			if readErr != nil || observation.Admission.Status == "blocked" && observation.Admission.CanAccept != nil && !*observation.Admission.CanAccept {
				if selected.Phase != "ready" || historical {
					return fail(errors.New("original Host creation is unresolved or superseded; replacement refused"))
				}
				previous := selected
				previous.Previous = nil
				previous.Fault = observation.Admission.Error
				if previous.Fault == "" {
					previous.Fault = "original Host admission blocked"
				}
				if readErr != nil {
					previous.Fault = fmt.Sprintf("original Host process and group absent; readback: %v", readErr)
				}
				selected = goalHostSelection{Previous: append(selected.Previous, previous)}
				exists = false
			} else {
				if observation.Admission.CanAccept == nil {
					result.Status = "unknown"
					return fail(errors.New("original Host does not expose admission readback"))
				}
				if !*observation.Admission.CanAccept {
					result.Status = "waiting"
					result.HostInstanceID = observation.Instance
					result.Next = goalHostNext(selected.Noodle.Path, goalFile, newOperation)
					return result, nil
				}
				if selected.Phase != "ready" {
					selected.Phase = "ready"
					if err = saveGoalHostSelection(selectionFile, selected); err != nil {
						return fail(err)
					}
				}
				result.Status = "ready"
				result.HostInstanceID = observation.Instance
				result.Noodle = observation.Binary
				result.Next = goalHostNext(selected.Noodle.Path, goalFile, newOperation)
				return result, nil
			}
		}
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	if !exists {
		if template == "" {
			path, e := goalHostTemplatePath()
			if e != nil {
				return fail(e)
			}
			var installed goalHostTemplateRegistration
			raw, e := os.ReadFile(path)
			if e != nil {
				return fail(fmt.Errorf("installed Host profile template is unavailable: %w", e))
			}
			if e = json.Unmarshal(raw, &installed); e != nil {
				return fail(e)
			}
			actual, e := loopruntime.ReadSessionReadbackFile(installed.Profile.Path, nil)
			if e != nil || actual != installed.Profile {
				return fail(errors.New("installed Host profile template changed"))
			}
			loaded, e := loopruntime.LoadOpenShellProfile(installed.Profile.Path)
			if e != nil {
				return fail(e)
			}
			if installed.Schema != 1 || loaded.SourceSHA256 != installed.PolicySHA256 {
				return fail(errors.New("installed Host profile template dependencies changed"))
			}
			template = installed.Profile.Path
		}
		templateRef, e := loopruntime.ReadSessionReadbackFile(template, nil)
		if e != nil {
			return fail(e)
		}
		if _, e = loopruntime.LoadOpenShellProfile(template); e != nil {
			return fail(e)
		}
		binaryRef, e := hostReadbackBinaryRef(binary)
		if e != nil {
			return fail(e)
		}
		id, e := hostID()
		if e != nil {
			return fail(e)
		}
		selected = goalHostSelection{Schema: 1, Goal: goal, Operation: operation, Root: filepath.Join(root, "h", id[:16]), Template: templateRef, Noodle: binaryRef, Phase: "materializing", Previous: selected.Previous}
		if e = os.MkdirAll(filepath.Dir(selected.Root), 0700); e != nil {
			return fail(e)
		}
		if e = os.Mkdir(selected.Root, 0700); e != nil {
			return fail(e)
		}
		if e = saveGoalHostSelection(selectionFile, selected); e != nil {
			return fail(e)
		}
	}
	if selected.Phase == "materializing" {
		actual, e := loopruntime.ReadSessionReadbackFile(selected.Template.Path, nil)
		if e != nil || actual != selected.Template {
			return fail(errors.New("original Goal template changed"))
		}
		profile, e := materializeGoalHostProfile(ctx, selected, func() (net.Listener, error) {
			return net.Listen("tcp", "127.0.0.1:0")
		})
		if e != nil {
			return fail(e)
		}
		selected.Profile = profile.Profile
		selected.Phase = "prepared"
		if e = saveGoalHostSelection(selectionFile, selected); e != nil {
			return fail(e)
		}
	}
	result.HostRoot, result.Profile, result.Noodle = selected.Root, selected.Profile, selected.Noodle
	if _, err = readGoalHostProfile(selected); err != nil {
		result.Status = "unknown"
		return fail(err)
	}
	selected.Phase = "starting"
	if err = saveGoalHostSelection(selectionFile, selected); err != nil {
		return fail(err)
	}
	if err = launch(ctx, selected.Noodle.Path, selected.Root); err != nil {
		result.Status = "unknown"
		result.Next = goalHostNext(selected.Noodle.Path, goalFile, newOperation)
		return fail(err)
	}
	observation, err := goalHostResources(ctx, selected)
	if err != nil {
		result.Status = "unknown"
		result.Next = goalHostNext(selected.Noodle.Path, goalFile, newOperation)
		return fail(err)
	}
	if observation.Admission.CanAccept == nil || !*observation.Admission.CanAccept {
		result.Status = "unknown"
		return fail(errors.New("new Goal Host is not available for admission"))
	}
	selected.Phase = "ready"
	if err = saveGoalHostSelection(selectionFile, selected); err != nil {
		return fail(err)
	}
	result.Status = "ready"
	result.HostInstanceID = observation.Instance
	result.Noodle = observation.Binary
	result.Next = goalHostNext(selected.Noodle.Path, goalFile, newOperation)
	return result, nil
}

func sameGoalOperation(a, b *loopruntime.SessionReadbackFileRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func goalHostProcessAbsent(root string) bool {
	pool, err := readHostPool(root)
	if err != nil {
		return false
	}
	generation, ok := pool.Generations[pool.Active]
	if !ok || generation.Process.PID <= 0 || generation.Process.SessionID != generation.ID || generation.Process.StartedAt.IsZero() {
		return false
	}
	absent, err := generation.Process.GroupAbsent()
	return err == nil && absent
}

func goalHostNext(binary, goal, operation string) map[string]any {
	argv := []string{binary, "host", "ensure", "--goal-file", goal}
	if operation != "" {
		argv = append(argv, "--new-host", "--request-file", operation)
	}
	return map[string]any{"owner": "Noodle.Host", "operation": "original_goal_host_readback", "argv": argv}
}

func saveGoalHostSelection(path string, value goalHostSelection) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, raw)
}

func readGoalHostProfile(selected goalHostSelection) (loopruntime.GoalProfileReceipt, error) {
	profile, err := loopruntime.ReadGoalOpenShellProfile(selected.Root)
	if err == nil && (profile.Template != selected.Template || profile.Profile != selected.Profile) {
		err = errors.New("original Goal Host profile materialization changed")
	}
	if err != nil {
		err = fmt.Errorf("%w: %w", errGoalHostProfileReadback, err)
	}
	return profile, err
}

func materializeGoalHostProfile(ctx context.Context, selected goalHostSelection, listen func() (net.Listener, error)) (loopruntime.GoalProfileReceipt, error) {
	var empty loopruntime.GoalProfileReceipt
	hosts := filepath.Dir(selected.Root)
	lock, err := lockfile.Acquire(ctx, filepath.Join(hosts, "profiles.lock"))
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	if _, err := os.Lstat(filepath.Join(selected.Root, "openshell")); err == nil {
		profile, err := loopruntime.ReadGoalOpenShellProfile(selected.Root)
		if err == nil && profile.Template != selected.Template {
			err = errors.New("original Goal Host template changed")
		}
		return profile, err
	} else if !os.IsNotExist(err) {
		return empty, err
	}
	entries, err := os.ReadDir(hosts)
	if err != nil {
		return empty, err
	}
	used := map[string]bool{}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return empty, errors.New("original Goal Host custody contains a symlink")
		}
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(hosts, entry.Name())
		if _, err := os.Lstat(filepath.Join(root, "openshell")); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return empty, err
		}
		profile, err := loopruntime.ReadGoalOpenShellReservation(root)
		if err != nil {
			return empty, fmt.Errorf("original Goal Host profile readback %s: %w", root, err)
		}
		used[profile.Gateway] = true
	}
	// Keep rejected listeners bound until publication so the native chooser advances.
	for range len(used) + 1 {
		listener, err := listen()
		if err != nil {
			return empty, err
		}
		defer listener.Close()
		address := listener.Addr().String()
		if !used["https://"+address] {
			return loopruntime.MaterializeGoalOpenShellProfile(selected.Template.Path, selected.Root, address)
		}
	}
	return empty, errors.New("original Goal Host endpoint selection did not produce an unused listener")
}

type goalHostObservation struct {
	Instance  string
	Binary    loopruntime.SessionReadbackFileRef
	Admission struct {
		Status    string `json:"status"`
		CanAccept *bool  `json:"can_accept"`
		Error     string `json:"error"`
	}
}

func goalHostResources(ctx context.Context, selected goalHostSelection) (goalHostObservation, error) {
	var result goalHostObservation
	if _, err := readGoalHostProfile(selected); err != nil {
		return result, err
	}
	raw, err := nativeHostRPC(ctx, selected.Root, hostRequest{Operation: "resources"})
	if err != nil {
		return result, err
	}
	var row struct {
		Resources  admission.Snapshot `json:"resources"`
		Protocol   string             `json:"protocol"`
		Root       string             `json:"host_root"`
		Instance   string             `json:"host_instance_id"`
		BinaryPath string             `json:"binary_path"`
		BinarySHA  string             `json:"binary_sha256"`
		Admission  struct {
			Status    string `json:"status"`
			CanAccept *bool  `json:"can_accept"`
			Error     string `json:"error"`
		} `json:"admission"`
	}
	if err = json.Unmarshal(raw, &row); err != nil {
		return result, err
	}
	if row.Protocol != hostProtocol || row.Root != selected.Root || !validPoolID(row.Instance) || row.Resources.AdmissionPolicy != admission.WorkerAdmissionPolicy || row.Resources.Maximum != admission.DefaultWorkerLimit {
		return result, errors.New("original Goal Host identity differs")
	}
	binary, err := hostReadbackBinaryRef(row.BinaryPath)
	if err != nil || binary.SHA256 != row.BinarySHA {
		return result, errors.New("original Goal Host executable changed")
	}
	pool, err := readHostPool(selected.Root)
	if err != nil {
		return result, err
	}
	generation, ok := pool.Generations[row.Instance]
	if !ok || pool.Active != row.Instance || generation.Binary != binary.Path || generation.SHA != binary.SHA256 {
		return result, errors.New("original Goal Host active generation differs")
	}
	result.Instance, result.Binary, result.Admission = row.Instance, binary, row.Admission
	return result, nil
}
