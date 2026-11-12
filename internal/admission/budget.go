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
	DefaultWorkerLimit             = 9
	WorkerAdmissionPolicy          = "host-workers-v1"
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
	SwapUsedBytes       *uint64              `json:"swap_used_bytes"`
	SwapoutRate         *float64             `json:"swapout_bytes_per_second"`
	SampleAgeSeconds    float64              `json:"sample_age_seconds"`
	Unsupported         bool                 `json:"unsupported_monitor"`
}

type Budget struct {
	shared               *sharedQuota
	sharedError          string
	pendingSharedReturns map[string]bool
	unresolvedResidents  int
	residentScopes       map[string]int
	switching            bool
	mu                   sync.Mutex
	maximum, active      int
	residents            int
	previous             *Sample
	pressure             Pressure
	rate                 float64
	rateValid            bool
	unsupported          bool
	sampleError          *string
	contexts             map[string]bool
	now                  func() time.Time
}

func (b *Budget) SetUnresolvedResidents(count int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unresolvedResidents = count
}

// AdmissionError reports custody faults independently of worker capacity and observations.
func (b *Budget) AdmissionError() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.unresolvedResidents != 0 {
		return "resident ownership requires native owner readback"
	}
	if len(b.pendingSharedReturns) != 0 {
		return "shared quota return requires original nonce reconciliation: " + b.sharedError
	}
	if b.shared != nil {
		if current := b.shared.snapshot(); current.Error != "" {
			return current.Error
		}
	}
	return b.sharedError
}

// HoldForSwitch stops new workers without changing live owners or task pause state.
// Shared proof can still admit services needed by a worker in the old round.
func (b *Budget) HoldForSwitch() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.switching = true
}

type allocation struct {
	sharedID string
	budget   *Budget
	refs     int
	resident bool
	scope    string
}
type Permit struct {
	parent              *Permit
	allocation          *allocation
	once                sync.Once
	released            bool
	replacementReserved bool
}

func NewBudget(maxWorkers int) *Budget {
	if maxWorkers <= 0 || maxWorkers > DefaultWorkerLimit {
		maxWorkers = DefaultWorkerLimit
	}
	return &Budget{maximum: maxWorkers, pressure: Unknown, contexts: map[string]bool{}, now: time.Now}
}

func (b *Budget) Register(id string) { b.mu.Lock(); defer b.mu.Unlock(); b.contexts[id] = true }
func (b *Budget) Unregister(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.contexts, id)
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
	b.rateValid = false
	b.sampleError = nil
	if err != nil {
		message := err.Error()
		b.sampleError = &message
	}
	if err != nil || s.At.IsZero() {
		b.pressure = Unknown
		b.previous = nil
		return
	}
	previous := b.previous
	b.previous = &s
	b.unsupported = s.Unsupported
	b.pressure = s.Pressure
	if s.Unsupported {
		b.pressure = Unknown
		return
	}
	if previous == nil || !s.At.After(previous.At) || s.At.Sub(previous.At) > MaxSampleAge || s.Swapouts < previous.Swapouts || previous.Unsupported {
		return
	}
	b.rate = float64(s.Swapouts-previous.Swapouts) / s.At.Sub(previous.At).Seconds()
	b.rateValid = true
}

func (b *Budget) stale() bool {
	return b.previous == nil || b.now().Sub(b.previous.At) > MaxSampleAge || b.now().Before(b.previous.At)
}

func (b *Budget) TryAcquire(id string) (*Permit, error) { return b.tryAcquire(id, false, "", "") }
func (b *Budget) TryAcquireTask(id, order string) (*Permit, error) {
	return b.tryAcquire(id, false, "", order)
}
func (b *Budget) tryAcquire(id string, resident bool, residentKey, order string) (*Permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pendingSharedReturns) != 0 || !b.contexts[id] || b.unresolvedResidents != 0 || (b.switching && (!resident || b.shared == nil || !b.shared.workerContext(id))) {
		return nil, ErrDeferred
	}
	if !resident && b.active-b.residents >= b.maximum {
		return nil, ErrDeferred
	}
	sharedID := ""
	if b.shared != nil {
		var err error
		sharedID, err = b.shared.acquire(id, b.maximum, resident, residentKey, b.switching)
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
		if b.residentScopes == nil {
			b.residentScopes = make(map[string]int)
		}
		b.residentScopes[residentKey]++
	}
	return &Permit{allocation: &allocation{budget: b, refs: 1, sharedID: sharedID, resident: resident, scope: residentKey}}, nil
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
				b.residentScopes[p.allocation.scope]--
			}
		}
	})
}

func (p *Permit) ReserveReplacement(id string) (*Permit, error) {
	b := p.allocation.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pendingSharedReturns) != 0 || p.released || p.replacementReserved || !b.contexts[id] || b.unresolvedResidents != 0 || b.switching {
		return nil, ErrDeferred
	}
	if b.shared != nil {
		if err := b.shared.replacement(p.allocation.sharedID); err != nil {
			if !errors.Is(err, ErrDeferred) {
				b.sharedError = err.Error()
			}
			return nil, ErrDeferred
		}
		b.sharedError = ""
	}
	p.replacementReserved = true
	p.allocation.refs++
	return &Permit{allocation: p.allocation, parent: p}, nil
}

func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{AdmissionPolicy: WorkerAdmissionPolicy, Pressure: b.pressure, Reason: "ready", Active: b.active - b.residents, Residents: b.residents, Allocations: b.active, Maximum: b.maximum, Limit: b.maximum, Unsupported: b.unsupported}
	s.SampleAgeSeconds = -1
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
		s.SwapoutRate = nil
		s.SwapUsedBytes = nil
	}
	if b.switching {
		s.Limit = 0
		s.Reason = "host_switch_draining"
	}
	s.UnresolvedResidents = b.unresolvedResidents
	if b.unresolvedResidents != 0 {
		s.Limit = 0
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
			s.Limit = 0
			s.Reason = "shared_quota_unknown"
		}
	}
	if s.Limit > 0 && s.Active >= s.Maximum {
		s.Reason = "worker_limit"
	}
	return s
}

func (b *Budget) TryAcquireResident(id string) (*Permit, error) {
	return b.tryAcquire(id, true, "", "")
}

func (b *Budget) TryAcquireScopedResident(id, scope string) (*Permit, error) {
	return b.tryAcquire(id, true, scope, "")
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
