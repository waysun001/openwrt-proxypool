package engine

import (
	"math"
	"sync"
	"time"

	"proxypoold/internal/model"
)

const (
	ErrorCodeInternal          = "internal"
	ErrorCodeAuthentication    = "auth_failed"
	ErrorCodeInvalidConfig     = "invalid_config"
	ErrorCodeUnsupportedOption = "unsupported_option"
	ErrorCodeWANDown           = "wan_down"
	ErrorCodeTimeout           = "timeout"
	ErrorCodeCapacityExceeded  = "capacity_exceeded"
	ErrorCodeInvalidJob        = "invalid_job"

	DefaultStableOnlineWindow = 5 * time.Minute
)

type EventKind string

const (
	EventEnable          EventKind = "enable"
	EventStart           EventKind = "start"
	EventStarted         EventKind = "started"
	EventValidated       EventKind = "validated"
	EventDegraded        EventKind = "degraded"
	EventHealthy         EventKind = "healthy"
	EventStop            EventKind = "stop"
	EventStopped         EventKind = "stopped"
	EventFailure         EventKind = "failure"
	EventTimeout         EventKind = "timeout"
	EventRetryDue        EventKind = "retry_due"
	EventWANAvailable    EventKind = "wan_available"
	EventRecover         EventKind = "recover"
	EventRecovered       EventKind = "recovered"
	EventManualReconnect EventKind = "manual_reconnect"
	EventStableOnline    EventKind = "stable_online"
)

type Event struct {
	NodeID     string
	JobID      string
	Generation uint64
	Kind       EventKind
	Err        *model.CodeError
	At         time.Time
}

