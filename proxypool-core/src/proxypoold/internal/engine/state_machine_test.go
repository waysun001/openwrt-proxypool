package engine

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

var stateTestEpoch = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func TestMachineLegalTransitionsExerciseEveryState(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(1)))
	now := stateTestEpoch
	generation := uint64(7)

	steps := []struct {
		kind EventKind
		want model.RuntimeState
		err  *model.CodeError
	}{
		{EventEnable, model.StateQueued, nil},
		{EventStart, model.StateStarting, nil},
		{EventStarted, model.StateValidating, nil},
		{EventValidated, model.StateOnline, nil},
		{EventDegraded, model.StateDegraded, &model.CodeError{Code: "health_check_failed", Message: "health check failed"}},
		{EventHealthy, model.StateOnline, nil},
		{EventRecover, model.StateRecovering, nil},
		{EventRecovered, model.StateQueued, nil},
		{EventStart, model.StateStarting, nil},
		{EventFailure, model.StateFailed, &model.CodeError{Code: ErrorCodeAuthentication, Message: "credentials rejected"}},
		{EventManualReconnect, model.StateQueued, nil},
		{EventStart, model.StateStarting, nil},
		{EventStop, model.StateStopping, nil},
		{EventStopped, model.StateDisabled, nil},
	}

	seen := map[model.RuntimeState]bool{model.StateDisabled: true}
	for index, step := range steps {
		now = now.Add(time.Second)
		status, err := machine.Apply(Event{
			NodeID:     "node-a",
			JobID:      "job-a",
			Generation: generation,
			Kind:       step.kind,
			Err:        step.err,
			At:         now,
		})
		if err != nil {
			t.Fatalf("step %d (%s): Apply() error = %v", index, step.kind, err)
		}
		if step.kind == EventRecover || step.kind == EventRecovered || step.kind == EventManualReconnect || step.kind == EventStop {
			generation++
		}
		if status.State != step.want {
			t.Fatalf("step %d (%s): state = %q, want %q", index, step.kind, status.State, step.want)
		}
		if status.Generation != generation {
			t.Fatalf("step %d (%s): generation = %d, want %d", index, step.kind, status.Generation, generation)
		}
		seen[status.State] = true
	}

	for _, state := range []model.RuntimeState{
		model.StateDisabled,
		model.StateQueued,
		model.StateStarting,
		model.StateValidating,
		model.StateOnline,
		model.StateDegraded,
		model.StateStopping,
		model.StateFailed,
		model.StateBackoff,
		model.StateRecovering,
	} {
		if state == model.StateBackoff {
			continue // Exercised explicitly by TestMachineTimeoutEntersBackoff.
		}
		if !seen[state] {
			t.Errorf("state %q was not exercised", state)
		}
	}
}

func TestMachineDropsStaleCompletionWithoutChangingNewGeneration(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(2)))
	applyMachineEvent(t, machine, "node-a", 8, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 8, EventStart, nil, stateTestEpoch.Add(time.Second))
	before := applyMachineEvent(t, machine, "node-a", 8, EventStarted, nil, stateTestEpoch.Add(2*time.Second))

	got, err := machine.Apply(Event{
		NodeID:     "node-a",
		JobID:      "job-a",
		Generation: 7,
		Kind:       EventValidated,
		At:         stateTestEpoch.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("Apply(stale completion) error = %v", err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("stale completion changed status:\n got: %#v\nwant: %#v", got, before)
	}
}

func TestMachineAcceptsCurrentOperationCompletionAfterWallClockMovesBackward(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(16)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch.Add(time.Hour))
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Hour+time.Second))

	status := applyMachineEvent(t, machine, "node-a", 1, EventStarted, nil, stateTestEpoch.Add(-time.Hour))
	if status.State != model.StateValidating {
		t.Fatalf("state = %q, want %q despite wall-clock correction", status.State, model.StateValidating)
	}
	if !status.UpdatedAt.Equal(stateTestEpoch.Add(-time.Hour)) {
		t.Fatalf("UpdatedAt = %v, want observation timestamp retained for display", status.UpdatedAt)
	}
}

func TestMachinePermanentFailureEntersFailedWithoutRetry(t *testing.T) {
	for _, code := range []string{ErrorCodeAuthentication, ErrorCodeInvalidConfig, ErrorCodeUnsupported} {
		t.Run(code, func(t *testing.T) {
			machine := NewMachine(NewRetryPolicy(rand.NewSource(3)))
			applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
			applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))

			status := applyMachineEvent(t, machine, "node-a", 1, EventFailure, &model.CodeError{Code: code, Message: "permanent failure"}, stateTestEpoch.Add(2*time.Second))
			if status.State != model.StateFailed {
				t.Fatalf("state = %q, want %q", status.State, model.StateFailed)
			}
			if !status.RetryAt.IsZero() {
				t.Fatalf("RetryAt = %v, want zero", status.RetryAt)
			}
			if status.Attempts != 1 {
				t.Fatalf("Attempts = %d, want 1", status.Attempts)
			}
		})
	}
}

func TestMachinePublishesSafeSpecificL2TPFailures(t *testing.T) {
	tests := []struct {
		code    string
		message string
	}{
		{ErrorCodeL2TPInterfaceFailed, "L2TP interface creation failed"},
		{ErrorCodeL2TPDaemonFailed, "L2TP service failed"},
		{ErrorCodeL2TPNegotiationFailed, "L2TP negotiation failed"},
		{ErrorCodeL2TPNoAddress, "L2TP did not receive an IPv4 address"},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			machine := NewMachine(NewRetryPolicy(rand.NewSource(31)))
			applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
			applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
			status := applyMachineEvent(t, machine, "node-a", 1, EventFailure,
				&model.CodeError{Code: test.code, Message: "unsafe adapter detail"}, stateTestEpoch.Add(2*time.Second))
			if status.LastError == nil || status.LastError.Code != test.code || status.LastError.Message != test.message {
				t.Fatalf("LastError = %#v", status.LastError)
			}
		})
	}
}

func TestMachineTimeoutEntersBackoff(t *testing.T) {
	timedOutAt := stateTestEpoch.Add(2 * time.Second)
	clock := &manualClock{wall: timedOutAt}
	machine := NewMachine(NewRetryPolicy(zeroSource{}), WithClock(clock))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))

	status := applyMachineEvent(t, machine, "node-a", 1, EventTimeout, nil, timedOutAt)
	if status.State != model.StateBackoff {
		t.Fatalf("state = %q, want %q", status.State, model.StateBackoff)
	}
	if status.LastError == nil || status.LastError.Code != ErrorCodeConnectTimeout {
		t.Fatalf("LastError = %#v, want connect_timeout", status.LastError)
	}
	if want := timedOutAt.Add(4 * time.Second); !status.RetryAt.Equal(want) {
		t.Fatalf("RetryAt = %v, want %v", status.RetryAt, want)
	}
}

func TestMachineWANDownWaitsForWANEvent(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(4)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	status := applyMachineEvent(t, machine, "node-a", 1, EventFailure, &model.CodeError{Code: ErrorCodeWANDown, Message: "WAN unavailable"}, stateTestEpoch.Add(2*time.Second))

	if status.State != model.StateBackoff || !status.RetryAt.IsZero() {
		t.Fatalf("WAN failure status = %#v, want backoff with no timer", status)
	}
	before := status
	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 1, Kind: EventRetryDue, At: stateTestEpoch.Add(time.Hour)})
	assertInternalError(t, err)
	after, _ := machine.Status("node-a")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("timer retry changed WAN-waiting status:\n got: %#v\nwant: %#v", after, before)
	}

	status = applyMachineEvent(t, machine, "node-a", 1, EventWANAvailable, nil, stateTestEpoch.Add(time.Hour))
	if status.State != model.StateQueued {
		t.Fatalf("state after WAN event = %q, want %q", status.State, model.StateQueued)
	}
	if status.Generation != 2 {
		t.Fatalf("generation after WAN retry = %d, want 2", status.Generation)
	}
}

