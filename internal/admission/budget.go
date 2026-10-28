package admission

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Pressure string

const (
	Unknown               Pressure = "unknown"
	Normal                Pressure = "normal"
	Warning               Pressure = "warning"
	Critical              Pressure = "critical"
	DefaultSwapUsedLimit           = uint64(15_000_000_000)
	WorkerAdmissionPolicy          = "swap-used-critical-guard-v2"
	PollInterval                   = 5 * time.Second
	SampleTimeout                  = time.Second
	MaxSampleAge                   = 15 * time.Second
)

var ErrDeferred = errors.New("worker resource admission deferred")

type Sample struct {
	At               time.Time
	Pressure         Pressure
	Swapouts         uint64 // cumulative bytes
	SwapUsedBytes    uint64 // current system-wide allocation
	TotalMemoryBytes uint64
	CPUs             int
	Unsupported      bool
}

type Sampler interface {
	Read(context.Context) (Sample, error)
}

type Snapshot struct {
	AdmissionPolicy     string               `json:"worker_admission_policy"`
	SharedQuota         *SharedQuotaSnapshot `json:"shared_quota,omitempty"`
	OwnedAllocations    int                  `json:"owned_allocations"`
	UnresolvedResidents int                  `json:"unresolved_residents"`
	Pressure            Pressure             `json:"pressure"`
	SampleError         *string              `json:"sample_error"`
	Reason              string               `json:"reason"`
	Active              int                  `json:"active_workers"`
	Residents           int                  `json:"active_residents"`
	Allocations         int                  `json:"active_allocations"`
	Maximum             int                  `json:"maximum_workers"`
	Limit               int                  `json:"effective_limit"`
	Credit              int                  `json:"start_credit"`
	StableSamples       int                  `json:"stable_samples"`
	SwapUsedLimit       uint64               `json:"swap_used_limit_bytes"`
	SwapUsedBytes       *uint64              `json:"swap_used_bytes"`
	SwapoutRate         *float64             `json:"swapout_bytes_per_second"`
	SampleAgeSeconds    float64              `json:"sample_age_seconds"`
	Unsupported         bool                 `json:"unsupported_monitor"`
}

type Budget struct {
	shared                                 *sharedQuota
	sharedError                            string
	pendingSharedReturns                   map[string]bool
	unresolvedResidents                    int
	switching                              bool
	mu                                     sync.Mutex
	maximum, active, limit, credit, stable int
	residents                              int
	auto                                   bool
	previous                               *Sample
	pressure                               Pressure
	reason                                 string
	swapUsedLimit                          uint64
	rate                                   float64
	rateValid                              bool
	unsupported                            bool
	sampleError                            *string
	contexts                               map[string]bool
	waiters                                []string
	waited                                 map[string]time.Time
	now                                    func() time.Time
}

func (b *Budget) SetUnresolvedResidents(count int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unresolvedResidents = count
}

// HoldForSwitch stops new workers without changing live owners or task pause state.
// Shared proof can still admit services needed by a worker in the old round.
func (b *Budget) HoldForSwitch() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.switching = true
	b.credit = 0
}

type allocation struct {
	sharedID string
	budget   *Budget
	refs     int
	resident bool
}
type Permit struct {
	parent              *Permit
	allocation          *allocation
	once                sync.Once
	released            bool
	replacementReserved bool
}

func HardwareLimit(memory uint64, cpus int) int {
	const gib = uint64(1 << 30)
	n := 1
	if memory > 4*gib {
		n = int((memory - 4*gib) / (3 * gib))
		if n < 1 {
			n = 1
		}
	}
	if cpus > 0 && n > cpus {
		n = cpus
	}
	return n
}

func NewBudget(maxWorkers int) *Budget {
	return NewBudgetWithSwapUsedLimit(maxWorkers, DefaultSwapUsedLimit)
}

func NewBudgetWithSwapUsedLimit(maxWorkers int, bytes uint64) *Budget {
	auto := maxWorkers <= 0
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	return &Budget{swapUsedLimit: bytes, maximum: maxWorkers, auto: auto, pressure: Unknown, reason: "awaiting_sample", contexts: map[string]bool{}, waited: map[string]time.Time{}, now: time.Now}
}

func (b *Budget) Register(id string) { b.mu.Lock(); defer b.mu.Unlock(); b.contexts[id] = true }
func (b *Budget) Unregister(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.contexts, id)
	delete(b.waited, id)
	b.removeWaiter(id)
}