type PublicError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type NodeStatus struct {
	NodeID     string             `json:"node_id"`
	JobID      string             `json:"job_id"`
	Generation uint64             `json:"generation"`
	State      model.RuntimeState `json:"state"`
	Attempts   uint64             `json:"attempts"`
	LastError  *PublicError       `json:"last_error,omitempty"`
	RetryAt    time.Time          `json:"retry_at,omitempty"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

type MachineOption func(*Machine)

func WithStableOnlineWindow(window time.Duration) MachineOption {
	return func(machine *Machine) {
		if window > 0 {
			machine.stableOnlineWindow = window
		}
	}
}

type nodeRecord struct {
	status      NodeStatus
	onlineSince time.Time
}

type Machine struct {
	mu                 sync.RWMutex
	nodes              map[string]nodeRecord
	retry              *RetryPolicy
	stableOnlineWindow time.Duration
}

func NewMachine(retry *RetryPolicy, options ...MachineOption) *Machine {
	if retry == nil {
		retry = NewRetryPolicy(nil)
	}
	machine := &Machine{
		nodes:              make(map[string]nodeRecord),
		retry:              retry,
		stableOnlineWindow: DefaultStableOnlineWindow,
	}
	for _, option := range options {
		if option != nil {
			option(machine)
		}
	}
	return machine
}

// Apply serializes one state transition. Invalid transitions are atomic: the
// stored status is not changed. Completion events from an older generation,
// superseded job, or older timestamp are intentionally dropped.
func (m *Machine) Apply(event Event) (NodeStatus, error) {
	if event.NodeID == "" || event.JobID == "" || event.Generation == 0 || event.At.IsZero() || !validEventKind(event.Kind) {
		return NodeStatus{}, internalTransitionError()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	record, exists := m.nodes[event.NodeID]
	if !exists {
		if event.Kind != EventEnable {
			return NodeStatus{}, internalTransitionError()
		}
		record = nodeRecord{status: NodeStatus{
			NodeID:     event.NodeID,
			JobID:      event.JobID,
			Generation: event.Generation,
			State:      model.StateQueued,
			UpdatedAt:  event.At,
		}}
		m.nodes[event.NodeID] = record
		return cloneNodeStatus(record.status), nil
	}

	if event.Generation < record.status.Generation {
		if isCompletionEvent(event.Kind) {
			return cloneNodeStatus(record.status), nil
		}
		return NodeStatus{}, internalTransitionError()
	}
	if event.Generation > record.status.Generation {
		if event.Kind == EventEnable && record.status.State == model.StateDisabled {
			record = nodeRecord{status: NodeStatus{
				NodeID:     event.NodeID,
				JobID:      event.JobID,
				Generation: event.Generation,
				State:      model.StateQueued,
				UpdatedAt:  event.At,
			}}
			m.nodes[event.NodeID] = record
			return cloneNodeStatus(record.status), nil
		}
		return NodeStatus{}, internalTransitionError()
	}
	if event.At.Before(record.status.UpdatedAt) {
		if isCompletionEvent(event.Kind) {
			return cloneNodeStatus(record.status), nil
		}
		return NodeStatus{}, internalTransitionError()
	}
	if event.JobID != record.status.JobID && isCompletionEvent(event.Kind) {
		return cloneNodeStatus(record.status), nil
	}
	if event.JobID != record.status.JobID && !startsNewOperation(event.Kind) {
		return NodeStatus{}, internalTransitionError()
	}

	candidate := cloneNodeRecord(record)
	if err := m.transition(&candidate, event); err != nil {
		return NodeStatus{}, err
	}
	candidate.status.UpdatedAt = event.At
	m.nodes[event.NodeID] = candidate
	return cloneNodeStatus(candidate.status), nil
}

func (m *Machine) Status(nodeID string) (NodeStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.nodes[nodeID]
	if !ok {
		return NodeStatus{}, false
	}
	return cloneNodeStatus(record.status), true
}

func (m *Machine) transition(record *nodeRecord, event Event) error {
	switch event.Kind {
	case EventEnable:
		return internalTransitionError()
	case EventStart:
		if record.status.State != model.StateQueued {
			return internalTransitionError()
		}
		record.status.State = model.StateStarting
		record.status.RetryAt = time.Time{}
	case EventStarted:
		if record.status.State != model.StateStarting {
			return internalTransitionError()
		}
		record.status.State = model.StateValidating
	case EventValidated:
		if record.status.State != model.StateValidating {
			return internalTransitionError()
		}
		record.status.State = model.StateOnline
		record.status.LastError = nil
		record.status.RetryAt = time.Time{}
		record.onlineSince = event.At
	case EventDegraded:
		if record.status.State != model.StateOnline {
			return internalTransitionError()
		}
		record.status.State = model.StateDegraded
		record.onlineSince = time.Time{}
		failure := event.Err
		if failure == nil {
			failure = &model.CodeError{Code: "health_check_failed", Message: "health check failed"}
		}
		record.status.LastError = publicErrorFromCode(failure)
	case EventHealthy:
		if record.status.State != model.StateDegraded {
			return internalTransitionError()
		}
		record.status.State = model.StateOnline
		record.status.LastError = nil
		record.status.RetryAt = time.Time{}
		record.onlineSince = event.At
	case EventStop:
		if !canStop(record.status.State) {
			return internalTransitionError()
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateStopping
		record.status.RetryAt = time.Time{}
		record.onlineSince = time.Time{}
	case EventStopped:
		if record.status.State != model.StateStopping {
			return internalTransitionError()
		}
		record.status.State = model.StateDisabled
		record.status.Attempts = 0
		record.status.LastError = nil
		record.status.RetryAt = time.Time{}
		record.onlineSince = time.Time{}
	case EventFailure:
		if !canFail(record.status.State) || event.Err == nil || event.Err.Code == "" {
			return internalTransitionError()
		}
		m.applyFailure(record, event.Err, event.At)
	case EventTimeout:
		if !canTimeout(record.status.State) {
			return internalTransitionError()
		}
		m.applyFailure(record, &model.CodeError{Code: ErrorCodeTimeout, Message: "operation timed out"}, event.At)
	case EventRetryDue:
		if record.status.State != model.StateBackoff || record.status.RetryAt.IsZero() || event.At.Before(record.status.RetryAt) {
			return internalTransitionError()
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateQueued
		record.status.RetryAt = time.Time{}
		record.onlineSince = time.Time{}
	case EventWANAvailable:
		if record.status.State != model.StateBackoff || !record.status.RetryAt.IsZero() || record.status.LastError == nil || record.status.LastError.Code != ErrorCodeWANDown {
			return internalTransitionError()
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateQueued
		record.onlineSince = time.Time{}
	case EventRecover:
		if !canRecover(record.status.State) {
			return internalTransitionError()
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateRecovering
		record.status.RetryAt = time.Time{}
		record.onlineSince = time.Time{}
		if event.Err != nil {
			record.status.LastError = publicErrorFromCode(event.Err)
		}
	case EventRecovered:
		if record.status.State != model.StateRecovering {
			return internalTransitionError()
		}
		record.status.State = model.StateQueued
		record.status.RetryAt = time.Time{}
	case EventManualReconnect:
		if !canManuallyReconnect(record.status.State) {
			return internalTransitionError()
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateQueued
		record.status.Attempts = 0
		record.status.LastError = nil
		record.status.RetryAt = time.Time{}
		record.onlineSince = time.Time{}
	case EventStableOnline:
		if record.status.State != model.StateOnline || record.onlineSince.IsZero() || event.At.Sub(record.onlineSince) < m.stableOnlineWindow {
			return internalTransitionError()
		}
		record.status.Attempts = 0
		record.status.LastError = nil
		record.status.RetryAt = time.Time{}
	default:
		return internalTransitionError()
	}
	return nil
}

func (m *Machine) applyFailure(record *nodeRecord, failure *model.CodeError, at time.Time) {
	decision := m.retry.Next(record.status.Attempts, failure)
	if record.status.Attempts < math.MaxUint64 {
		record.status.Attempts++
	}
	record.status.LastError = publicErrorFromCode(failure)
	record.status.RetryAt = time.Time{}
	record.onlineSince = time.Time{}
	switch decision.Mode {
	case RetryAfter:
		record.status.State = model.StateBackoff
		record.status.RetryAt = at.Add(decision.Delay)
	case RetryOnWANEvent:
		record.status.State = model.StateBackoff
	default:
		record.status.State = model.StateFailed
	}
}

func startsNewOperation(kind EventKind) bool {
	switch kind {
	case EventStop, EventRecover, EventRetryDue, EventWANAvailable, EventManualReconnect:
		return true
	default:
		return false
	}
}

func isCompletionEvent(kind EventKind) bool {
	switch kind {
	case EventStarted, EventValidated, EventDegraded, EventHealthy, EventStopped,
		EventFailure, EventTimeout, EventRecovered, EventStableOnline:
		return true
	default:
		return false
	}
}

func validEventKind(kind EventKind) bool {
	switch kind {
	case EventEnable, EventStart, EventStarted, EventValidated, EventDegraded,
		EventHealthy, EventStop, EventStopped, EventFailure, EventTimeout,
		EventRetryDue, EventWANAvailable, EventRecover, EventRecovered,
		EventManualReconnect, EventStableOnline:
		return true
	default:
		return false
	}
}

func canStop(state model.RuntimeState) bool {
	switch state {
	case model.StateQueued, model.StateStarting, model.StateValidating, model.StateOnline,
		model.StateDegraded, model.StateFailed, model.StateBackoff, model.StateRecovering:
		return true
	default:
		return false
	}
}

func canFail(state model.RuntimeState) bool {
	switch state {
	case model.StateStarting, model.StateValidating, model.StateOnline, model.StateDegraded, model.StateRecovering:
		return true
	default:
		return false
	}
}

func canTimeout(state model.RuntimeState) bool {
	return canFail(state)
}

func canRecover(state model.RuntimeState) bool {
	switch state {
	case model.StateStarting, model.StateValidating, model.StateOnline, model.StateDegraded,
		model.StateFailed, model.StateBackoff:
		return true
	default:
		return false
	}
}

func canManuallyReconnect(state model.RuntimeState) bool {
	switch state {
	case model.StateQueued, model.StateStarting, model.StateValidating, model.StateOnline,
		model.StateDegraded, model.StateFailed, model.StateBackoff, model.StateRecovering:
		return true
	default:
		return false
	}
}

func bumpGeneration(status *NodeStatus) error {
	if status.Generation == math.MaxUint64 {
		return internalTransitionError()
	}
	status.Generation++
	return nil
}

func publicErrorFromCode(source *model.CodeError) *PublicError {
	if source == nil {
		return nil
	}
	message := "node operation failed"
	switch source.Code {
	case ErrorCodeAuthentication:
		message = "authentication failed"
	case ErrorCodeInvalidConfig:
		message = "configuration is invalid"
	case ErrorCodeUnsupportedOption:
		message = "protocol option is unsupported"
	case ErrorCodeWANDown:
		message = "WAN is unavailable"
	case ErrorCodeTimeout:
		message = "operation timed out"
	case "health_check_failed":
		message = "health check failed"
	}
	return &PublicError{Code: source.Code, Message: message}
}

func clonePublicError(source *PublicError) *PublicError {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

func cloneNodeStatus(status NodeStatus) NodeStatus {
	status.LastError = clonePublicError(status.LastError)
	return status
}

func cloneNodeRecord(record nodeRecord) nodeRecord {
	record.status = cloneNodeStatus(record.status)
	return record
}

func internalTransitionError() *model.CodeError {
	return codeError(ErrorCodeInternal, "internal state transition rejected")
}

func codeError(code, message string) *model.CodeError {
	return &model.CodeError{Code: code, Message: message}
}
