package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

func newRetireMaintenanceSuccessorCmd() *cobra.Command {
	var plan, planSHA, request, requestSHA, closure, closureSHA, failure, failureSHA string
	var apply bool
	cmd := &cobra.Command{Use: "retire-successor", Short: "Retire one normally closed failed maintenance successor without changing its outcome", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		proof := successorRetirement{Schema: 1}
		for _, input := range []struct {
			path, expected string
			ref            *loopruntime.SessionReadbackFileRef
		}{{plan, planSHA, &proof.Plan}, {request, requestSHA, &proof.Request}, {closure, closureSHA, &proof.Closure}, {failure, failureSHA, &proof.Failure}} {
			ref, err := loopruntime.ReadSessionReadbackFile(input.path, nil)
			if err != nil {
				return err
			}
			if input.expected != "" && ref.SHA256 != input.expected {
				return fmt.Errorf("reviewed successor retirement input changed")
			}
			*input.ref = ref
		}
		path, err := retireMaintenanceSuccessor(proof, apply)
		if err != nil {
			return err
		}
		response := map[string]any{"status": "eligible", "resource_retirement": path, "task_outcome": "failed", "effects": []string{}}
		if apply {
			response["status"] = "retired"
		} else {
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			response["next"] = map[string]any{"owner": "Noodle", "argv": []string{executable, "openshell", "progress-maintenance", "retire-successor", "--plan", proof.Plan.Path, "--expected-sha256", proof.Plan.SHA256, "--request-file", proof.Request.Path, "--expected-request-sha256", proof.Request.SHA256, "--closed-host-file", proof.Closure.Path, "--expected-closed-host-sha256", proof.Closure.SHA256, "--failure-file", proof.Failure.Path, "--expected-failure-sha256", proof.Failure.SHA256, "--apply"}}
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
	}}
	for name, target := range map[string]*string{"plan": &plan, "expected-sha256": &planSHA, "request-file": &request, "expected-request-sha256": &requestSHA, "closed-host-file": &closure, "expected-closed-host-sha256": &closureSHA, "failure-file": &failure, "expected-failure-sha256": &failureSHA} {
		cmd.Flags().StringVar(target, name, "", "Exact original successor retirement input")
		_ = cmd.MarkFlagRequired(name)
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Publish resource retirement only; preserve the failed native receipt")
	return cmd
}

func retireMaintenanceSuccessor(proof successorRetirement, apply bool) (string, error) {
	plan, closed, receipt, before, err := validateSuccessorRetirement(proof)
	if err != nil {
		return "", err
	}
	hostLock, err := lockfile.TryLock(filepath.Join(plan.HostDir, "host.lock"))
	if err != nil {
		return "", err
	}
	defer hostLock.Close()
	projectLock, err := lockfile.TryLock(filepath.Join(receipt.ProjectDir, ".noodle", "noodle.lock"))
	if err != nil {
		return "", err
	}
	defer projectLock.Close()
	// Repeat immutable inputs under both owner locks before any marker write.
	if _, _, _, _, err := validateSuccessorRetirement(proof); err != nil {
		return "", err
	}
	if procx.IsPIDAlive(closed.HostPID) {
		return "", fmt.Errorf("original successor Host process remains")
	}
	var resources successorResources
	if err := readRetirementSource(closed.Resources, &resources); err != nil {
		return "", err
	}
	absent, err := procx.ProcessGroupAbsent(resources.Residents.Records[0].ProcessGroup)
	if err != nil {
		return "", err
	}
	if !absent {
		return "", fmt.Errorf("original successor gateway group remains")
	}
	profile, err := loopruntime.LoadOpenShellProfile(plan.Profile.Path)
	if err != nil {
		return "", err
	}
	if err := readRetirementSource(plan.Profile, nil); err != nil {
		return "", err
	}
	for _, endpoint := range []string{resources.WebURL, profile.Gateway.Binding.Gateway} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("original successor endpoint identity is missing")
		}
		conn, err := net.DialTimeout("tcp", u.Host, time.Second)
		if err == nil {
			_ = conn.Close()
			return "", fmt.Errorf("original successor endpoint remains open")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return "", fmt.Errorf("original successor endpoint closure is unknown: %w", err)
		}
	}
	if err := requireOriginalMaintainedReview(receipt.ProjectDir, plan.SessionDirectory); err != nil {
		return "", err
	}
	receiptsPath := filepath.Join(plan.HostDir, "receipts.json")
	data, err := os.ReadFile(receiptsPath)
	if err != nil {
		return "", err
	}
	var live []map[string]any
	if err := json.Unmarshal(data, &live); err != nil {
		return "", err
	}
	if !reflect.DeepEqual(live, before) {
		return "", fmt.Errorf("original successor Host history changed before retirement")
	}
	path := successorRetirementPath(plan.HostDir, receipt.ContextID)
	if apply {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		encoded, err := json.MarshalIndent(proof, "", "  ")
		if err != nil {
			return "", err
		}
		if existing, err := os.ReadFile(path); err == nil {
			if !bytes.Equal(existing, encoded) {
				return "", fmt.Errorf("successor retirement already belongs to a different proof")
			}
		} else if !os.IsNotExist(err) {
			return "", err
		} else if err := writeMaintenanceExclusive(path, encoded); err != nil {
			return "", err
		}
	}
	after, err := os.ReadFile(receiptsPath)
	if err != nil || !bytes.Equal(data, after) {
		return "", errors.Join(err, fmt.Errorf("retirement changed original Host receipt bytes"))
	}
	if _, _, _, _, err := validateSuccessorRetirement(proof); err != nil {
		return "", err
	}
	return path, nil
}

func requireOriginalMaintainedReview(project, sessionDirectory string) error {
	var snapshot struct {
		State state.State `json:"state"`
	}
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(project, ".noodle", "state.snapshot.json"), &snapshot); err != nil {
		return err
	}
	matched := 0
	for id, order := range snapshot.State.Orders {
		for _, stage := range order.Stages {
			for _, attempt := range stage.Attempts {
				if attempt.SessionID != filepath.Base(sessionDirectory) {
					continue
				}
				if len(order.Stages) != 1 || order.Status != state.OrderActive || stage.Status != state.StageReview || stage.Runtime != "openshell" || len(stage.Attempts) != 1 || attempt.Status != state.AttemptCancelled || snapshot.State.PendingReviews[id].SessionID != attempt.SessionID {
					return fmt.Errorf("successor retirement has changed or unfinished native task history")
				}
				matched++
			}
		}
	}
	if matched != 1 {
		return fmt.Errorf("original maintained review is missing or ambiguous")
	}
	return nil
}
