package admission

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Pressure string

const (
	Unknown       Pressure = "unknown"
	Normal        Pressure = "normal"
	Warning       Pressure = "warning"
	Critical      Pressure = "critical"
	PollInterval           = 5 * time.Second
	SampleTimeout          = time.Second
	MaxSampleAge           = 15 * time.Second
)

var ErrDeferred = errors.New("worker resource admission deferred")

type Sample struct {
	At               time.Time
	Pressure         Pressure
	Swapouts         uint64 // cumulative bytes
	TotalMemoryBytes uint64
	CPUs             int
	Unsupported      bool
}

type Sampler interface {
	Read(context.Context) (Sample, error)
}

type Snapshot struct {
	UnresolvedResidents int      `json:"unresolved_residents"`
	Pressure            Pressure `json:"pressure"`
	SampleError         *string  `json:"sample_error"`
	Reason              string   `json:"reason"`
	Active              int      `json:"active_workers"`
	Residents           int      `json:"active_residents"`
	Allocations         int      `json:"active_allocations"`
	Maximum             int      `json:"maximum_workers"`
	Limit               int      `json:"effective_limit"`
	Credit              int      `json:"start_credit"`
	StableSamples       int      `json:"stable_samples"`
	SwapoutRate         *float64 `json:"swapout_bytes_per_second"`
	SampleAgeSeconds    float64  `json:"sample_age_seconds"`
	Unsupported         bool     `json:"unsupported_monitor"`
}

type Budget struct {
	unresolvedResidents                    int
	warningStable                          int
	warningGrants                          map[warningTask]WarningGrant
	gatewayWarnings                        map[warningTask]GatewayWarningGrant
	residentScopes                         map[string]int
	mu                                     sync.Mutex
	maximum, active, limit, credit, stable int
	residents                              int
	auto                                   bool
	previous                               *Sample
	pressure                               Pressure
	reason                                 string
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

type allocation struct {
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
	auto := maxWorkers <= 0
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	return &Budget{maximum: maxWorkers, auto: auto, pressure: Unknown, reason: "awaiting_sample", contexts: map[string]bool{}, waited: map[string]time.Time{}, now: time.Now}
}

func (b *Budget) Register(id string) { b.mu.Lock(); defer b.mu.Unlock(); b.contexts[id] = true }
func (b *Budget) Unregister(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.contexts, id)
	for task, grant := range b.warningGrants {
		if task.context == id && grant.Status == "available" {
			grant.Status = "revoked"
			b.warningGrants[task] = grant
		}
	}
	for task, grant := range b.gatewayWarnings {
		if task.context == id && grant.Status == "available" {
			grant.Status = "revoked"
			b.gatewayWarnings[task] = grant
		}
	}
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
	previousWarningStable := b.warningStable
	b.warningStable = 0
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
		b.reason = "unsupported_monitor_static_limit"
		b.limit = b.maximum
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
	if s.Pressure == Warning && b.rate == 0 {
		b.warningStable = previousWarningStable + 1
	}
	if s.Pressure != Normal || b.rate > 0 {
		b.reason = "memory_pressure"
		if b.rate > 0 {
			b.reason = "swapout_rate"
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

func (b *Budget) TryAcquire(id string) (*Permit, error) {
	return b.tryAcquire(id, "")
}

func (b *Budget) TryAcquireTask(id, order string) (*Permit, error) {
	return b.tryAcquire(id, order)
}

func (b *Budget) tryAcquire(id, order string) (*Permit, error) {
	return b.acquire(id, order, false, "")
}

func (b *Budget) acquire(id, order string, resident bool, scope string) (*Permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.contexts[id] || b.unresolvedResidents != 0 {
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
	exception := b.warningAvailable(id, order)
	gatewayAllowed := b.gatewayWarningAvailable(id, order, resident, scope)
	exception = exception || gatewayAllowed
	if !exception && (b.stale() || b.active >= b.limit || (!b.unsupported && b.credit == 0) || len(b.waiters) == 0 || b.waiters[0] != id) {
		return nil, ErrDeferred
	}
	b.active++
	b.consumeWarningGrant(id, order)
	if resident {
		b.residents++
		if b.residentScopes == nil {
			b.residentScopes = make(map[string]int)
		}
		b.residentScopes[scope]++
		b.markGatewayStarted(id, scope)
	} else if grant, ok := b.gatewayWarnings[warningTask{id, order}]; ok && b.gatewayWarningStatus(grant).Status == "available" {
		grant.Status = "consumed"
		b.gatewayWarnings[warningTask{id, order}] = grant
	}
	if !b.unsupported {
		b.credit = 0
	}
	b.removeWaiter(id)
	delete(b.waited, id)
	return &Permit{allocation: &allocation{budget: b, refs: 1, resident: resident, scope: scope}}, nil
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
	if p.released || p.replacementReserved || !b.contexts[id] || b.unresolvedResidents != 0 || b.stale() || b.limit == 0 || b.active > b.limit || (!b.unsupported && b.credit == 0) {
		return nil, ErrDeferred
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
	s := Snapshot{Pressure: b.pressure, Reason: b.reason, Active: b.active - b.residents, Residents: b.residents, Allocations: b.active, Maximum: b.maximum, Limit: b.limit, Credit: b.credit, StableSamples: b.stable, Unsupported: b.unsupported}
	if b.sampleError != nil {
		message := *b.sampleError
		s.SampleError = &message
	}
	if b.rateValid {
		rate := b.rate
		s.SwapoutRate = &rate
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
	}
	s.UnresolvedResidents = b.unresolvedResidents
	if b.unresolvedResidents != 0 {
		s.Limit, s.Credit = 0, 0
		s.Reason = "resident_owner_readback_required"
	}
	return s
}

func (b *Budget) TryAcquireResident(id string) (*Permit, error) {
	return b.acquire(id, "", true, "")
}

func (b *Budget) TryAcquireScopedResident(id, scope string) (*Permit, error) {
	return b.acquire(id, "", true, scope)
}
