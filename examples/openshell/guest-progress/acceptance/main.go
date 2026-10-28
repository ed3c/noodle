// Command acceptance runs the guest progress fixture through an isolated,
// provider-free native gateway owned by Noodle's normal resident/custody APIs.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	rt "github.com/poteto/noodle/runtime"
	"google.golang.org/protobuf/encoding/protojson"
)

type inputs struct {
	Schema            int                    `json:"schema"`
	Root              string                 `json:"root"`
	Name              string                 `json:"name"`
	Endpoint          string                 `json:"endpoint"`
	GatewayVersion    string                 `json:"gateway_version"`
	GatewaySHA        string                 `json:"gateway_sha256"`
	CLI               string                 `json:"cli"`
	CLISHA            string                 `json:"cli_sha256"`
	Fixture           string                 `json:"fixture"`
	FixtureSHA        string                 `json:"fixture_sha256"`
	Config            string                 `json:"config"`
	ConfigSHA         string                 `json:"config_sha256"`
	Spec              string                 `json:"spec"`
	SpecSHA           string                 `json:"spec_sha256"`
	ClientMetadata    string                 `json:"client_metadata"`
	ClientMetadataSHA string                 `json:"client_metadata_sha256"`
	Environment       []string               `json:"environment"`
	Resident          admission.ResidentSpec `json:"resident"`
}

type owner struct {
	cfg           inputs
	residents     *admission.Residents
	budget        *admission.Budget
	gateway       *rt.OpenShellGateway
	custody       *rt.OpenShellCustody
	worker        *rt.OpenShellWorker
	permit        *admission.Permit
	acquired      bool
	exitObserved  bool
	execAttempted bool
	sources       map[string]string
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: acceptance /absolute/fresh/inputs.json")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func (o *owner) evidence(name string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(o.cfg.Root, name), b)
}
func (o *owner) verify() error {
	for path, expected := range o.sources {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if hash(b) != expected {
			return fmt.Errorf("fixture source changed: %s", path)
		}
	}
	return nil
}

func run(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	o := &owner{sources: map[string]string{filename: hash(data)}}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&o.cfg); err != nil {
		return err
	}
	c := o.cfg
	if c.Schema != 1 || !filepath.IsAbs(c.Root) || filepath.Dir(filename) != c.Root || !strings.HasPrefix(filepath.Base(c.Root), "ngp-") || c.Name != filepath.Base(c.Root) || len(c.Resident.Argv) == 0 || c.Resident.Directory != c.Root {
		return fmt.Errorf("fresh fixture owner binding invalid")
	}
	for path, expected := range map[string]string{c.Resident.Argv[0]: c.GatewaySHA, c.CLI: c.CLISHA, c.Fixture: c.FixtureSHA, c.Config: c.ConfigSHA, c.Spec: c.SpecSHA, c.ClientMetadata: c.ClientMetadataSHA} {
		o.sources[path] = expected
	}
	if err = o.verify(); err != nil {
		return err
	}
	if c.Config != filepath.Join(c.Root, "gateway.toml") || c.Spec != filepath.Join(c.Root, "sandbox-spec.json") || c.ClientMetadata != filepath.Join(c.Root, "client", "openshell", "gateways", c.Name, "metadata.json") {
		return fmt.Errorf("fixture source is outside its new owner root")
	}
	marker, err := os.OpenFile(filepath.Join(c.Root, "run-started.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("fixture is not a fresh original invocation: %w", err)
	}
	markerErr := json.NewEncoder(marker).Encode(map[string]any{"pid": os.Getpid(), "inputs_sha256": hash(data)})
	markerErr = errors.Join(markerErr, marker.Sync(), marker.Close(), filex.SyncDir(c.Root))
	if markerErr != nil {
		return markerErr
	}
	var metadata struct {
		Name     string `json:"name"`
		Endpoint string `json:"gateway_endpoint"`
	}
	b, err := os.ReadFile(c.ClientMetadata)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, &metadata) != nil || metadata.Name != c.Name || metadata.Endpoint != c.Endpoint {
		return fmt.Errorf("fixture CLI selection differs")
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(c.Root, "tls/client/tls.crt"), filepath.Join(c.Root, "tls/client/tls.key"))
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(filepath.Join(c.Root, "tls/ca.crt"))
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return fmt.Errorf("fixture CA unavailable")
	}
	o.budget = admission.NewBudget(2)
	o.budget.Register("guest-progress-proof")
	o.residents, err = admission.OpenResidents(filepath.Join(c.Root, "host"))
	if err != nil {
		return err
	}
	o.residents.BindBudget(o.budget)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.budget.Run(ctx, admission.NewSampler())
	o.gateway, err = rt.NewOpenShellGateway(o.residents, o.budget, rt.OpenShellGatewaySpec{Resident: c.Resident, Binding: rt.OpenShellCustodyBinding{StateRoot: filepath.Join(c.Root, "custody"), Gateway: c.Endpoint, Workspace: "default", Owner: c.Name, ConfigSource: c.Config, ConfigFingerprint: hash(data)}, TLS: rt.OpenShellTLS{RootCAs: pool, Certificate: cert}, Version: c.GatewayVersion, ComputeDriver: "vm", ReadinessTimeout: 30 * time.Second})
	if err != nil {
		return err
	}
	// This is a wait for real observed admission, not fabricated Normal samples.
	if err = o.waitBudget(ctx); err != nil {
		_ = o.gateway.Close()
		return err
	}
	op, opCancel := context.WithTimeout(ctx, 40*time.Second)
	o.custody, err = o.gateway.Acquire(op, "guest-progress-proof")
	opCancel()
	if err != nil {
		records, readErr := o.residents.Snapshot()
		if readErr == nil && len(records) == 0 {
			return errors.Join(err, o.gateway.Close())
		}
		return o.retain(errors.Join(err, readErr))
	}
	if err = o.waitBudget(ctx); err != nil {
		return o.finishOrRetain(err)
	}
	o.permit, err = o.budget.TryAcquireTask("guest-progress-proof", "progress-fixture")
	if err != nil {
		return o.finishOrRetain(err)
	}
	if err = o.execute(ctx); err != nil {
		return o.finishOrRetain(err)
	}
	return o.finishOrRetain(nil)
}

