package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
)

func TestManagedHostSwitchPreservesHistoryAndNeverReplaysLostStart(t *testing.T) {
	for _, fault := range []string{"none", "lost-launch-ack", "raced-owner", "missing-witness", "poisoned-actor", "capacity-hold", "same-digest"} {
		t.Run(fault, func(t *testing.T) {
			_, _, budget, old, original := emptyRetirementControllerFixture(t, "none")
			root := old.controlRoot()
			historical := admission.Resident{ID: "completed-gateway", Name: "gateway", State: "exited", Contexts: []string{}, Released: []string{"completed-context"}}
			historyRaw, _ := json.Marshal(map[string]admission.Resident{historical.ID: historical})
			if err := os.WriteFile(filepath.Join(old.dir, "residents.json"), historyRaw, 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			old.residents, err = admission.OpenResidents(old.dir)
			if err != nil {
				t.Fatal(err)
			}
			request := strings.Repeat("e", 32)
			owner := hostTaskOwner{State: "terminal", Current: hostReceipt{RequestID: request, HostInstanceID: old.instanceID, ContextID: "completed-context", ContextState: "completed"}, unknown: map[string]json.RawMessage{"original_extension": json.RawMessage(`{"retained":true}`)}}
			if err = updateHostPool(root, func(p *hostPool) error { p.Owners[request] = owner; return nil }); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(filepath.Dir(root), "target-noodle")
			if err = os.WriteFile(binary, []byte("new immutable fixture binary"), 0700); err != nil {
				t.Fatal(err)
			}
			if fault == "same-digest" {
				raw, e := os.ReadFile(old.binaryPath)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(binary, raw, 0700); e != nil {
					t.Fatal(e)
				}
			}
			ref, err := hostReadbackBinaryRef(binary)
			if err != nil {
				t.Fatal(err)
			}
			launches, sourceReads := 0, 0
			var stopWaiting context.CancelFunc
			exited := false
			var target *noodleHost
			native := managedHostSwitchNative{emptyHostRetirementNative: original}
			native.absent = func(process procx.ProcessRecord) (bool, error) {
				if process.SessionID != old.instanceID {
					return false, errors.New("foreign group")
				}
				return exited, nil
			}
			native.rpc = func(ctx context.Context, dir string, req hostRequest) (json.RawMessage, error) {
				// Both readbacks would deadlock if the foreground held the machine lock.
				if snapshot := budget.Snapshot().MachineCapacity; snapshot == nil || strings.Contains(snapshot.Reason, "deadline exceeded") {
					return nil, errors.New("resources self-locked original machine owner")
				}
				p, e := readHostPool(root)
				if e != nil {
					return nil, e
				}
				if dir == old.dir {
					sourceReads++
					if sourceReads == 2 && fault == "raced-owner" {
						if e = updateHostPool(root, func(p *hostPool) error {
							p.Owners[strings.Repeat("f", 32)] = hostTaskOwner{State: "admitting"}
							return nil
						}); e != nil {
							return nil, e
						}
					}
					if p.Active == "" {
						if fault == "missing-witness" {
							stopWaiting()
						}
						if fault == "poisoned-actor" {
							old.rolling.failure = "original actor pool timeout"
							return json.Marshal(old.resources())
						}
						if fault != "missing-witness" && !exited {
							if !old.advanceRolling(ctx) {
								return nil, errors.New("original inactive actor failed to exit")
							}
							if e = old.saveResidentExitWitness(); e != nil {
								return nil, e
							}
							exited = true
						}
					}
					return json.Marshal(old.resources())
				}
				if target != nil && dir == target.dir {
					return json.Marshal(target.resources())
				}
				return nil, errors.New("foreign readback")
			}
			native.prepare = func(ctx context.Context, b *admission.Budget, source admission.PreparationSource, claim admission.ResourceClaim) (admission.PreparationRef, error) {
				if fault == "capacity-hold" {
					return admission.PreparationRef{}, admission.ErrDeferred
				}
				return b.PrepareMachineResources(ctx, source, claim, hostShutdownTestSampler{})
			}
			native.launch = func(ctx context.Context, actual, binary, digest string) (hostGeneration, error) {
				launches++
				admitted, ok := ctx.Value(goalPreparationContextKey{}).(goalResidentAdmission)
				qid, _ := ctx.Value(managedSwitchLaunchKey{}).(string)
				if !ok || admitted.Root != root || actual != root || admitted.Preparation.ID == "" || qid == "" {
					t.Fatal("launcher guard was bypassed")
				}
				id := strings.Repeat("a", 32)
				g := hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), Binary: binary, SHA: digest, Preparation: &admitted.Preparation, State: "starting", Process: procx.ProcessRecord{PID: os.Getpid(), SessionID: id, StartedAt: time.Now().UTC()}}
				if e := os.MkdirAll(g.Directory, 0700); e != nil {
					return g, e
				}
				if e := updateHostPool(root, func(p *hostPool) error {
					p.Generations[id] = g
					for i := range p.Queue {
						if p.Queue[i].ID == qid {
							p.Queue[i].Generation = id
							return nil
						}
					}
					return errors.New("original queue missing")
				}); e != nil {
					return g, e
				}
				residents, e := admission.OpenResidents(g.Directory)
				if e != nil {
					return g, e
				}
				target = &noodleHost{dir: g.Directory, instanceID: id, binaryPath: binary, binarySHA: digest, budget: budget, residents: residents, contexts: map[string]*hostContext{}, rolling: &hostRolling{root: root, carry: map[string]rollingCarry{}}}
				if fault == "lost-launch-ack" {
					return g, errors.New("original launch ACK lost")
				}
				return g, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)

			stopWaiting = cancel
			defer cancel()
			result, managed, err := switchManagedHost(ctx, root, ref.Path, ref.SHA256, native)
			if fault == "lost-launch-ack" {
				if err == nil || launches != 1 {
					t.Fatal("lost start ACK reported success", result, err, launches)
				}
				result, managed, err = switchManagedHost(ctx, root, ref.Path, ref.SHA256, native)
			}
			p, e := readHostPool(root)
			if e != nil {
				t.Fatal(e)
			}
			if fault == "none" || fault == "lost-launch-ack" {
				if err != nil || !managed || result.State != "active" || launches != 1 || p.Active != target.instanceID {
					t.Fatal("same Host switch failed", result, err, launches)
				}
				if p.Generations[old.instanceID].ResidentExit == nil || p.Generations[old.instanceID].State != "exited" || budget.Snapshot().MachineCapacity.Leases != 1 {
					t.Fatal("original resident return or history missing", p)
				}
				held := p.Owners[request]
				held.MaintenanceHold = ""
				got, _ := json.Marshal(held)
				want, _ := json.Marshal(owner)
				if string(got) != string(want) {
					t.Fatal("completed owner history changed", string(got), string(want))
				}
				if p.Owners[request].MaintenanceHold != result.ID {
					t.Fatal("historical owner not held")
				}
				rows, e := old.residents.Snapshot()
				if e != nil || len(rows) != 1 || rows[0].Released[0] != "completed-context" {
					t.Fatal("completed resident history lost", rows, e)
				}
				again, _, e := switchManagedHost(ctx, root, ref.Path, ref.SHA256, native)
				if e != nil || again.State != "active" || launches != 1 {
					t.Fatal("active retry relaunched", again, e)
				}
			} else {
				if err == nil || !managed || launches != 0 || p.Generations[old.instanceID].ResidentExit != nil {
					t.Fatal("unproven retirement launched or erased source", result, err, launches)
				}
				if fault == "raced-owner" && len(p.Owners) != 2 {
					t.Fatal("late original owner was erased")
				}
				if fault == "capacity-hold" && (p.Active != old.instanceID || p.Generations[old.instanceID].State != "active" || exited) {
					t.Fatal("capacity HOLD fenced old actor")
				}
			}
		})
	}
}