func TestMachineRetryDueUsesMonotonicClockInsteadOfEventTimestamp(t *testing.T) {
	clock := &manualClock{wall: stateTestEpoch}
	machine := NewMachine(NewRetryPolicy(zeroSource{}), WithClock(clock))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	before := applyMachineEvent(t, machine, "node-a", 1, EventTimeout, nil, stateTestEpoch.Add(2*time.Second))

	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 1, Kind: EventRetryDue, At: stateTestEpoch.Add(24 * time.Hour)})
	assertInternalError(t, err)
	afterEarly, _ := machine.Status("node-a")
	if !reflect.DeepEqual(afterEarly, before) {
		t.Fatal("forward wall-clock jump released retry before monotonic delay")
	}

	clock.advance(4 * time.Second)
	status := applyMachineEvent(t, machine, "node-a", 1, EventRetryDue, nil, stateTestEpoch.Add(-24*time.Hour))
	if status.State != model.StateQueued || status.Generation != 2 {
		t.Fatalf("status = %#v, want queued generation 2 after monotonic delay", status)
	}
}

func TestMachineManualReconnectCreatesNewGeneration(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(5)))
	applyMachineEvent(t, machine, "node-a", 41, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 41, EventStart, nil, stateTestEpoch.Add(time.Second))
	applyMachineEvent(t, machine, "node-a", 41, EventFailure, &model.CodeError{Code: ErrorCodeAuthentication, Message: "bad password"}, stateTestEpoch.Add(2*time.Second))

	status := applyMachineEvent(t, machine, "node-a", 41, EventManualReconnect, nil, stateTestEpoch.Add(3*time.Second))
	if status.Generation != 42 || status.State != model.StateQueued {
		t.Fatalf("status = %#v, want generation 42 queued", status)
	}
	if status.Attempts != 0 || status.LastError != nil || !status.RetryAt.IsZero() {
		t.Fatalf("manual reconnect did not clear failure state: %#v", status)
	}
}

func TestMachineManualReconnectRejectsGenerationOverflowWithoutMutation(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(6)))
	before := applyMachineEvent(t, machine, "node-a", math.MaxUint64, EventEnable, nil, stateTestEpoch)

	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-b", Generation: math.MaxUint64, Kind: EventManualReconnect, At: stateTestEpoch.Add(time.Second)})
	assertInternalError(t, err)
	after, _ := machine.Status("node-a")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("overflow changed status:\n got: %#v\nwant: %#v", after, before)
	}
}

func TestMachineActiveReconnectReservesBothGenerationsAtRequest(t *testing.T) {
	tests := []struct {
		name       string
		generation uint64
		accepted   bool
	}{
		{name: "max minus two", generation: math.MaxUint64 - 2, accepted: true},
		{name: "max minus one", generation: math.MaxUint64 - 1, accepted: false},
		{name: "max", generation: math.MaxUint64, accepted: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			machine := NewMachine(NewRetryPolicy(zeroSource{}))
			before := nodeRecord{status: NodeStatus{
				NodeID: "node-a", JobID: "owner-job", Generation: test.generation,
				State: model.StateStarting, UpdatedAt: stateTestEpoch,
			}}
			machine.nodes["node-a"] = before

			stopping, err := machine.Apply(Event{
				NodeID: "node-a", JobID: "reconnect-job", Generation: test.generation,
				Kind: EventManualReconnect, At: stateTestEpoch.Add(time.Second),
			})
			if !test.accepted {
				assertInternalError(t, err)
				if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, before) {
					t.Fatalf("rejected active reconnect mutated record:\n got: %#v\nwant: %#v", after, before)
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply(active reconnect) error = %v", err)
			}
			if stopping.State != model.StateStopping || stopping.Generation != math.MaxUint64-1 || !stopping.ReconnectPending {
				t.Fatalf("stopping status = %#v, want reserved barrier at max-1", stopping)
			}
			queued := applyMachineEventForJob(t, machine, "node-a", "reconnect-job", math.MaxUint64-1, EventStopped, nil, stateTestEpoch.Add(2*time.Second))
			if queued.State != model.StateQueued || queued.Generation != math.MaxUint64 || queued.JobID != "reconnect-job" {
				t.Fatalf("released status = %#v, want reconnect queued at max", queued)
			}
		})
	}
}

func TestMachineRecoveryReservesEntryAndReleaseGenerationsAtRequest(t *testing.T) {
	tests := []struct {
		name       string
		generation uint64
		accepted   bool
	}{
		{name: "max minus two", generation: math.MaxUint64 - 2, accepted: true},
		{name: "max minus one", generation: math.MaxUint64 - 1, accepted: false},
		{name: "max", generation: math.MaxUint64, accepted: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			machine := NewMachine(NewRetryPolicy(zeroSource{}))
			before := nodeRecord{
				status: NodeStatus{
					NodeID: "node-a", JobID: "owner-job", Generation: test.generation,
					State: model.StateOnline, UpdatedAt: stateTestEpoch,
				},
				onlineSinceElapsed: time.Minute,
				onlineSinceSet:     true,
			}
			machine.nodes["node-a"] = before

			recovering, err := machine.Apply(Event{
				NodeID: "node-a", JobID: "recovery-job", Generation: test.generation,
				Kind: EventRecover, At: stateTestEpoch.Add(time.Second),
			})
			if !test.accepted {
				assertInternalError(t, err)
				if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, before) {
					t.Fatalf("rejected recovery mutated record:\n got: %#v\nwant: %#v", after, before)
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply(recover) error = %v", err)
			}
			if recovering.State != model.StateRecovering || recovering.Generation != math.MaxUint64-1 {
				t.Fatalf("recovering status = %#v, want owner at max-1", recovering)
			}
			queued := applyMachineEventForJob(t, machine, "node-a", "recovery-job", math.MaxUint64-1, EventRecovered, nil, stateTestEpoch.Add(2*time.Second))
			if queued.State != model.StateQueued || queued.Generation != math.MaxUint64 || queued.JobID != "recovery-job" {
				t.Fatalf("released status = %#v, want recovery queued at max", queued)
			}
		})
	}
}