func (o *owner) waitBudget(ctx context.Context) error {
	timeout := time.NewTimer(time.Minute)
	defer timeout.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		s := o.budget.Snapshot()
		if s.Limit > s.Allocations && s.Credit > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			_ = o.evidence("admission-refusal.json", s)
			return admission.ErrDeferred
		case <-tick.C:
		}
	}
}
func (o *owner) ready(ctx context.Context) error {
	op, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := o.worker.Readback(op, o.custody); err != nil {
			return err
		}
		if o.worker.Record().Boundary == rt.OpenShellBoundaryReady {
			return nil
		}
		select {
		case <-op.Done():
			return op.Err()
		case <-tick.C:
		}
	}
}
func (o *owner) execute(ctx context.Context) error {
	var spec pb.SandboxSpec
	b, err := os.ReadFile(o.cfg.Spec)
	if err != nil {
		return err
	}
	if err = protojson.Unmarshal(b, &spec); err != nil {
		return err
	}
	if len(spec.Providers) != 0 {
		return fmt.Errorf("fixture unexpectedly configures providers")
	}
	op, cancel := context.WithTimeout(ctx, 30*time.Second)
	name, err := o.custody.Create(op, rt.OpenShellTask{OrderID: "progress-fixture", AttemptID: uuid(), SessionID: uuid()}, &spec)
	cancel()
	if err != nil {
		return err
	}
	for _, record := range o.custody.Snapshot().Records {
		if record.Name == name {
			o.worker, err = rt.NewOpenShellWorker(record.Worker.Identity)
			break
		}
	}
	if err != nil {
		return err
	}
	if o.worker == nil {
		return fmt.Errorf("original fixture worker missing")
	}
	if err = o.ready(ctx); err != nil {
		return err
	}
	if err = o.verify(); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(o.cfg.Root, "upload.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	uploadContext, uploadCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer uploadCancel()
	upload := exec.CommandContext(uploadContext, o.cfg.CLI, "--gateway", o.cfg.Name, "--workspace", "default", "--color", "never", "sandbox", "upload", "--no-git-ignore", name, o.cfg.Fixture, "/tmp")
	upload.Env = o.cfg.Environment
	upload.Stdout = log
	upload.Stderr = log
	err = upload.Run()
	closeErr := log.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = o.verify(); err != nil {
		return err
	}
	if err = o.worker.Readback(ctx, o.custody); err != nil {
		return err
	}
	if o.worker.Record().Boundary != rt.OpenShellBoundaryReady {
		return fmt.Errorf("fixture VM changed around upload")
	}
	command := rt.OpenShellCommand{RequestID: uuid(), Workdir: "/tmp", Env: map[string]string{}, Argv: []string{"/tmp/" + filepath.Base(o.cfg.Fixture), "-test.run=Test(StopReapsEscapedWriterBeforeArchiving|PostLaunchFailureRetainsOwnerUntilReaped)", "-test.v", "-test.timeout=40s"}}
	op, cancel = context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	o.execAttempted = true
	stream, launchErr := o.custody.StartOwned(op, o.worker.Record().Identity.Sandbox, uuid(), command)
	if stream == nil {
		return launchErr
	}
	output, err := os.OpenFile(filepath.Join(o.cfg.Root, "fixture.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	for chunk := range stream.Output() {
		if _, err = output.Write(chunk.Data); err != nil {
			_ = output.Close()
			return err
		}
	}
	result, waitErr := stream.Wait()
	o.exitObserved = result.ExitObserved
	if syncErr := output.Sync(); syncErr != nil {
		waitErr = errors.Join(waitErr, syncErr)
	}
	waitErr = errors.Join(waitErr, output.Close())
	if err = o.evidence("exec-result.json", result); err != nil {
		return err
	}
	if err = errors.Join(launchErr, waitErr); err != nil {
		return err
	}
	if !result.ExitObserved || result.ExitCode != 0 {
		return fmt.Errorf("native fixture did not pass: exit=%d observed=%t", result.ExitCode, result.ExitObserved)
	}
	return nil
}

func (o *owner) finishOrRetain(testErr error) error {
	// A failed fixture with a known native exit can still stop its exact VM.
	// Unknown create/exec/stop effects retain this original process and owner.
	if o.worker != nil && (o.exitObserved || !o.execAttempted) && o.worker.Record().Boundary != rt.OpenShellBoundaryStopped {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := o.worker.Stop(ctx, o.custody, o.custody)
		if err == nil {
			for o.worker.Record().Boundary != rt.OpenShellBoundaryStopped {
				if err = o.worker.Readback(ctx, o.custody); err != nil {
					break
				}
				select {
				case <-ctx.Done():
					err = ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				if err != nil {
					break
				}
			}
		}
		cancel()
		testErr = errors.Join(testErr, err)
	}
	canRelease := o.worker == nil && len(o.custody.Snapshot().Records) == 0 || o.worker != nil && (o.exitObserved || !o.execAttempted) && o.worker.Record().Boundary == rt.OpenShellBoundaryStopped && !o.worker.Record().StopPending
	if canRelease {
		if o.permit != nil {
			o.permit.Release()
		}
		if _, err := o.gateway.Release("guest-progress-proof"); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err = o.residents.WaitClosed(ctx)
			cancel()
			if err == nil {
				err = o.gateway.Close()
			}
			testErr = errors.Join(testErr, err)
			if err == nil {
				status := "PASS"
				if testErr != nil {
					status = "FAIL"
				}
				evidenceErr := o.evidence("result.json", map[string]any{"status": status, "fixture_sha256": o.cfg.FixtureSHA, "model_calls": 0, "providers": 0, "gateway_closed": true, "error": fmt.Sprint(testErr)})
				return errors.Join(testErr, evidenceErr)
			}
		} else {
			testErr = errors.Join(testErr, err)
		}
	}
	return o.retain(testErr)
}

func (o *owner) retain(testErr error) error {
	evidenceErr := o.evidence("retained.json", map[string]any{"status": "unknown", "owner_pid": os.Getpid(), "error": fmt.Sprint(testErr), "model_calls": 0, "gateway_closed": false})
	fmt.Fprintln(os.Stderr, "fixture owner retained; inspect evidence in", o.cfg.Root, errors.Join(testErr, evidenceErr))
	// Inspection keeps the actual owner alive. No failed effect is retried.
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if scanner.Text() == "inspect" {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "retained", "resources": o.budget.Snapshot(), "worker": o.workerRecord()})
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	select {}
}
func (o *owner) workerRecord() any {
	if o.worker == nil {
		return nil
	}
	return o.worker.Record()
}