func (b *Budget) removeWaiter(id string) {
	for i, x := range b.waiters {
		if x == id {
			b.waiters = append(b.waiters[:i], b.waiters[i+1:]...)
			return
		}
	}
}

func (b *Budget) Run(ctx context.Context, sampler Sampler) {
	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, SampleTimeout)
		sample, err := sampler.Read(readCtx)
		cancel()
		b.Observe(sample, err)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *Budget) Observe(s Sample, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.pendingSharedReturns {
		b.returnSharedAllocation(id)
	}
	b.credit = 0
	b.rateValid = false
	b.sampleError = nil
	if err != nil {
		message := err.Error()
		b.sampleError = &message
	}
	if err != nil || s.At.IsZero() {
		b.pressure = Unknown
		b.reason = "sample_failed"
		b.stable = 0
		b.limit = 0
		b.previous = nil
		return
	}
	if b.auto {
		if !s.Unsupported && (s.TotalMemoryBytes == 0 || s.CPUs < 1) {
			b.pressure = Unknown
			b.reason = "hardware_unknown"
			b.limit = 0
			b.stable = 0
			b.previous = nil
			return
		}
		b.maximum = HardwareLimit(s.TotalMemoryBytes, s.CPUs)
	}
	b.unsupported = s.Unsupported
	if s.Unsupported {
		b.previous = &s
		b.pressure = Unknown
		b.reason = "unsupported_swap_monitor"
		b.limit = 0
		return
	}
	previous := b.previous
	b.previous = &s
	b.pressure = s.Pressure
	b.rate = 0
	if previous == nil || !s.At.After(previous.At) || s.At.Sub(previous.At) > MaxSampleAge || s.Swapouts < previous.Swapouts || previous.Unsupported {
		b.reason = "counter_baseline"
		b.stable = 0
		b.limit = 0
		return
	}
	b.rateValid = true
	b.rate = float64(s.Swapouts-previous.Swapouts) / s.At.Sub(previous.At).Seconds()
	pressureAllowsAdmission := s.Pressure == Normal || s.Pressure == Warning
	if !pressureAllowsAdmission || s.SwapUsedBytes >= b.swapUsedLimit {
		b.reason = "memory_pressure"
		if pressureAllowsAdmission && s.SwapUsedBytes >= b.swapUsedLimit {
			b.reason = "swap_used_limit"
		}
		b.stable = 0
		b.limit = 0
		return
	}
	b.stable++
	if b.stable < 3 {
		b.reason = "stabilizing"
		b.limit = 0
		return
	}
	if b.limit < b.maximum {
		b.limit++
	}
	b.credit = 1
	b.reason = "ready"
}

func (b *Budget) stale() bool {
	return b.previous == nil || b.now().Sub(b.previous.At) > MaxSampleAge || b.now().Before(b.previous.At)
}

func (b *Budget) TryAcquire(id string) (*Permit, error) { return b.tryAcquire(id, false, "") }

func (b *Budget) tryAcquire(id string, resident bool, residentKey string) (*Permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pendingSharedReturns) != 0 || !b.contexts[id] || b.unresolvedResidents != 0 || (b.switching && (!resident || b.shared == nil || !b.shared.workerContext(id))) {
		return nil, ErrDeferred
	}
	if _, ok := b.waited[id]; !ok {
		b.waiters = append(b.waiters, id)
	}
	b.waited[id] = b.now()
	for len(b.waiters) > 0 {
		first := b.waiters[0]
		if b.contexts[first] && b.now().Sub(b.waited[first]) <= 30*time.Second {
			break
		}
		b.waiters = b.waiters[1:]
		delete(b.waited, first)
	}
	if b.stale() || b.active >= b.limit || (!b.unsupported && b.credit == 0) || len(b.waiters) == 0 || b.waiters[0] != id {
		return nil, ErrDeferred
	}
	sharedID := ""
	if b.shared != nil {
		var err error
		sharedID, err = b.shared.acquire(id, b.maximum, b.now(), !b.unsupported, resident, residentKey, b.switching)
		if err != nil {
			var uncertain *quotaWriteUncertain
			if errors.As(err, &uncertain) {
				b.rememberSharedReturn(sharedID)
			}
			if !errors.Is(err, ErrDeferred) {
				b.sharedError = err.Error()
			}
			return nil, ErrDeferred
		}
		b.sharedError = ""
	}
	b.active++
	if resident {
		b.residents++
	}
	if !b.unsupported {
		b.credit = 0
	}
	b.removeWaiter(id)
	delete(b.waited, id)
	return &Permit{allocation: &allocation{budget: b, refs: 1, sharedID: sharedID, resident: resident}}, nil
}