func TestMachinePendingReconnectReservesReleaseGenerationAtRegistration(t *testing.T) {
	ownerStates := []struct {
		name                string
		state               model.RuntimeState
		cleanupPending      bool
		resumeAfterRecovery bool
		release             EventKind
	}{
		{name: "stopping", state: model.StateStopping, release: EventStopped},
		{name: "ordinary recovery", state: model.StateRecovering, resumeAfterRecovery: true, release: EventRecovered},
		{name: "cleanup recovery", state: model.StateRecovering, cleanupPending: true, release: EventCleanupComplete},
	}
	boundaries := []struct {
		name       string
		generation uint64
		accepted   bool
	}{
		{name: "max minus two", generation: math.MaxUint64 - 2, accepted: true},
		{name: "max minus one", generation: math.MaxUint64 - 1, accepted: true},
		{name: "max", generation: math.MaxUint64, accepted: false},
	}

	for _, owner := range ownerStates {
		for _, boundary := range boundaries {
			t.Run(owner.name+"/"+boundary.name, func(t *testing.T) {
				machine := NewMachine(NewRetryPolicy(zeroSource{}))
				before := nodeRecord{
					status: NodeStatus{
						NodeID: "node-a", JobID: "owner-job", Generation: boundary.generation,
						State: owner.state, UpdatedAt: stateTestEpoch, CleanupPending: owner.cleanupPending,
					},
					resumeAfterRecovery: owner.resumeAfterRecovery,
				}
				machine.nodes["node-a"] = before

				pending, err := machine.Apply(Event{
					NodeID: "node-a", JobID: "reconnect-job", Generation: boundary.generation,
					Kind: EventManualReconnect, At: stateTestEpoch.Add(time.Second),
				})
				if !boundary.accepted {
					assertInternalError(t, err)
					if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, before) {
						t.Fatalf("rejected pending reconnect mutated record:\n got: %#v\nwant: %#v", after, before)
					}
					return
				}
				if err != nil {
					t.Fatalf("Apply(pending reconnect) error = %v", err)
				}
				if pending.Generation != boundary.generation || !pending.ReconnectPending || pending.State != owner.state {
					t.Fatalf("pending status = %#v, want owner token retained", pending)
				}
				storedPending := machine.nodes["node-a"]
				if storedPending.pendingJobID != "reconnect-job" || storedPending.resumeAfterRecovery {
					t.Fatalf("pending record = %#v, want exclusive reconnect intent", storedPending)
				}
				queued := applyMachineEventForJob(t, machine, "node-a", "owner-job", boundary.generation, owner.release, nil, stateTestEpoch.Add(2*time.Second))
				if queued.State != model.StateQueued || queued.Generation != boundary.generation+1 || queued.JobID != "reconnect-job" {
					t.Fatalf("released status = %#v, want reserved reconnect generation", queued)
				}
			})
		}
	}
}

func TestMachineOverflowingReconnectPreservesExistingStopIntent(t *testing.T) {
	for _, cleanupPending := range []bool{false, true} {
		name := "ordinary recovery"
		if cleanupPending {
			name = "cleanup recovery"
		}
		t.Run(name, func(t *testing.T) {
			machine := NewMachine(NewRetryPolicy(zeroSource{}))
			before := nodeRecord{
				status: NodeStatus{
					NodeID: "node-a", JobID: "owner-job", Generation: math.MaxUint64,
					State: model.StateRecovering, UpdatedAt: stateTestEpoch, CleanupPending: cleanupPending,
				},
				pendingJobID: "stop-job",
			}
			machine.nodes["node-a"] = before
			_, err := machine.Apply(Event{
				NodeID: "node-a", JobID: "reconnect-job", Generation: math.MaxUint64,
				Kind: EventManualReconnect, At: stateTestEpoch.Add(time.Second),
			})
			assertInternalError(t, err)
			if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, before) {
				t.Fatalf("overflowing reconnect replaced pending stop:\n got: %#v\nwant: %#v", after, before)
			}
		})
	}
}

func TestMachineStableOnlineResetsConsecutiveAttempts(t *testing.T) {
	clock := &manualClock{wall: stateTestEpoch}
	machine := NewMachine(NewRetryPolicy(zeroSource{}), WithClock(clock), WithStableOnlineWindow(30*time.Second))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	backoff := applyMachineEvent(t, machine, "node-a", 1, EventTimeout, nil, stateTestEpoch.Add(2*time.Second))
	clock.advance(4 * time.Second)
	retry := applyMachineEvent(t, machine, "node-a", 1, EventRetryDue, nil, backoff.RetryAt)
	if retry.Generation != 2 {
		t.Fatalf("automatic retry generation = %d, want 2", retry.Generation)
	}
	applyMachineEvent(t, machine, "node-a", 2, EventStart, nil, backoff.RetryAt.Add(time.Second))
	applyMachineEvent(t, machine, "node-a", 2, EventStarted, nil, backoff.RetryAt.Add(2*time.Second))
	online := applyMachineEvent(t, machine, "node-a", 2, EventValidated, nil, backoff.RetryAt.Add(3*time.Second))
	if online.Attempts != 1 {
		t.Fatalf("Attempts on initial online = %d, want 1 until stable", online.Attempts)
	}

	beforeEarly := online
	clock.advance(30*time.Second - time.Nanosecond)
	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 2, Kind: EventStableOnline, At: backoff.RetryAt.Add(32 * time.Second)})
	assertInternalError(t, err)
	afterEarly, _ := machine.Status("node-a")
	if !reflect.DeepEqual(afterEarly, beforeEarly) {
		t.Fatalf("premature stable event changed status:\n got: %#v\nwant: %#v", afterEarly, beforeEarly)
	}

	clock.advance(time.Nanosecond)
	stable := applyMachineEvent(t, machine, "node-a", 2, EventStableOnline, nil, backoff.RetryAt.Add(33*time.Second))
	if stable.Attempts != 0 {
		t.Fatalf("Attempts after stable online = %d, want 0", stable.Attempts)
	}
	if stable.LastError != nil || !stable.RetryAt.IsZero() {
		t.Fatalf("stable online retained failure metadata: %#v", stable)
	}
}

func TestMachineIllegalTransitionReturnsInternalAndDoesNotMutate(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(7)))
	before := applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)

	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 1, Kind: EventValidated, At: stateTestEpoch.Add(time.Second)})
	assertInternalError(t, err)
	after, _ := machine.Status("node-a")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("illegal transition changed status:\n got: %#v\nwant: %#v", after, before)
	}
}

func TestMachineStopTimeoutEntersCleanupBarrierBeforeDisabled(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	stopping := applyMachineEvent(t, machine, "node-a", 1, EventStop, nil, stateTestEpoch.Add(2*time.Second))

	barrier := applyMachineEvent(t, machine, "node-a", stopping.Generation, EventTimeout, nil, stateTestEpoch.Add(3*time.Second))
	if barrier.State != model.StateRecovering || !barrier.CleanupPending || barrier.ReconnectPending {
		t.Fatalf("stop timeout status = %#v, want cleanup-pending recovery", barrier)
	}
	if barrier.LastError == nil || barrier.LastError.Code != ErrorCodeStopTimeout || !barrier.RetryAt.IsZero() {
		t.Fatalf("stop timeout error/retry = %#v/%v", barrier.LastError, barrier.RetryAt)
	}
	beforeStart := barrier
	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: barrier.Generation, Kind: EventStart, At: stateTestEpoch.Add(4 * time.Second)})
	assertInternalError(t, err)
	afterStart, _ := machine.Status("node-a")
	if !reflect.DeepEqual(afterStart, beforeStart) {
		t.Fatal("cleanup-pending barrier changed before acknowledgement")
	}

	disabled := applyMachineEvent(t, machine, "node-a", barrier.Generation, EventCleanupComplete, nil, stateTestEpoch.Add(5*time.Second))
	if disabled.State != model.StateDisabled || disabled.CleanupPending || disabled.ReconnectPending {
		t.Fatalf("cleanup completion status = %#v, want disabled with released barrier", disabled)
	}
}

func TestMachineAdapterStopTimeoutCodeEntersCleanupBarrier(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	stopping := applyMachineEvent(t, machine, "node-a", 1, EventStop, nil, stateTestEpoch.Add(time.Second))
	status := applyMachineEvent(t, machine, "node-a", stopping.Generation, EventFailure, &model.CodeError{Code: ErrorCodeStopTimeout, Message: "raw stop detail"}, stateTestEpoch.Add(2*time.Second))
	if status.State != model.StateRecovering || !status.CleanupPending || status.LastError == nil || status.LastError.Code != ErrorCodeStopTimeout {
		t.Fatalf("status = %#v, want stop-timeout cleanup barrier", status)
	}
}

