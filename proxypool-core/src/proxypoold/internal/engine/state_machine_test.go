package engine

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"proxypoold/internal/model"
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
		if step.kind == EventRecover || step.kind == EventManualReconnect || step.kind == EventStop {
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

func TestMachinePermanentFailureEntersFailedWithoutRetry(t *testing.T) {
	for _, code := range []string{ErrorCodeAuthentication, ErrorCodeInvalidConfig, ErrorCodeUnsupportedOption} {
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

func TestMachineTimeoutEntersBackoff(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	timedOutAt := stateTestEpoch.Add(2 * time.Second)

	status := applyMachineEvent(t, machine, "node-a", 1, EventTimeout, nil, timedOutAt)
	if status.State != model.StateBackoff {
		t.Fatalf("state = %q, want %q", status.State, model.StateBackoff)
	}
	if status.LastError == nil || status.LastError.Code != ErrorCodeTimeout {
		t.Fatalf("LastError = %#v, want timeout", status.LastError)
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

func TestMachineStableOnlineResetsConsecutiveAttempts(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}), WithStableOnlineWindow(30*time.Second))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	backoff := applyMachineEvent(t, machine, "node-a", 1, EventTimeout, nil, stateTestEpoch.Add(2*time.Second))
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
	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 2, Kind: EventStableOnline, At: backoff.RetryAt.Add(32 * time.Second)})
	assertInternalError(t, err)
	afterEarly, _ := machine.Status("node-a")
	if !reflect.DeepEqual(afterEarly, beforeEarly) {
		t.Fatalf("premature stable event changed status:\n got: %#v\nwant: %#v", afterEarly, beforeEarly)
	}

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

func TestMachineStopTimeoutDoesNotScheduleReconnect(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	before := applyMachineEvent(t, machine, "node-a", 1, EventStop, nil, stateTestEpoch.Add(2*time.Second))

	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: before.Generation, Kind: EventTimeout, At: stateTestEpoch.Add(3 * time.Second)})
	assertInternalError(t, err)
	after, _ := machine.Status("node-a")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("stop timeout scheduled reconnect or changed state:\n got: %#v\nwant: %#v", after, before)
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
	if status.LastError == nil || status.LastError.Code != "dial_failed" {
		t.Fatalf("LastError = %#v, want dial_failed", status.LastError)
	}
	if status.LastError.Message != "node operation failed" {
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
	machine := NewMachine(NewRetryPolicy(rand.NewSource(14)))
	applyMachineEvent(t, machine, "node-a", 1, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", 1, EventStart, nil, stateTestEpoch.Add(time.Second))
	applyMachineEvent(t, machine, "node-a", 1, EventStarted, nil, stateTestEpoch.Add(2*time.Second))
	onlineAt := stateTestEpoch.Add(3 * time.Second)
	before := applyMachineEvent(t, machine, "node-a", 1, EventValidated, nil, onlineAt)

	_, err := machine.Apply(Event{NodeID: "node-a", JobID: "job-a", Generation: 1, Kind: EventStableOnline, At: onlineAt.Add(5*time.Minute - time.Nanosecond)})
	assertInternalError(t, err)
	afterEarly, _ := machine.Status("node-a")
	if !reflect.DeepEqual(afterEarly, before) {
		t.Fatal("premature default stable event changed status")
	}
	status := applyMachineEvent(t, machine, "node-a", 1, EventStableOnline, nil, onlineAt.Add(5*time.Minute))
	if status.Attempts != 0 {
		t.Fatalf("Attempts = %d, want reset at default five-minute window", status.Attempts)
	}
}

func TestMachineAutomaticRetryRejectsGenerationOverflowWithoutMutation(t *testing.T) {
	machine := NewMachine(NewRetryPolicy(zeroSource{}))
	applyMachineEvent(t, machine, "node-a", math.MaxUint64, EventEnable, nil, stateTestEpoch)
	applyMachineEvent(t, machine, "node-a", math.MaxUint64, EventStart, nil, stateTestEpoch.Add(time.Second))
	before := applyMachineEvent(t, machine, "node-a", math.MaxUint64, EventTimeout, nil, stateTestEpoch.Add(2*time.Second))

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

func applyMachineEvent(t *testing.T, machine *Machine, nodeID string, generation uint64, kind EventKind, codeErr *model.CodeError, at time.Time) NodeStatus {
	t.Helper()
	status, err := machine.Apply(Event{NodeID: nodeID, JobID: "job-a", Generation: generation, Kind: kind, Err: codeErr, At: at})
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
