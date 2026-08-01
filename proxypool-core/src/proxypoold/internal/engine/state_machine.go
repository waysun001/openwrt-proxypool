package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

const (
	ErrorCodeInvalidRequest   = "invalid_request"
	ErrorCodeInternal         = "internal"
	ErrorCodeAuthentication   = "auth_failed"
	ErrorCodeInvalidConfig    = "invalid_config"
	ErrorCodeUnsupported      = "unsupported"
	ErrorCodeWANDown          = "wan_down"
	ErrorCodeConnectTimeout   = "connect_timeout"
	ErrorCodeStopTimeout      = "stop_timeout"
	ErrorCodeCapacityExceeded = "capacity_exceeded"
	ErrorCodeRevisionConflict = "revision_conflict"
	ErrorCodeDuplicate        = "duplicate"
	ErrorCodeNotFound         = "not_found"
	ErrorCodeResolveFailed    = "resolve_failed"
	ErrorCodeProbeFailed      = "probe_failed"
	ErrorCodeDataplaneFailed  = "dataplane_failed"
	ErrorCodeDNSFailed        = "dns_failed"

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
	EventCleanupComplete EventKind = "cleanup_complete"
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

func (event Event) MarshalJSON() ([]byte, error) {
	type wire struct {
		NodeID     string       `json:"node_id"`
		JobID      string       `json:"job_id"`
		Generation uint64       `json:"generation"`
		Kind       EventKind    `json:"kind"`
		Error      *PublicError `json:"error,omitempty"`
		At         time.Time    `json:"at"`
	}
	return json.Marshal(wire{
		NodeID:     event.NodeID,
		JobID:      event.JobID,
		Generation: event.Generation,
		Kind:       event.Kind,
		Error:      publicErrorFromCode(event.Err),
		At:         event.At,
	})
}

func (event Event) String() string {
	return fmt.Sprintf("engine.Event{NodeID:%q JobID:%q Generation:%d Kind:%q Error:%s At:%q}", event.NodeID, event.JobID, event.Generation, event.Kind, publicErrorString(publicErrorFromCode(event.Err)), event.At.Format(time.RFC3339Nano))
}

func (event Event) GoString() string { return event.String() }

func (event Event) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, event.String())
}

type PublicError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (publicError PublicError) MarshalJSON() ([]byte, error) {
	safe := normalizePublicError(&publicError)
	type wire struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	return json.Marshal(wire{Code: safe.Code, Message: safe.Message})
}

func (publicError PublicError) String() string {
	return publicErrorString(normalizePublicError(&publicError))
}

func (publicError PublicError) GoString() string { return publicError.String() }

func (publicError PublicError) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, publicError.String())
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

	CleanupPending   bool `json:"cleanup_pending"`
	ReconnectPending bool `json:"reconnect_pending"`
}

func (status NodeStatus) String() string {
	return fmt.Sprintf("engine.NodeStatus{NodeID:%q JobID:%q Generation:%d State:%q Attempts:%d Error:%s CleanupPending:%t ReconnectPending:%t}", status.NodeID, status.JobID, status.Generation, status.State, status.Attempts, publicErrorString(status.LastError), status.CleanupPending, status.ReconnectPending)
}

func (status NodeStatus) GoString() string { return status.String() }

func (status NodeStatus) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, status.String())
}

type MachineOption func(*Machine)

func WithStableOnlineWindow(window time.Duration) MachineOption {
	return func(machine *Machine) {
		if window > 0 {
			machine.stableOnlineWindow = window
		}
	}
}

func WithClock(clock platform.Clock) MachineOption {
	return func(machine *Machine) {
		if clock != nil {
			machine.clock = clock
		}
	}
}

type nodeRecord struct {
	status              NodeStatus
	onlineSinceElapsed  time.Duration
	onlineSinceSet      bool
	retryReadyElapsed   time.Duration
	retryReadySet       bool
	pendingJobID        string
	resumeAfterRecovery bool
}

type Machine struct {
	mu                 sync.RWMutex
	nodes              map[string]nodeRecord
	retry              *RetryPolicy
	clock              platform.Clock
	stableOnlineWindow time.Duration
}