func TestMachineManualReconnectWaitsForActiveOperationCleanup(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(17)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))

	stopping, err := machine.Apply(Event{NodeID: "node-a", JobID: "reconnect-job", Generation: 1, Kind: EventManualReconnect, At: stateTestEpoch.Add(2 * time.Second)})
	if err != nil {
		t.Fatalf("Apply(manual reconnect) error = %v", err)
	}
	if stopping.State != model.StateStopping || !stopping.ReconnectPending || stopping.Generation != 2 {
		t.Fatalf("manual reconnect status = %#v, want stopping generation 2", stopping)
	}
	beforeStart := stopping
	_, err = machine.Apply(Event{NodeID: "node-a", JobID: "reconnect-job", Generation: 2, Kind: EventStart, At: stateTestEpoch.Add(3 * time.Second)})
	assertInternalError(t, err)
	afterStart, _ := machine.Status("node-a")
	if !reflect.DeepEqual(afterStart, beforeStart) {
		t.Fatal("new start crossed active-operation cleanup barrier")
	}

	queued := applyMachineEventForJob(t, machine, "node-a", "reconnect-job", 2, EventStopped, nil, stateTestEpoch.Add(4*time.Second))
	if queued.State != model.StateQueued || queued.Generation != 3 || queued.ReconnectPending || queued.CleanupPending {
		t.Fatalf("stopped acknowledgement status = %#v, want queued generation 3", queued)
	}
	started := applyMachineEventForJob(t, machine, "node-a", "reconnect-job", 3, EventStart, nil, stateTestEpoch.Add(5*time.Second))
	if started.State != model.StateStarting {
		t.Fatalf("state after released barrier = %q, want starting", started.State)
	}
}

func TestMachineStopSupersedesReconnectWhileStopping(t *testing.T) {
	tests := []struct {
		name         string
		cleanupEntry EventKind
		cleanupError *model.CodeError
	}{
		{name: "stopped acknowledgement"},
		{name: "timeout cleanup acknowledgement", cleanupEntry: EventTimeout},
		{
			name: "adapter stop timeout cleanup acknowledgement", cleanupEntry: EventFailure,
			cleanupError: &model.CodeError{Code: ErrorCodeStopTimeout, Message: "raw adapter detail"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			machine := NewMachine(NewRetryPolicy(zeroSource{}))
			applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
			applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
			stopping := applyMachineEventForJob(t, machine, "node-a", "reconnect-owner", 1, EventManualReconnect, nil, stateTestEpoch.Add(2*time.Second))
			if stopping.State != model.StateStopping || stopping.Generation != 2 || stopping.JobID != "reconnect-owner" || !stopping.ReconnectPending {
				t.Fatalf("reconnect barrier = %#v, want stopping owner generation 2", stopping)
			}

			beforeStop := cloneNodeRecord(machine.nodes["node-a"])
			stopAt := stateTestEpoch.Add(3 * time.Second)
			pendingStop, err := machine.Apply(Event{
				NodeID: "node-a", JobID: "latest-stop", Generation: stopping.Generation,
				Kind: EventStop, At: stopAt,
			})
			if err != nil {
				t.Fatalf("Apply(stop superseding reconnect) error = %v", err)
			}
			expected := cloneNodeRecord(beforeStop)
			expected.status.UpdatedAt = stopAt
			expected.status.ReconnectPending = false
			expected.pendingJobID = "latest-stop"
			expected.resumeAfterRecovery = false
			if got := machine.nodes["node-a"]; !reflect.DeepEqual(got, expected) {
				t.Fatalf("deferred stop changed cleanup ownership or unrelated state:\n got: %#v\nwant: %#v", got, expected)
			}
			if pendingStop.State != model.StateStopping || pendingStop.Generation != 2 || pendingStop.JobID != "reconnect-owner" || pendingStop.ReconnectPending {
				t.Fatalf("pending stop status = %#v, want stopping owner retained and reconnect cleared", pendingStop)
			}

			releaseKind := EventStopped
			if test.cleanupEntry != "" {
				barrier := applyMachineEventForJob(t, machine, "node-a", "reconnect-owner", 2, test.cleanupEntry, test.cleanupError, stateTestEpoch.Add(4*time.Second))
				if barrier.State != model.StateRecovering || !barrier.CleanupPending || barrier.ReconnectPending || barrier.JobID != "reconnect-owner" || barrier.Generation != 2 {
					t.Fatalf("stop-timeout barrier = %#v, want cleanup owned by reconnect operation", barrier)
				}
				if stored := machine.nodes["node-a"]; stored.pendingJobID != "latest-stop" || stored.resumeAfterRecovery {
					t.Fatalf("stop-timeout barrier lost deferred stop intent: %#v", stored)
				}
				releaseKind = EventCleanupComplete
			}

			for name, completion := range map[string]Event{
				"latest intent is not owner": {
					NodeID: "node-a", JobID: "latest-stop", Generation: 2,
					Kind: releaseKind, At: stateTestEpoch.Add(5 * time.Second),
				},
				"stale owner generation": {
					NodeID: "node-a", JobID: "reconnect-owner", Generation: 1,
					Kind: releaseKind, At: stateTestEpoch.Add(5 * time.Second),
				},
			} {
				beforeCompletion := cloneNodeRecord(machine.nodes["node-a"])
				if _, completionErr := machine.Apply(completion); completionErr != nil {
					t.Fatalf("Apply(%s completion) error = %v", name, completionErr)
				}
				if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, beforeCompletion) {
					t.Fatalf("%s completion released or changed owner barrier:\n got: %#v\nwant: %#v", name, after, beforeCompletion)
				}
			}

			disabled := applyMachineEventForJob(t, machine, "node-a", "reconnect-owner", 2, releaseKind, nil, stateTestEpoch.Add(6*time.Second))
			if disabled.State != model.StateDisabled || disabled.Generation != 2 || disabled.JobID != "latest-stop" || disabled.CleanupPending || disabled.ReconnectPending {
				t.Fatalf("owner acknowledgement = %#v, want latest stop released to disabled", disabled)
			}
			if stored := machine.nodes["node-a"]; stored.pendingJobID != "" || stored.resumeAfterRecovery {
				t.Fatalf("released stop retained private barrier metadata: %#v", stored)
			}
			afterRelease := cloneNodeRecord(machine.nodes["node-a"])
			if _, lateErr := machine.Apply(Event{
				NodeID: "node-a", JobID: "reconnect-owner", Generation: 2,
				Kind: releaseKind, At: stateTestEpoch.Add(7 * time.Second),
			}); lateErr != nil {
				t.Fatalf("Apply(late owner completion) error = %v", lateErr)
			}
			if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, afterRelease) {
				t.Fatalf("late owner completion changed released stop:\n got: %#v\nwant: %#v", after, afterRelease)
			}
		})
	}
}

