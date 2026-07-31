package engine

import (
	"math/rand"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/model"
)

type zeroSource struct{}

func (zeroSource) Int63() int64 { return 0 }
func (zeroSource) Seed(int64)   {}

func TestRetryPolicyMinimumJitterTracksRequiredBaseSchedule(t *testing.T) {
	policy := NewRetryPolicy(zeroSource{})
	wants := []time.Duration{
		4 * time.Second,
		12 * time.Second,
		24 * time.Second,
		48 * time.Second,
		96 * time.Second,
		192 * time.Second,
		240 * time.Second,
		240 * time.Second,
	}

	for attempt, want := range wants {
		decision := policy.Next(uint64(attempt), &model.CodeError{Code: "temporary", Message: "temporary failure"})
		if decision.Mode != RetryAfter {
			t.Fatalf("attempt %d: mode = %q, want %q", attempt, decision.Mode, RetryAfter)
		}
		if decision.Delay != want {
			t.Fatalf("attempt %d: delay = %v, want %v", attempt, decision.Delay, want)
		}
	}
}

func TestRetryPolicyJitterStaysWithinBoundsAndFiveMinuteCap(t *testing.T) {
	bases := []time.Duration{
		5 * time.Second,
		15 * time.Second,
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		300 * time.Second,
		300 * time.Second,
	}

	for seed := int64(0); seed < 100; seed++ {
		policy := NewRetryPolicy(rand.NewSource(seed))
		for attempt, base := range bases {
			decision := policy.Next(uint64(attempt), &model.CodeError{Code: "temporary", Message: "temporary failure"})
			minimum := base - base/5
			maximum := base + base/5
			if maximum > MaxRetryDelay {
				maximum = MaxRetryDelay
			}
			if decision.Delay < minimum || decision.Delay > maximum {
				t.Fatalf("seed %d attempt %d: delay %v outside [%v, %v]", seed, attempt, decision.Delay, minimum, maximum)
			}
			if decision.Delay > 5*time.Minute {
				t.Fatalf("seed %d attempt %d: delay %v exceeds five-minute cap", seed, attempt, decision.Delay)
			}
		}
	}
}

func TestRetryPolicyInjectedSourceIsDeterministic(t *testing.T) {
	first := NewRetryPolicy(rand.NewSource(99))
	second := NewRetryPolicy(rand.NewSource(99))
	failure := &model.CodeError{Code: "temporary", Message: "temporary failure"}

	for attempt := uint64(0); attempt < 20; attempt++ {
		got := first.Next(attempt, failure)
		want := second.Next(attempt, failure)
		if got != want {
			t.Fatalf("attempt %d: first = %#v, second = %#v", attempt, got, want)
		}
	}
}

func TestRetryPolicyPermanentFailuresNeverUseAutomaticRetry(t *testing.T) {
	policy := NewRetryPolicy(rand.NewSource(11))
	for _, code := range []string{ErrorCodeAuthentication, ErrorCodeInvalidConfig, ErrorCodeUnsupportedOption} {
		decision := policy.Next(0, &model.CodeError{Code: code, Message: "permanent failure"})
		if decision != (RetryDecision{Mode: RetryNone}) {
			t.Fatalf("code %q: decision = %#v, want no retry", code, decision)
		}
	}
}

func TestRetryPolicyWANDownWaitsForEventWithoutTimer(t *testing.T) {
	policy := NewRetryPolicy(rand.NewSource(12))
	decision := policy.Next(0, &model.CodeError{Code: ErrorCodeWANDown, Message: "WAN unavailable"})
	if decision != (RetryDecision{Mode: RetryOnWANEvent}) {
		t.Fatalf("decision = %#v, want WAN-event retry", decision)
	}
}

func TestRetryPolicyNilFailureDoesNotRetry(t *testing.T) {
	policy := NewRetryPolicy(rand.NewSource(13))
	if got := policy.Next(0, nil); got != (RetryDecision{Mode: RetryNone}) {
		t.Fatalf("decision = %#v, want no retry", got)
	}
}

func TestRetryPolicyIsSafeForConcurrentCallers(t *testing.T) {
	policy := NewRetryPolicy(rand.NewSource(14))
	failure := &model.CodeError{Code: "temporary", Message: "temporary failure"}
	const callers = 128
	var wg sync.WaitGroup
	errors := make(chan time.Duration, callers)
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision := policy.Next(0, failure)
			if decision.Mode != RetryAfter || decision.Delay < 4*time.Second || decision.Delay > 6*time.Second {
				errors <- decision.Delay
			}
		}()
	}
	wg.Wait()
	close(errors)
	for delay := range errors {
		t.Errorf("concurrent delay = %v, want [4s, 6s]", delay)
	}
}

func TestRetryPolicyHugeAttemptDoesNotOverflow(t *testing.T) {
	policy := NewRetryPolicy(zeroSource{})
	decision := policy.Next(^uint64(0), &model.CodeError{Code: "temporary", Message: "temporary failure"})
	if decision.Mode != RetryAfter || decision.Delay != 4*time.Minute {
		t.Fatalf("decision = %#v, want four-minute minimum jitter at cap", decision)
	}
}