func NewMachine(retry *RetryPolicy, options ...MachineOption) *Machine {
	if retry == nil {
		retry = NewRetryPolicy(nil)
	}
	machine := &Machine{
		nodes:              make(map[string]nodeRecord),
		retry:              retry,
		clock:              platform.RealClock{},
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
// stored status is not changed. Completion events from an older generation or
// superseded job are intentionally dropped. At is observation metadata only.
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

// restoreNode is used only to hydrate or roll back the controller's durable
// projection. Runtime timers deliberately remain unset: after restart the
// scheduler reconciles uncertain work instead of trusting elapsed timers.
func (m *Machine) restoreNode(nodeID string, status NodeStatus, exists bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !exists {
		delete(m.nodes, nodeID)
		return
	}
	m.nodes[nodeID] = nodeRecord{status: cloneNodeStatus(status)}
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
		record.retryReadySet = false
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
		record.retryReadySet = false
		record.onlineSinceElapsed = m.clock.Monotonic()
		record.onlineSinceSet = true
	case EventDegraded:
		if record.status.State != model.StateOnline {
			return internalTransitionError()
		}
		record.status.State = model.StateDegraded
		record.onlineSinceSet = false
		failure := event.Err
		if failure == nil {
			failure = &model.CodeError{Code: ErrorCodeProbeFailed, Message: "health check failed"}
		}
		record.status.LastError = publicErrorFromCode(failure)
	case EventHealthy:
		if record.status.State != model.StateDegraded {
			return internalTransitionError()
		}
		record.status.State = model.StateOnline
		record.status.LastError = nil
		record.status.RetryAt = time.Time{}
		record.retryReadySet = false
		record.onlineSinceElapsed = m.clock.Monotonic()
		record.onlineSinceSet = true
	case EventStop:
		if !canStop(record.status.State) {
			return internalTransitionError()
		}
		if record.status.State == model.StateStopping || record.status.State == model.StateRecovering {
			// The in-flight stop or recovery operation owns the node until its
			// matching completion releases it.
			// A stop request only changes the desired post-barrier outcome; it
			// must not invalidate or overlap the cleanup already in flight.
			record.status.ReconnectPending = false
			record.pendingJobID = event.JobID
			record.resumeAfterRecovery = false
			return nil
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateStopping
		record.status.CleanupPending = false
		record.status.ReconnectPending = false
		record.pendingJobID = ""
		record.resumeAfterRecovery = false
		record.status.RetryAt = time.Time{}
		record.retryReadySet = false
		record.onlineSinceSet = false
	case EventStopped:
		if record.status.State != model.StateStopping {
			return internalTransitionError()
		}
		if record.status.ReconnectPending {
			return m.releaseBarrierToQueue(record)
		}
		m.releaseBarrierToDisabled(record)
	case EventFailure:
		if event.Err == nil || event.Err.Code == "" {
			return internalTransitionError()
		}
		if record.status.State == model.StateStopping {
			if event.Err.Code != ErrorCodeStopTimeout {
				return internalTransitionError()
			}
			m.enterCleanupBarrier(record, event.Err)
			return nil
		}
		if record.status.State == model.StateRecovering {
			if record.pendingJobID != "" {
				m.enterCleanupBarrier(record, event.Err)
				return nil
			}
			if record.status.CleanupPending {
				if event.Err.Code != ErrorCodeStopTimeout {
					return internalTransitionError()
				}
				m.enterCleanupBarrier(record, event.Err)
				return nil
			}
		}
		if !canFail(record.status.State) {
			return internalTransitionError()
		}
		m.applyFailure(record, event.Err)
	case EventTimeout:
		if record.status.State == model.StateStopping {
			m.enterCleanupBarrier(record, nil)
			return nil
		}
		if record.status.State == model.StateRecovering && (record.status.CleanupPending || record.pendingJobID != "") {
			m.enterCleanupBarrier(record, nil)
			return nil
		}
		if !canTimeout(record.status.State) {
			return internalTransitionError()
		}
		m.applyFailure(record, &model.CodeError{Code: ErrorCodeConnectTimeout, Message: "connection timed out"})
	case EventRetryDue:
		if record.status.State != model.StateBackoff || !record.retryReadySet || m.clock.Monotonic() < record.retryReadyElapsed {
			return internalTransitionError()
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateQueued
		record.status.RetryAt = time.Time{}
		record.retryReadySet = false
		record.onlineSinceSet = false
	case EventWANAvailable:
		if record.status.State != model.StateBackoff || record.retryReadySet || record.status.LastError == nil || record.status.LastError.Code != ErrorCodeWANDown {
			return internalTransitionError()
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateQueued
		record.onlineSinceSet = false
	case EventRecover:
		if !canRecover(record.status.State) {
			return internalTransitionError()
		}
		if err := requireGenerationCapacity(record.status, 2); err != nil {
			return err
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateRecovering
		record.status.CleanupPending = false
		record.status.ReconnectPending = false
		record.pendingJobID = ""
		record.resumeAfterRecovery = true
		record.status.RetryAt = time.Time{}
		record.retryReadySet = false
		record.onlineSinceSet = false
		if event.Err != nil {
			record.status.LastError = publicErrorFromCode(event.Err)
		}
	case EventRecovered:
		if record.status.State != model.StateRecovering || record.status.CleanupPending {
			return internalTransitionError()
		}
		if record.resumeAfterRecovery || record.status.ReconnectPending {
			return m.releaseBarrierToQueue(record)
		}
		if record.pendingJobID == "" {
			return internalTransitionError()
		}
		m.releaseBarrierToDisabled(record)
	case EventCleanupComplete:
		if record.status.State != model.StateRecovering || !record.status.CleanupPending {
			return internalTransitionError()
		}
		if record.status.ReconnectPending {
			return m.releaseBarrierToQueue(record)
		}
		m.releaseBarrierToDisabled(record)
	case EventManualReconnect:
		if !canManuallyReconnect(record.status.State) {
			return internalTransitionError()
		}
		if record.status.State == model.StateStopping || record.status.State == model.StateRecovering {
			if err := requireGenerationCapacity(record.status, 1); err != nil {
				return err
			}
			record.status.ReconnectPending = true
			record.pendingJobID = event.JobID
			record.resumeAfterRecovery = false
			return nil
		}
		if hasActiveIO(record.status.State) {
			if err := requireGenerationCapacity(record.status, 2); err != nil {
				return err
			}
			if err := bumpGeneration(&record.status); err != nil {
				return err
			}
			record.status.JobID = event.JobID
			record.status.State = model.StateStopping
			record.status.CleanupPending = false
			record.status.ReconnectPending = true
			record.pendingJobID = event.JobID
			record.resumeAfterRecovery = false
			record.status.RetryAt = time.Time{}
			record.retryReadySet = false
			record.onlineSinceSet = false
			return nil
		}
		if err := bumpGeneration(&record.status); err != nil {
			return err
		}
		record.status.JobID = event.JobID
		record.status.State = model.StateQueued
		record.status.Attempts = 0
		record.status.LastError = nil
		record.status.RetryAt = time.Time{}
		record.status.CleanupPending = false
		record.status.ReconnectPending = false
		record.pendingJobID = ""
		record.resumeAfterRecovery = false
		record.retryReadySet = false
		record.onlineSinceSet = false
	case EventStableOnline:
		elapsed := m.clock.Monotonic()
		if record.status.State != model.StateOnline || !record.onlineSinceSet || elapsed < record.onlineSinceElapsed || elapsed-record.onlineSinceElapsed < m.stableOnlineWindow {
			return internalTransitionError()
		}
		record.status.Attempts = 0
		record.status.LastError = nil
		record.status.RetryAt = time.Time{}
		record.retryReadySet = false
	default:
		return internalTransitionError()
	}
	return nil
}

func (m *Machine) applyFailure(record *nodeRecord, failure *model.CodeError) {
	decision := m.retry.Next(record.status.Attempts, failure)
	if record.status.Attempts < math.MaxUint64 {
		record.status.Attempts++
	}
	record.status.LastError = publicErrorFromCode(failure)
	record.status.RetryAt = time.Time{}
	record.status.CleanupPending = false
	record.status.ReconnectPending = false
	record.pendingJobID = ""
	record.resumeAfterRecovery = false
	record.retryReadySet = false
	record.onlineSinceSet = false
	switch decision.Mode {
	case RetryAfter:
		record.status.State = model.StateBackoff
		record.status.RetryAt = m.clock.Now().Add(decision.Delay)
		record.retryReadyElapsed = saturatingDurationAdd(m.clock.Monotonic(), decision.Delay)
		record.retryReadySet = true
	case RetryOnWANEvent:
		record.status.State = model.StateBackoff
	default:
		record.status.State = model.StateFailed
	}
}

func (m *Machine) enterCleanupBarrier(record *nodeRecord, failure *model.CodeError) {
	if failure == nil {
		failure = &model.CodeError{Code: ErrorCodeStopTimeout}
	}
	record.status.State = model.StateRecovering
	record.status.CleanupPending = true
	record.resumeAfterRecovery = false
	record.status.LastError = publicErrorFromCode(failure)
	record.status.RetryAt = time.Time{}
	record.retryReadySet = false
	record.onlineSinceSet = false
}

func (m *Machine) releaseBarrierToQueue(record *nodeRecord) error {
	if err := bumpGeneration(&record.status); err != nil {
		return err
	}
	if record.pendingJobID != "" {
		record.status.JobID = record.pendingJobID
	}
	if record.status.ReconnectPending {
		record.status.Attempts = 0
		record.status.LastError = nil
	}
	record.status.State = model.StateQueued
	record.status.CleanupPending = false
	record.status.ReconnectPending = false
	record.status.RetryAt = time.Time{}
	record.pendingJobID = ""
	record.resumeAfterRecovery = false
	record.retryReadySet = false
	record.onlineSinceSet = false
	return nil
}

func (m *Machine) releaseBarrierToDisabled(record *nodeRecord) {
	if record.pendingJobID != "" {
		record.status.JobID = record.pendingJobID
	}
	record.status.State = model.StateDisabled
	record.status.Attempts = 0
	record.status.LastError = nil
	record.status.RetryAt = time.Time{}
	record.status.CleanupPending = false
	record.status.ReconnectPending = false
	record.pendingJobID = ""
	record.resumeAfterRecovery = false
	record.retryReadySet = false
	record.onlineSinceSet = false
}

func saturatingDurationAdd(base, delta time.Duration) time.Duration {
	if delta > 0 && base > time.Duration(math.MaxInt64)-delta {
		return time.Duration(math.MaxInt64)
	}
	return base + delta
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
		EventFailure, EventTimeout, EventRecovered, EventCleanupComplete, EventStableOnline:
		return true
	default:
		return false
	}
}

func validEventKind(kind EventKind) bool {
	switch kind {
	case EventEnable, EventStart, EventStarted, EventValidated, EventDegraded,
		EventHealthy, EventStop, EventStopped, EventFailure, EventTimeout,
		EventRetryDue, EventWANAvailable, EventRecover, EventRecovered, EventCleanupComplete,
		EventManualReconnect, EventStableOnline:
		return true
	default:
		return false
	}
}

func canStop(state model.RuntimeState) bool {
	switch state {
	case model.StateQueued, model.StateStarting, model.StateValidating, model.StateOnline,
		model.StateDegraded, model.StateStopping, model.StateFailed, model.StateBackoff, model.StateRecovering:
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
		model.StateDegraded, model.StateStopping, model.StateFailed, model.StateBackoff, model.StateRecovering:
		return true
	default:
		return false
	}
}

func hasActiveIO(state model.RuntimeState) bool {
	switch state {
	case model.StateStarting, model.StateValidating, model.StateOnline, model.StateDegraded:
		return true
	default:
		return false
	}
}

func bumpGeneration(status *NodeStatus) error {
	if err := requireGenerationCapacity(*status, 1); err != nil {
		return err
	}
	status.Generation++
	return nil
}

func requireGenerationCapacity(status NodeStatus, increments uint64) error {
	if increments > math.MaxUint64-status.Generation {
		return internalTransitionError()
	}
	return nil
}

func publicErrorFromCode(source *model.CodeError) *PublicError {
	if source == nil {
		return nil
	}
	return publicErrorForCode(source.Code)
}

func clonePublicError(source *PublicError) *PublicError {
	return normalizePublicError(source)
}

func normalizePublicError(source *PublicError) *PublicError {
	if source == nil {
		return nil
	}
	return publicErrorForCode(source.Code)
}

func publicErrorForCode(code string) *PublicError {
	message := ""
	switch code {
	case ErrorCodeInvalidRequest:
		message = "invalid request"
	case ErrorCodeInvalidConfig:
		message = "configuration is invalid"
	case ErrorCodeRevisionConflict:
		message = "configuration revision conflicts"
	case ErrorCodeCapacityExceeded:
		message = "capacity is exhausted"
	case ErrorCodeDuplicate:
		message = "object already exists"
	case ErrorCodeNotFound:
		message = "object was not found"
	case ErrorCodeAuthentication:
		message = "authentication failed"
	case ErrorCodeResolveFailed:
		message = "endpoint resolution failed"
	case ErrorCodeConnectTimeout:
		message = "connection timed out"
	case ErrorCodeStopTimeout:
		message = "stop timed out"
	case ErrorCodeProbeFailed:
		message = "connection probe failed"
	case ErrorCodeWANDown:
		message = "WAN is unavailable"
	case ErrorCodeDataplaneFailed:
		message = "dataplane update failed"
	case ErrorCodeDNSFailed:
		message = "DNS validation failed"
	case ErrorCodeUnsupported:
		message = "protocol option is unsupported"
	case ErrorCodeInternal:
		message = "internal node error"
	default:
		code = ErrorCodeInternal
		message = "internal node error"
	}
	return &PublicError{Code: code, Message: message}
}

func publicErrorString(publicError *PublicError) string {
	if publicError == nil {
		return "<nil>"
	}
	safe := normalizePublicError(publicError)
	return fmt.Sprintf("engine.PublicError{Code:%q Message:%q}", safe.Code, safe.Message)
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