func TestMachineStopSupersedesPendingReconnectWithoutChangingDistinctStopOwner(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}))
	applyMachineEventForJob(t, machine, "node-a", "active-job", 1, EventEnable, nil, stateTestEpoch)
	stopping := applyMachineEventForJob(t, machine, "node-a", "stop-owner", 1, EventStop, nil, stateTestEpoch.Add(time.Second))
	pendingReconnect := applyMachineEventForJob(t, machine, "node-a", "reconnect-intent", stopping.Generation, EventManualReconnect, nil, stateTestEpoch.Add(2*time.Second))
	if pendingReconnect.State != model.StateStopping || pendingReconnect.JobID != "stop-owner" || pendingReconnect.Generation != 2 || !pendingReconnect.ReconnectPending {
		t.Fatalf("pending reconnect = %#v, want distinct stop owner retained", pendingReconnect)
	}

	pendingStop := applyMachineEventForJob(t, machine, "node-a", "latest-stop", 2, EventStop, nil, stateTestEpoch.Add(3*time.Second))
	if pendingStop.State != model.StateStopping || pendingStop.JobID != "stop-owner" || pendingStop.Generation != 2 || pendingStop.ReconnectPending {
		t.Fatalf("superseding stop = %#v, want original stop owner retained", pendingStop)
	}
	if stored := machine.nodes["node-a"]; stored.pendingJobID != "latest-stop" || stored.resumeAfterRecovery {
		t.Fatalf("superseding stop record = %#v, want latest stop intent", stored)
	}

	beforeWrongOwner := cloneNodeRecord(machine.nodes["node-a"])
	if _, err := machine.Apply(Event{
		NodeID: "node-a", JobID: "reconnect-intent", Generation: 2,
		Kind: EventStopped, At: stateTestEpoch.Add(4 * time.Second),
	}); err != nil {
		t.Fatalf("Apply(pending reconnect completion) error = %v", err)
	}
	if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, beforeWrongOwner) {
		t.Fatalf("pending reconnect incorrectly released stop-owner barrier:\n got: %#v\nwant: %#v", after, beforeWrongOwner)
	}

	disabled := applyMachineEventForJob(t, machine, "node-a", "stop-owner", 2, EventStopped, nil, stateTestEpoch.Add(5*time.Second))
	if disabled.State != model.StateDisabled || disabled.Generation != 2 || disabled.JobID != "latest-stop" || disabled.ReconnectPending {
		t.Fatalf("stop owner acknowledgement = %#v, want latest stop disabled", disabled)
	}
}

func TestMachineRepeatedStopWhileStoppingUsesLatestIntentWithoutGenerationBudget(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}))
	applyMachineEventForJob(t, machine, "node-a", "active-job", math.MaxUint64-1, EventEnable, nil, stateTestEpoch)
	stopping := applyMachineEventForJob(t, machine, "node-a", "stop-owner", math.MaxUint64-1, EventStop, nil, stateTestEpoch.Add(time.Second))
	if stopping.State != model.StateStopping || stopping.Generation != math.MaxUint64 || stopping.JobID != "stop-owner" || stopping.ReconnectPending {
		t.Fatalf("initial stop = %#v, want stopping at generation max", stopping)
	}

	for index, jobID := range []string{"newer-stop", "latest-stop"} {
		before := cloneNodeRecord(machine.nodes["node-a"])
		at := stateTestEpoch.Add(time.Duration(index+2) * time.Second)
		status, err := machine.Apply(Event{
			NodeID: "node-a", JobID: jobID, Generation: math.MaxUint64,
			Kind: EventStop, At: at,
		})
		if err != nil {
			t.Fatalf("Apply(repeated stop %q at generation max) error = %v", jobID, err)
		}
		expected := cloneNodeRecord(before)
		expected.status.UpdatedAt = at
		expected.status.ReconnectPending = false
		expected.pendingJobID = jobID
		expected.resumeAfterRecovery = false
		if got := machine.nodes["node-a"]; !reflect.DeepEqual(got, expected) {
			t.Fatalf("repeated stop %q changed owner token or unrelated state:\n got: %#v\nwant: %#v", jobID, got, expected)
		}
		if status.Generation != math.MaxUint64 || status.JobID != "stop-owner" || status.State != model.StateStopping {
			t.Fatalf("repeated stop status = %#v, want original owner at generation max", status)
		}
	}

	beforeOverflow := cloneNodeRecord(machine.nodes["node-a"])
	_, overflowErr := machine.Apply(Event{
		NodeID: "node-a", JobID: "reconnect-after-stop", Generation: math.MaxUint64,
		Kind: EventManualReconnect, At: stateTestEpoch.Add(4 * time.Second),
	})
	assertInternalError(t, overflowErr)
	if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, beforeOverflow) {
		t.Fatalf("overflowing reconnect changed latest stop intent:\n got: %#v\nwant: %#v", after, beforeOverflow)
	}

	for name, completion := range map[string]Event{
		"latest intent is not owner": {
			NodeID: "node-a", JobID: "latest-stop", Generation: math.MaxUint64,
			Kind: EventStopped, At: stateTestEpoch.Add(5 * time.Second),
		},
		"stale owner generation": {
			NodeID: "node-a", JobID: "stop-owner", Generation: math.MaxUint64 - 1,
			Kind: EventStopped, At: stateTestEpoch.Add(5 * time.Second),
		},
	} {
		before := cloneNodeRecord(machine.nodes["node-a"])
		if _, err := machine.Apply(completion); err != nil {
			t.Fatalf("Apply(%s completion) error = %v", name, err)
		}
		if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, before) {
			t.Fatalf("%s completion changed max-generation barrier", name)
		}
	}

	disabled := applyMachineEventForJob(t, machine, "node-a", "stop-owner", math.MaxUint64, EventStopped, nil, stateTestEpoch.Add(6*time.Second))
	if disabled.State != model.StateDisabled || disabled.Generation != math.MaxUint64 || disabled.JobID != "latest-stop" {
		t.Fatalf("owner acknowledgement = %#v, want latest stop disabled at generation max", disabled)
	}
}

func TestMachineManualReconnectDefersWhileRecovering(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(18)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	applyMachineEvent(t, machine, "node-a", 1, EventStarted, nil, stateTestEpoch.Add(2*time.Second))
	applyMachineEvent(t, machine, "node-a", 1, EventValidated, nil, stateTestEpoch.Add(3*time.Second))
	recovering := applyMachineEvent(t, machine, "node-a", 1, EventRecover, nil, stateTestEpoch.Add(4*time.Second))

	pending, err := machine.Apply(Event{NodeID: "node-a", JobID: "reconnect-job", Generation: recovering.Generation, Kind: EventManualReconnect, At: stateTestEpoch.Add(5 * time.Second)})
	if err != nil {
		t.Fatalf("Apply(manual reconnect during recovery) error = %v", err)
	}
	if pending.State != model.StateRecovering || !pending.ReconnectPending || pending.Generation != recovering.Generation {
		t.Fatalf("pending status = %#v, want unchanged recovery generation with reconnect pending", pending)
	}
	_, err = machine.Apply(Event{NodeID: "node-a", JobID: "reconnect-job", Generation: pending.Generation, Kind: EventStart, At: stateTestEpoch.Add(6 * time.Second)})
	assertInternalError(t, err)

	queued := applyMachineEvent(t, machine, "node-a", recovering.Generation, EventRecovered, nil, stateTestEpoch.Add(7*time.Second))
	if queued.State != model.StateQueued || queued.Generation != recovering.Generation+1 || queued.JobID != "reconnect-job" || queued.ReconnectPending {
		t.Fatalf("recovery acknowledgement status = %#v, want deferred reconnect queued", queued)
	}
}

func TestMachineManualReconnectDuringStopTimeoutWaitsForCleanupComplete(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(19)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	stopping := applyMachineEvent(t, machine, "node-a", 1, EventStop, nil, stateTestEpoch.Add(2*time.Second))
	barrier := applyMachineEvent(t, machine, "node-a", stopping.Generation, EventTimeout, nil, stateTestEpoch.Add(3*time.Second))

	pending, err := machine.Apply(Event{NodeID: "node-a", JobID: "reconnect-job", Generation: barrier.Generation, Kind: EventManualReconnect, At: stateTestEpoch.Add(4 * time.Second)})
	if err != nil {
		t.Fatalf("Apply(manual reconnect during cleanup) error = %v", err)
	}
	if pending.State != model.StateRecovering || !pending.CleanupPending || !pending.ReconnectPending {
		t.Fatalf("pending status = %#v, want cleanup and reconnect pending", pending)
	}
	queued := applyMachineEvent(t, machine, "node-a", barrier.Generation, EventCleanupComplete, nil, stateTestEpoch.Add(5*time.Second))
	if queued.State != model.StateQueued || queued.Generation != barrier.Generation+1 || queued.JobID != "reconnect-job" || queued.CleanupPending || queued.ReconnectPending {
		t.Fatalf("cleanup acknowledgement status = %#v, want deferred reconnect queued", queued)
	}
}

