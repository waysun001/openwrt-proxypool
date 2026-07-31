package engine

import (
	"math/rand"
	"sync"
	"time"

	"proxypoold/internal/model"
)

const MaxRetryDelay = 5 * time.Minute

type RetryMode string

const (
	RetryNone       RetryMode = "none"
	RetryAfter      RetryMode = "timer"
	RetryOnWANEvent RetryMode = "wan_event"
)

type RetryDecision struct {
	Mode  RetryMode     `json:"mode"`
	Delay time.Duration `json:"delay,omitempty"`
}

// RetryPolicy is safe for concurrent callers even though rand.Source is not.
type RetryPolicy struct {
	mu   sync.Mutex
	rand *rand.Rand
}

func NewRetryPolicy(source rand.Source) *RetryPolicy {
	if source == nil {
		source = rand.NewSource(time.Now().UnixNano())
	}
	return &RetryPolicy{rand: rand.New(source)}
}

func (p *RetryPolicy) Next(attempt uint64, failure *model.CodeError) RetryDecision {
	if failure == nil {
		return RetryDecision{Mode: RetryNone}
	}
	switch failure.Code {
	case ErrorCodeAuthentication, ErrorCodeInvalidConfig, ErrorCodeUnsupported, ErrorCodeStopTimeout:
		return RetryDecision{Mode: RetryNone}
	case ErrorCodeWANDown:
		return RetryDecision{Mode: RetryOnWANEvent}
	}

	base := retryBaseDelay(attempt)
	minimum := base - base/5
	maximum := base + base/5
	if maximum > MaxRetryDelay {
		maximum = MaxRetryDelay
	}

	p.mu.Lock()
	offset := p.rand.Int63n(int64(maximum-minimum) + 1)
	p.mu.Unlock()
	return RetryDecision{Mode: RetryAfter, Delay: minimum + time.Duration(offset)}
}

func retryBaseDelay(attempt uint64) time.Duration {
	switch attempt {
	case 0:
		return 5 * time.Second
	case 1:
		return 15 * time.Second
	case 2:
		return 30 * time.Second
	case 3:
		return 60 * time.Second
	case 4:
		return 120 * time.Second
	case 5:
		return 240 * time.Second
	default:
		return MaxRetryDelay
	}
}
