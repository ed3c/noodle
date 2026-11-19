//go:build darwin || linux

package admission

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMachineCapacityPressureObservationsAdmitOriginalPreparationAndWorker(t *testing.T) {
	for _, sequence := range [][]Pressure{{Warning, Normal, Warning}, {Critical, Unknown, Critical}, {Unknown, Critical, Unknown}} {
		t.Run(strings.Join([]string{string(sequence[0]), string(sequence[1]), string(sequence[2])}, "-"), func(t *testing.T) {
			root := capacityFixtureRoot(t)
			claim, source := capacityFixtureClaim(root), preparationFixtureSource(root)
			claim.MemoryBytes = claim.Policy.MemoryBytes + 1
			b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", claim)
			b.BindResourceSource("original-native-preparation", "preparation", func(string) (ResourceClaim, error) { return claim, nil })
			var sample Sample
			for _, pressure := range sequence {
				*now = now.Add(PollInterval)
				sample = Sample{At: *now, Pressure: pressure, CPUs: 128}
				if pressure == Unknown {
					sample.PressureObservationError = "fixture pressure observation unavailable"
				}
				b.Observe(sample, nil)
			}
			ref, err := b.reservePreparation(source, claim)
			if err != nil {
				t.Fatal("memory pressure observation deferred preparation", err)
			}
			if err = b.StartPreparationEffects(ref); err != nil {
				t.Fatal(err)
			}
			if err = b.BindPreparation(ref, "owner", "order", "openshell"); err != nil {
				t.Fatal(err)
			}
			permit, err := b.TryAcquireTask("owner", "order", "openshell")
			if err != nil {
				t.Fatal("memory pressure observation deferred worker", err)
			}
			if permit.allocation.machineID != ref.ID {
				t.Fatal("promotion replaced original nonce")
			}
			snapshot := b.Snapshot()
			if snapshot.Pressure != sample.Pressure || snapshot.PressureObservationError != sample.PressureObservationError || snapshot.SwapoutRate == nil ||
				snapshot.Active != 1 || snapshot.MachineCapacity.Leases != 1 || snapshot.Maximum != 9 {
				t.Fatalf("pressure, Swap, or quota readback changed: %+v", snapshot)
			}
			permit.Release()
		})
	}
}

func TestMachineCapacityMemoryPressurePreservesSwapAndDiskRefusals(t *testing.T) {
	for _, pressure := range []Pressure{Critical, Unknown} {
		for _, condition := range []string{"stale", "swap-used", "swap-rate", "disk"} {
			for _, consumer := range []string{"preparation", "worker"} {
				t.Run(string(pressure)+"/"+condition+"/"+consumer, func(t *testing.T) {
					root := capacityFixtureRoot(t)
					claim := capacityFixtureClaim(root)
					b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", claim)
					b.BindResourceSource("original-native-preparation", "preparation", func(string) (ResourceClaim, error) { return claim, nil })
					for range 3 {
						*now = now.Add(PollInterval)
						b.Observe(Sample{At: *now, Pressure: pressure, TotalMemoryBytes: 10000, CPUs: 128}, nil)
					}
					sample := Sample{At: now.Add(PollInterval), Pressure: pressure, CPUs: 128}
					*now = sample.At
					expected := ""
					switch condition {
					case "stale":
						expected = "machine_sample_stale"
					case "swap-used":
						sample.SwapUsedBytes = claim.Policy.MaxSwapUsedBytes + 1
						expected = "swap_used_pressure"
					case "swap-rate":
						sample.Swapouts = (claim.Policy.MaxSwapoutBytesPerSecond + 1) * uint64(PollInterval/time.Second)
						expected = "swapout_pressure"
					case "disk":
						b.machine.probe = func(string) (diskCapacity, error) {
							return diskCapacity{Volume: "fixture-volume", Available: claim.DiskBytes + claim.Policy.DiskSafetyBytes - 1}, nil
						}
						expected = "disk_reservation_capacity_insufficient"
					}
					b.Observe(sample, nil)
					if condition == "stale" {
						*now = now.Add(MaxSampleAge + time.Second)
					}
					var err error
					if consumer == "preparation" {
						_, err = b.reservePreparation(preparationFixtureSource(root), claim)
					} else {
						_, err = b.TryAcquireTask("owner", "order", "openshell")
					}
					if !errors.Is(err, ErrDeferred) || !strings.Contains(err.Error(), expected) {
						t.Fatalf("%s guard lost: %v", expected, err)
					}
					if snapshot := b.Snapshot(); snapshot.Active != 0 || snapshot.MachineCapacity.Leases != 0 {
						t.Fatal("refusal changed original capacity", snapshot)
					}
				})
			}
		}
	}
}