func TestMachineStopDefersBehindEveryRecoveryBarrierSubstate(t *testing.T) {
	tests := []struct {
		name             string
		cleanupPending   bool
		reconnectPending bool
	}{
		{name: "ordinary recovery"},
		{name: "ordinary recovery with reconnect", reconnectPending: true},
		{name: "cleanup pending", cleanupPending: true},
		{name: "cleanup and reconnect pending", cleanupPending: true, reconnectPending: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			machine := NewMachine(NewRetryPolicy(zeroSource{}))
			var lastError *PublicError
			if test.cleanupPending {
				lastError = &PublicError{Code: ErrorCodeStopTimeout, Message: "stop timed out"}
			}
			pendingJobID := ""
			if test.reconnectPending {
				pendingJobID = "reconnect-job"
			}
			record := nodeRecord{
				status: NodeStatus{
					NodeID:           "node-a",
					JobID:            "recovery-job",
					Generation:       7,
					State:            model.StateRecovering,
					LastError:        lastError,
					UpdatedAt:        stateTestEpoch,
					CleanupPending:   test.cleanupPending,
					ReconnectPending: test.reconnectPending,
				},
				pendingJobID:        pendingJobID,
				resumeAfterRecovery: !test.cleanupPending,
			}
			machine.nodes["node-a"] = record

			pendingStop, err := machine.Apply(Event{
				NodeID: "node-a", JobID: "stop-job", Generation: 7,
				Kind: EventStop, At: stateTestEpoch.Add(time.Second),
			})
			if err != nil {
				t.Fatalf("Apply(stop during recovery) error = %v", err)
			}
			if pendingStop.State != model.StateRecovering || pendingStop.Generation != 7 ||
				pendingStop.CleanupPending != test.cleanupPending || pendingStop.ReconnectPending {
				t.Fatalf("pending stop status = %#v, want unchanged recovery ownership and reconnect cancelled", pendingStop)
			}
			stored := machine.nodes["node-a"]
			if stored.pendingJobID != "stop-job" || stored.resumeAfterRecovery {
				t.Fatalf("pending stop record = %#v, want stop intent behind recovery barrier", stored)
			}

			_, err = machine.Apply(Event{
				NodeID: "node-a", JobID: "stop-job", Generation: 7,
				Kind: EventStart, At: stateTestEpoch.Add(2 * time.Second),
			})
			assertInternalError(t, err)

			releaseKind := EventRecovered
			if test.cleanupPending {
				releaseKind = EventCleanupComplete
			}
			disabled := applyMachineEventForJob(t, machine, "node-a", "recovery-job", 7, releaseKind, nil, stateTestEpoch.Add(3*time.Second))
			if disabled.State != model.StateDisabled || disabled.Generation != 7 || disabled.JobID != "stop-job" ||
				disabled.CleanupPending || disabled.ReconnectPending {
				t.Fatalf("released stop status = %#v, want disabled without starting overlapping work", disabled)
			}
		})
	}
}

func TestMachinePendingRecoveryIntentFailureAndTimeoutStayBehindCleanupBarrier(t *testing.T) {
	owners := []struct {
		name           string
		cleanupPending bool
	}{
		{name: "ordinary recovery"},
		{name: "cleanup recovery", cleanupPending: true},
	}
	intents := []struct {
		name             string
		jobID            string
		reconnectPending bool
	}{
		{name: "stop intent", jobID: "stop-job"},
		{name: "reconnect intent", jobID: "reconnect-job", reconnectPending: true},
	}
	completions := []struct {
		name        string
		kind        EventKind
		err         *model.CodeError
		wantCode    string
		wantMessage string
	}{
		{
			name: "failure", kind: EventFailure,
			err:         &model.CodeError{Code: ErrorCodeProbeFailed, Message: "raw owner failure"},
			wantCode:    ErrorCodeProbeFailed,
			wantMessage: "connection probe failed",
		},
		{
			name:        "timeout",
			kind:        EventTimeout,
			wantCode:    ErrorCodeStopTimeout,
			wantMessage: "stop timed out",
		},
	}

	for _, owner := range owners {
		for _, intent := range intents {
			for _, completion := range completions {
				t.Run(owner.name+"/"+intent.name+"/"+completion.name, func(t *testing.T) {
					machine := NewMachine(NewRetryPolicy(zeroSource{}))
					var initialError *PublicError
					if owner.cleanupPending {
						initialError = &PublicError{Code: ErrorCodeStopTimeout, Message: "stop timed out"}
					}
					before := nodeRecord{
						status: NodeStatus{
							NodeID: "node-a", JobID: "owner-job", Generation: 7,
							State: model.StateRecovering, Attempts: 3, LastError: initialError,
							UpdatedAt: stateTestEpoch, CleanupPending: owner.cleanupPending,
							ReconnectPending: intent.reconnectPending,
						},
						onlineSinceElapsed:  time.Minute,
						retryReadyElapsed:   2 * time.Minute,
						pendingJobID:        intent.jobID,
						resumeAfterRecovery: !owner.cleanupPending && intent.reconnectPending,
					}
					machine.nodes["node-a"] = before
					completionAt := stateTestEpoch.Add(time.Second)

					status, err := machine.Apply(Event{
						NodeID: "node-a", JobID: "owner-job", Generation: 7,
						Kind: completion.kind, Err: completion.err, At: completionAt,
					})
					if err != nil {
						t.Fatalf("Apply(owner %s) error = %v", completion.kind, err)
					}
					if status.State != model.StateRecovering || !status.CleanupPending ||
						status.ReconnectPending != intent.reconnectPending || status.Generation != 7 || status.JobID != "owner-job" ||
						status.Attempts != 3 || !status.RetryAt.IsZero() || status.LastError == nil ||
						status.LastError.Code != completion.wantCode || status.LastError.Message != completion.wantMessage {
						t.Fatalf("barrier status = %#v, want preserved intent behind cleanup", status)
					}
					barrier := machine.nodes["node-a"]
					if barrier.pendingJobID != intent.jobID || barrier.resumeAfterRecovery || barrier.retryReadySet || barrier.onlineSinceSet ||
						barrier.retryReadyElapsed != 2*time.Minute || barrier.onlineSinceElapsed != time.Minute {
						t.Fatalf("barrier private record = %#v, want pending intent and inert retry/online metadata", barrier)
					}

					for _, blocked := range []EventKind{EventStart, EventRetryDue, EventRecovered} {
						beforeBlocked := cloneNodeRecord(machine.nodes["node-a"])
						_, blockedErr := machine.Apply(Event{
							NodeID: "node-a", JobID: "owner-job", Generation: 7,
							Kind: blocked, At: completionAt.Add(time.Second),
						})
						assertInternalError(t, blockedErr)
						if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, beforeBlocked) {
							t.Fatalf("blocked %s changed barrier:\n got: %#v\nwant: %#v", blocked, after, beforeBlocked)
						}
					}

					for name, stale := range map[string]Event{
						"wrong job": {
							NodeID: "node-a", JobID: "other-job", Generation: 7,
							Kind: EventCleanupComplete, At: completionAt.Add(2 * time.Second),
						},
						"old generation": {
							NodeID: "node-a", JobID: "owner-job", Generation: 6,
							Kind: EventCleanupComplete, At: completionAt.Add(2 * time.Second),
						},
					} {
						beforeStale := cloneNodeRecord(machine.nodes["node-a"])
						if _, staleErr := machine.Apply(stale); staleErr != nil {
							t.Fatalf("Apply(%s cleanup acknowledgement) error = %v", name, staleErr)
						}
						if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, beforeStale) {
							t.Fatalf("%s cleanup acknowledgement changed barrier", name)
						}
					}

					released := applyMachineEventForJob(t, machine, "node-a", "owner-job", 7, EventCleanupComplete, nil, completionAt.Add(3*time.Second))
					if intent.reconnectPending {
						if released.State != model.StateQueued || released.Generation != 8 || released.JobID != intent.jobID {
							t.Fatalf("released reconnect = %#v, want queued generation 8", released)
						}
					} else if released.State != model.StateDisabled || released.Generation != 7 || released.JobID != intent.jobID {
						t.Fatalf("released stop = %#v, want disabled generation 7", released)
					}
					afterRelease := cloneNodeRecord(machine.nodes["node-a"])
					if _, lateErr := machine.Apply(Event{
						NodeID: "node-a", JobID: "owner-job", Generation: 7,
						Kind: EventFailure, Err: &model.CodeError{Code: ErrorCodeProbeFailed}, At: completionAt.Add(4 * time.Second),
					}); lateErr != nil {
						t.Fatalf("Apply(late owner completion) error = %v", lateErr)
					}
					if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, afterRelease) {
						t.Fatalf("late owner completion changed released record:\n got: %#v\nwant: %#v", after, afterRelease)
					}
				})
			}
		}
	}
}

