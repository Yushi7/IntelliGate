package circuitbreaker

import (
	"context"
	"errors"
	"sync"
	"time"
)

type State uint8

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

type Config struct {
	FailureThreshold         int
	OpenCooldown             time.Duration
	HalfOpenMaxRequests      int
	HalfOpenSuccessThreshold int
}

func DefaultConfig() Config {
	return Config{
		FailureThreshold:         5,
		OpenCooldown:             10 * time.Second,
		HalfOpenMaxRequests:      1,
		HalfOpenSuccessThreshold: 1,
	}
}

type Breaker struct {
	mu                  sync.Mutex
	config              Config
	state               State
	consecutiveFailures int
	openedAt            time.Time
	halfOpenInFlight    int
	halfOpenSuccesses   int
	generation          uint64
	now                 func() time.Time
}

func New(config Config) (*Breaker, error) {
	if config.FailureThreshold <= 0 {
		return nil, errors.New("circuit-breaker failure threshold must be positive")
	}
	if config.OpenCooldown <= 0 {
		return nil, errors.New("circuit-breaker open cooldown must be positive")
	}
	if config.HalfOpenMaxRequests <= 0 {
		return nil, errors.New("circuit-breaker half-open max requests must be positive")
	}
	if config.HalfOpenSuccessThreshold <= 0 {
		return nil, errors.New("circuit-breaker half-open success threshold must be positive")
	}

	return newWithClock(config, time.Now), nil
}

func newWithClock(config Config, now func() time.Time) *Breaker {
	return &Breaker{
		config: config,
		state:  StateClosed,
		now:    now,
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advanceCooldownLocked()
	return b.state
}

func (b *Breaker) TryAcquire() (*Permit, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advanceCooldownLocked()

	switch b.state {
	case StateClosed:
		return &Permit{breaker: b, generation: b.generation}, true
	case StateHalfOpen:
		if b.halfOpenSuccesses >= b.config.HalfOpenSuccessThreshold ||
			b.halfOpenInFlight >= b.config.HalfOpenMaxRequests {
			return nil, false
		}
		b.halfOpenInFlight++
		return &Permit{breaker: b, generation: b.generation}, true
	default:
		return nil, false
	}
}

func (b *Breaker) advanceCooldownLocked() {
	if b.state == StateOpen && !b.now().Before(b.openedAt.Add(b.config.OpenCooldown)) {
		b.state = StateHalfOpen
		b.halfOpenInFlight = 0
		b.halfOpenSuccesses = 0
		b.generation++
	}
}

func (b *Breaker) finish(generation uint64, result permitResult) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if generation != b.generation {
		return
	}

	switch b.state {
	case StateClosed:
		switch result {
		case resultFailure:
			b.consecutiveFailures++
			if b.consecutiveFailures >= b.config.FailureThreshold {
				b.openLocked()
			}
		case resultSuccess:
			b.consecutiveFailures = 0
		}
	case StateHalfOpen:
		if b.halfOpenInFlight > 0 {
			b.halfOpenInFlight--
		}
		switch result {
		case resultFailure:
			b.openLocked()
		case resultSuccess:
			b.halfOpenSuccesses++
		}
		if b.state == StateHalfOpen &&
			b.halfOpenSuccesses >= b.config.HalfOpenSuccessThreshold &&
			b.halfOpenInFlight == 0 {
			b.closeLocked()
		}
	}
}

func (b *Breaker) openLocked() {
	b.state = StateOpen
	b.openedAt = b.now()
	b.consecutiveFailures = 0
	b.halfOpenInFlight = 0
	b.halfOpenSuccesses = 0
	b.generation++
}

func (b *Breaker) closeLocked() {
	b.state = StateClosed
	b.consecutiveFailures = 0
	b.halfOpenInFlight = 0
	b.halfOpenSuccesses = 0
	b.generation++
}

type permitResult uint8

const (
	resultSuccess permitResult = iota
	resultFailure
	resultAbandoned
)

type Permit struct {
	breaker    *Breaker
	generation uint64
	once       sync.Once
}

func (p *Permit) Success() {
	p.complete(resultSuccess)
}

func (p *Permit) Failure() {
	p.complete(resultFailure)
}

func (p *Permit) Abandon() {
	p.complete(resultAbandoned)
}

func (p *Permit) complete(result permitResult) {
	if p == nil || p.breaker == nil {
		return
	}
	p.once.Do(func() {
		p.breaker.finish(p.generation, result)
	})
}

type permitContextKey struct{}

func WithPermit(ctx context.Context, permit *Permit) context.Context {
	return context.WithValue(ctx, permitContextKey{}, permit)
}

func PermitFromContext(ctx context.Context) *Permit {
	permit, _ := ctx.Value(permitContextKey{}).(*Permit)
	return permit
}