func (p *Permit) Release() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		b := p.allocation.budget
		b.mu.Lock()
		defer b.mu.Unlock()
		p.released = true
		if p.parent != nil {
			p.parent.replacementReserved = false
		}
		p.allocation.refs--
		if p.allocation.refs == 0 {
			if b.shared != nil {
				b.returnSharedAllocation(p.allocation.sharedID)
			}
			b.active--
			if p.allocation.resident {
				b.residents--
			}
		}
	})
}

func (p *Permit) ReserveReplacement(id string) (*Permit, error) {
	b := p.allocation.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pendingSharedReturns) != 0 || p.released || p.replacementReserved || !b.contexts[id] || b.unresolvedResidents != 0 || b.switching || b.stale() || b.limit == 0 || b.active > b.limit || (!b.unsupported && b.credit == 0) {
		return nil, ErrDeferred
	}
	if b.shared != nil {
		if err := b.shared.replacement(p.allocation.sharedID, b.now(), !b.unsupported); err != nil {
			if !errors.Is(err, ErrDeferred) {
				b.sharedError = err.Error()
			}
			return nil, ErrDeferred
		}
	}
	if !b.unsupported {
		b.credit = 0
	}
	p.replacementReserved = true
	p.allocation.refs++
	return &Permit{allocation: p.allocation, parent: p}, nil
}

func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{AdmissionPolicy: WorkerAdmissionPolicy, SwapUsedLimit: b.swapUsedLimit, Pressure: b.pressure, Reason: b.reason, Active: b.active - b.residents, Residents: b.residents, Allocations: b.active, Maximum: b.maximum, Limit: b.limit, Credit: b.credit, StableSamples: b.stable, Unsupported: b.unsupported}
	if b.sampleError != nil {
		message := *b.sampleError
		s.SampleError = &message
	}
	if b.rateValid {
		rate := b.rate
		s.SwapoutRate = &rate
	}
	if b.previous != nil && !b.unsupported {
		used := b.previous.SwapUsedBytes
		s.SwapUsedBytes = &used
	}
	if b.previous != nil {
		s.SampleAgeSeconds = b.now().Sub(b.previous.At).Seconds()
	}
	if b.stale() {
		s.Pressure = Unknown
		if b.previous != nil {
			s.Reason = "sample_stale"
		}
		s.Credit = 0
		s.Limit = 0
		s.SwapoutRate = nil
		s.SwapUsedBytes = nil
	}
	if b.switching {
		s.Limit, s.Credit = 0, 0
		s.Reason = "host_switch_draining"
	}
	s.UnresolvedResidents = b.unresolvedResidents
	if b.unresolvedResidents != 0 {
		s.Limit, s.Credit = 0, 0
		s.Reason = "resident_owner_readback_required"
	}
	s.OwnedAllocations = b.active
	if b.shared != nil {
		shared := b.shared.snapshot()
		s.SharedQuota = &shared
		if shared.Error == "" {
			s.Allocations = shared.Allocations
			s.Residents = shared.Residents
			s.Active = shared.Allocations - shared.Residents
			s.OwnedAllocations = shared.Owned
		}
		if len(b.pendingSharedReturns) != 0 && shared.Error == "" {
			shared.Error = b.sharedError
		}
		if shared.Error != "" {
			s.Limit, s.Credit = 0, 0
			s.Reason = "shared_quota_unknown"
		}
		if b.sharedError != "" {
			message := b.sharedError
			s.SampleError = &message
		}
	}
	return s
}

func (b *Budget) TryAcquireResident(id string) (*Permit, error) {
	return b.tryAcquire(id, true, "")
}

// Only an unreturned reservation or a joined worker may enter this return set.
// The original Host sampler retries the same nonce, never another dispatch.
func (b *Budget) rememberSharedReturn(id string) {
	if b.pendingSharedReturns == nil {
		b.pendingSharedReturns = make(map[string]bool)
	}
	b.pendingSharedReturns[id] = true
}
func (b *Budget) returnSharedAllocation(id string) {
	if err := b.shared.release(id); err != nil {
		b.rememberSharedReturn(id)
		b.sharedError = err.Error()
		return
	}
	delete(b.pendingSharedReturns, id)
	if len(b.pendingSharedReturns) == 0 {
		b.sharedError = ""
	}
}