func TestMachineRecoveryFailureWithoutPendingIntentClearsBarrierMetadata(t *testing.T) {
	tests := []struct {
		name string
		kind EventKind
		err  *model.CodeError
	}{
		{name: "failure", kind: EventFailure, err: &model.CodeError{Code: ErrorCodeProbeFailed}},
		{name: "timeout", kind: EventTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			machine := NewMachine(NewRetryPolicy(zeroSource{}))
			machine.nodes["node-a"] = nodeRecord{
				status: NodeStatus{
					NodeID: "node-a", JobID: "owner-job", Generation: 7,
					State: model.StateRecovering, UpdatedAt: stateTestEpoch,
				},
				resumeAfterRecovery: true,
			}

			status, err := machine.Apply(Event{
				NodeID: "node-a", JobID: "owner-job", Generation: 7,
				Kind: test.kind, Err: test.err, At: stateTestEpoch.Add(time.Second),
			})
			if err != nil {
				t.Fatalf("Apply(%s) error = %v", test.kind, err)
			}
			if status.State != model.StateBackoff {
				t.Fatalf("state = %q, want ordinary retry backoff", status.State)
			}
			stored := machine.nodes["node-a"]
			if stored.resumeAfterRecovery || stored.pendingJobID != "" || stored.status.CleanupPending || stored.status.ReconnectPending {
				t.Fatalf("completed recovery retained barrier metadata: %#v", stored)
			}
		})
	}
}

func TestMachineFutureGenerationEventReturnsInternalWithoutMutation(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(8)))
	before := applyMachineEvent(t, machine, "node-a", 8, EventEnable, nil, stateTestEpoch)

	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 9, Kind: EventStart, At: stateTestEpoch.Add(time.Second)})
	assertInternalError(t, err)
	after, _ := machine.Status("node-a")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("future generation changed status:\n got: %#v\nwant: %#v", after, before)
	}
}

func TestMachineStatusReturnsIndependentErrorCopy(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(9)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	inputError := &model.CodeError{Code: ErrorCodeAuthentication, Message: "credentials rejected"}
	applyMachineEvent(t, machine, "node-a", 1, EventFailure, inputError, stateTestEpoch.Add(2*time.Second))
	inputError.Message = "mutated input"

	first, ok := machine.Status("node-a")
	if !ok {
		t.Fatal("Status() did not find node")
	}
	first.LastError.Message = "mutated output"
	second, _ := machine.Status("node-a")
	if second.LastError.Message != "authentication failed" {
		t.Fatalf("stored error message = %q, want independent copy", second.LastError.Message)
	}
}

func TestMachineDoesNotExposeRawAdapterErrorMessage(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(10)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	status := applyMachineEvent(t, machine, "node-a", 1, EventFailure, &model.CodeError{
		Code:    "dial_failed",
		Message: "dial command contained password=super-secret and token=hidden",
	}, stateTestEpoch.Add(2*time.Second))
	if status.LastError == nil || status.LastError.Code != ErrorCodeInternal {
		t.Fatalf("LastError = %#v, want normalized internal", status.LastError)
	}
	if status.LastError.Message != "internal node error" {
		t.Fatalf("public message = %q, want sanitized generic message", status.LastError.Message)
	}
}

func TestMachineDropsCompletionFromSupersededJob(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(11)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	before, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-b", Generation: 1, Kind: EventStop, At: stateTestEpoch.Add(2 * time.Second)})
	if err != nil {
		t.Fatalf("Apply(stop) error = %v", err)
	}
	if before.Generation != 2 {
		t.Fatalf("stop generation = %d, want 2", before.Generation)
	}

	got, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 1, Kind: EventStarted, At: stateTestEpoch.Add(3 * time.Second)})
	if err != nil {
		t.Fatalf("Apply(stale job completion) error = %v", err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("superseded job completion changed status:\n got: %#v\nwant: %#v", got, before)
	}
}

func TestMachineRecoveryCreatesNewOperationGeneration(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(15)))
	applyMachineEvent(t, machine, "node-a", 3, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 3, EventStart, nil, stateTestEpoch.Add(time.Second))
	applyMachineEvent(t, machine, "node-a", 3, EventStarted, nil, stateTestEpoch.Add(2*time.Second))
	applyMachineEvent(t, machine, "node-a", 3, EventValidated, nil, stateTestEpoch.Add(3*time.Second))

	status := applyMachineEvent(t, machine, "node-a", 3, EventRecover, nil, stateTestEpoch.Add(4*time.Second))
	if status.Generation != 4 || status.State != model.StateRecovering {
		t.Fatalf("recovery status = %#v, want generation 4 recovering", status)
	}
	before := status
	got, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 3, Kind: EventFailure, Err: &model.CodeError{Code: "temporary", Message: "old attempt"}, At: stateTestEpoch.Add(5 * time.Second)})
	if err != nil {
		t.Fatalf("Apply(old recovery completion) error = %v", err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("old operation changed recovery status:\n got: %#v\nwant: %#v", got, before)
	}
}

func TestMachineReservesGenerationZero(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(12)))
	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 0, Kind: EventEnable, At: stateTestEpoch})
	assertInternalError(t, err)
	if _, ok := machine.Status("node-a"); ok {
		t.Fatal("generation zero created a node status")
	}
}

func TestMachineRequiresJobIDWithoutCreatingStatus(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(rand.NewSource(13)))
	_, err := machine.Apply(Event{NodeID: "node-a", Generation: 1, Kind: EventEnable, At: stateTestEpoch})
	assertInternalError(t, err)
	if _, ok := machine.Status("node-a"); ok {
		t.Fatal("event without job ID created a node status")
	}
}

