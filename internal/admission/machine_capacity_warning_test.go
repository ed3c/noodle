//go:build darwin || linux

package admission

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMachineCapacityWarningAdmitsOriginalPreparationAndWorker(t *testing.T) {
	for _, sequence := range [][]Pressure{{Warning, Warning, Warning}, {Warning, Normal, Warning}} {
		t.Run(strings.Join([]string{string(sequence[0]), string(sequence[1]), string(sequence[2])}, "-"), func(t *testing.T) {
			root := capacityFixtureRoot(t)
			claim, source := capacityFixtureClaim(root), preparationFixtureSource(root)
			b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", claim)
			b.BindResourceSource("original-native-preparation", "preparation", func(string) (ResourceClaim, error) { return claim, nil })
			for _, pressure := range sequence {
				*now = now.Add(PollInterval)
				b.Observe(Sample{At: *now, Pressure: pressure, TotalMemoryBytes: 10000, CPUs: 128}, nil)
			}
			ref, err := b.reservePreparation(source, claim)
			if err != nil {
				t.Fatal("Warning alone deferred preparation", err)
			}
			if err = b.StartPreparationEffects(ref); err != nil {
				t.Fatal(err)
			}
			if err = b.BindPreparation(ref, "owner", "order", "openshell"); err != nil {
				t.Fatal(err)
			}
			permit, err := b.TryAcquireTask("owner", "order", "openshell")
			if err != nil {
				t.Fatal("Warning alone deferred worker", err)
			}
			if permit.allocation.machineID != ref.ID {
				t.Fatal("promotion replaced original nonce")
			}
			if snapshot := b.Snapshot(); snapshot.Pressure != Warning || snapshot.Active != 1 || snapshot.MachineCapacity.Leases != 1 || snapshot.Maximum != 9 {
				t.Fatalf("warning/nonce/quota observation changed: %+v", snapshot)
			}
			permit.Release()
		})
	}
}

func TestMachineCapacityWarningPreservesOtherResourceRefusals(t *testing.T) {
	for _, condition := range []string{"critical", "unknown", "invalid", "stale", "swap-used", "swap-rate", "cpu", "ram", "disk"} {
		for _, consumer := range []string{"preparation", "worker"} {
			t.Run(condition+"/"+consumer, func(t *testing.T) {
				root := capacityFixtureRoot(t)
				claim := capacityFixtureClaim(root)
				switch condition {
				case "cpu":
					claim.CPUs = claim.Policy.CPUs + 1
				case "ram":
					claim.MemoryBytes = claim.Policy.MemoryBytes + 1
				}
				b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", claim)
				b.BindResourceSource("original-native-preparation", "preparation", func(string) (ResourceClaim, error) { return claim, nil })
				for range 3 {
					*now = now.Add(PollInterval)
					b.Observe(Sample{At: *now, Pressure: Warning, TotalMemoryBytes: 10000, CPUs: 128}, nil)
				}
				sample := Sample{At: now.Add(PollInterval), Pressure: Warning, TotalMemoryBytes: 10000, CPUs: 128}
				*now = sample.At
				expected := ""
				switch condition {
				case "critical":
					sample.Pressure = Critical
					expected = "ram_pressure_critical"
				case "unknown":
					sample.Pressure = Unknown
					expected = "ram_pressure_unknown"
				case "invalid":
					sample.Pressure = Pressure("invalid")
					expected = "ram_pressure_invalid"
				case "stale":
					expected = "machine_sample_stale"
				case "swap-used":
					sample.SwapUsedBytes = claim.Policy.MaxSwapUsedBytes + 1
					expected = "swap_used_pressure"
				case "swap-rate":
					sample.Swapouts = (claim.Policy.MaxSwapoutBytesPerSecond + 1) * uint64(PollInterval/time.Second)
					expected = "swapout_pressure"
				case "cpu":
					expected = "cpu_reservation_capacity_insufficient"
				case "ram":
					expected = "ram_reservation_capacity_insufficient"
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