func TestMachineDefaultStableOnlineWindowIsFiveMinutes(t *testing.T) {
	clock := &manualClock{wall: stateTestEpoch}
	machine := NewMachine(NewRetryPolicy(rand.NewSource(14)), WithClock(clock))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	applyMachineEvent(t, machine, "node-a", 1, EventStarted, nil, stateTestEpoch.Add(2*time.Second))
	onlineAt := stateTestEpoch.Add(3 * time.Second)
	before := applyMachineEvent(t, machine, "node-a", 1, EventValidated, nil, onlineAt)

	clock.advance(5*time.Minute - time.Nanosecond)
	clock.wall = onlineAt.Add(24 * time.Hour)
	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 1, Kind: EventStableOnline, At: clock.wall})
	assertInternalError(t, err)
	afterEarly, _ := machine.Status("node-a")
	if !reflect.DeepEqual(afterEarly, before) {
		t.Fatal("premature default stable event changed status")
	}
	clock.advance(time.Nanosecond)
	clock.wall = onlineAt.Add(-24 * time.Hour)
	status := applyMachineEvent(t, machine, "node-a", 1, EventStableOnline, nil, clock.wall)
	if status.Attempts != 0 {
		t.Fatalf("Attempts = %d, want reset at default five-minute window", status.Attempts)
	}
}

func TestMachineAutomaticRetryRejectsGenerationOverflowWithoutMutation(t *testing.T) {
	clock := &manualClock{wall: stateTestEpoch}
	machine := NewMachine(NewRetryPolicy(zeroSource{}), WithClock(clock))
	applyMachineEvent(t, machine, "node-a", math.MaxUint64, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", math.MaxUint64, EventStart, nil, stateTestEpoch.Add(time.Second))
	before := applyMachineEvent(t, machine, "node-a", math.MaxUint64, EventTimeout, nil, stateTestEpoch.Add(2*time.Second))
	clock.advance(4 * time.Second)

	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: math.MaxUint64, Kind: EventRetryDue, At: before.RetryAt})
	assertInternalError(t, err)
	after, _ := machine.Status("node-a")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("retry overflow changed status:\n got: %#v\nwant: %#v", after, before)
	}
}

func TestMachineAttemptsSaturate(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	machine.mu.Lock()
	record := machine.nodes["node-a"]
	record.status.Attempts = math.MaxUint64
	machine.nodes["node-a"] = record
	machine.mu.Unlock()

	status := applyMachineEvent(t, machine, "node-a", 1, EventTimeout, nil, stateTestEpoch.Add(2*time.Second))
	if status.Attempts != math.MaxUint64 {
		t.Fatalf("Attempts = %d, want saturation at %d", status.Attempts, uint64(math.MaxUint64))
	}
}

func TestMachineRejectsEveryUnlistedStateEventPairWithoutMutatingRecord(t *testing.T) {
	eventKinds := []EventKind{
		EventEnable, EventStart, EventStarted, EventValidated, EventDegraded, EventHealthy,
		EventStop, EventStopped, EventFailure, EventTimeout, EventRetryDue, EventWANAvailable,
		EventRecover, EventRecovered, EventCleanupComplete, EventManualReconnect, EventStableOnline,
	}
	allowed := map[model.RuntimeState]map[EventKind]bool{
		model.StateDisabled:   {},
		model.StateQueued:     {EventStart: true, EventStop: true, EventManualReconnect: true},
		model.StateStarting:   {EventStarted: true, EventStop: true, EventFailure: true, EventTimeout: true, EventRecover: true, EventManualReconnect: true},
		model.StateValidating: {EventValidated: true, EventStop: true, EventFailure: true, EventTimeout: true, EventRecover: true, EventManualReconnect: true},
		model.StateOnline:     {EventDegraded: true, EventStop: true, EventFailure: true, EventTimeout: true, EventRecover: true, EventManualReconnect: true, EventStableOnline: true},
		model.StateDegraded:   {EventHealthy: true, EventStop: true, EventFailure: true, EventTimeout: true, EventRecover: true, EventManualReconnect: true},
		model.StateStopping:   {EventStop: true, EventStopped: true, EventFailure: true, EventTimeout: true, EventManualReconnect: true},
		model.StateFailed:     {EventStop: true, EventRecover: true, EventManualReconnect: true},
		model.StateBackoff:    {EventStop: true, EventRetryDue: true, EventWANAvailable: true, EventRecover: true, EventManualReconnect: true},
		model.StateRecovering: {EventStop: true, EventFailure: true, EventTimeout: true, EventRecovered: true, EventCleanupComplete: true, EventManualReconnect: true},
	}

	for state, allowedEvents := range allowed {
		state := state
		for _, kind := range eventKinds {
			kind := kind
			if allowedEvents[kind] {
				continue
			}
			t.Run(string(state)+"/"+string(kind), func(t *testing.T) {
				clock := &manualClock{wall: stateTestEpoch, monotonic: 10 * time.Minute}
				machine := NewMachine(NewRetryPolicy(zeroSource{}), WithClock(clock))
				record := nodeRecord{
					status: NodeStatus{
						NodeID: "node-a", JobID: "job-a", Generation: 7, State: state, Attempts: 3,
						LastError: &PublicError{Code: ErrorCodeInternal, Message: "internal node error"},
						RetryAt:   stateTestEpoch.Add(time.Minute), UpdatedAt: stateTestEpoch,
					},
					onlineSinceElapsed:  time.Minute,
					onlineSinceSet:      state == model.StateOnline,
					retryReadyElapsed:   2 * time.Minute,
					retryReadySet:       state == model.StateBackoff,
					resumeAfterRecovery: state == model.StateRecovering,
				}
				machine.nodes["node-a"] = record
				before := cloneNodeRecord(record)
				_, err := machine.Apply(Event{
					NodeID: "node-a", JobID: "job-a", Generation: 7, Kind: kind,
					Err: &model.CodeError{Code: "temporary", Message: "raw detail"}, At: stateTestEpoch.Add(time.Hour),
				})
				assertInternalError(t, err)
				if after := machine.nodes["node-a"]; !reflect.DeepEqual(after, before) {
					t.Fatalf("unlisted transition changed complete record:\n got: %#v\nwant: %#v", after, before)
				}
			})
		}
	}
}

func applyMachineEvent(t *testing.T, machine *Machine, nodeID string, generation uint64, kind EventKind, codeErr *model.CodeError, at time.Time) NodeStatus {
	t.Helper()
	return applyMachineEventForJob(t, machine, nodeID, "job-a", generation, kind, codeErr, at)
}

func applyMachineEventForJob(t *testing.T, machine *Machine, nodeID, jobID string, generation uint64, kind EventKind, codeErr *model.CodeError, at time.Time) NodeStatus {
	t.Helper()
	status, err := machine.Apply(Event{NodeID: nodeID, JobID: jobID, Generation: generation, Kind: kind, Err: codeErr, At: at})
	if err != nil {
		t.Fatalf("Apply(%s) error = %v", kind, err)
	}
	return status
}

func assertInternalError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want internal CodeError")
	}
	var codeErr *model.CodeError
	if !errors.As(err, &codeErr) || codeErr.Code != ErrorCodeInternal {
		t.Fatalf("error = %#v, want CodeError code %q", err, ErrorCodeInternal)
	}
}

type manualClock struct {
	wall      time.Time
	monotonic time.Duration
}

func (clock *manualClock) Now() time.Time { return clock.wall }

func (clock *manualClock) Monotonic() time.Duration { return clock.monotonic }

func (clock *manualClock) NewTimer(time.Duration) platform.Timer {
	return manualTimer{channel: make(chan time.Time)}
}

func (clock *manualClock) advance(delta time.Duration) { clock.monotonic += delta }

type manualTimer struct {
	channel <-chan time.Time
}

func (timer manualTimer) C() <-chan time.Time { return timer.channel }

func (manualTimer) Stop() bool { return true }
