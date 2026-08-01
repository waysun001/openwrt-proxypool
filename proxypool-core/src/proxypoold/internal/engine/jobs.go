package engine

import (
	"fmt"
	"io"
	"math"
	"reflect"
	"sync"
	"time"

	"proxypoold/internal/model"
)

const (
	MaxRetainedJobs       = 256
	MaxRetainedNodeEvents = 2048
)

type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
	JobReplaced  JobState = "replaced"
)

type Job struct {
	ID             string         `json:"id"`
	Kind           string         `json:"kind"`
	Creator        string         `json:"creator"`
	CreatedAt      time.Time      `json:"created_at"`
	ConfigRevision uint64         `json:"config_revision"`
	State          JobState       `json:"state"`
	Total          int            `json:"total"`
	Queued         int            `json:"queued"`
	Running        int            `json:"running"`
	Succeeded      int            `json:"succeeded"`
	Failed         int            `json:"failed"`
	CancelledNodes int            `json:"cancelled_nodes"`
	Cancelled      bool           `json:"cancelled"`
	ReplacedBy     string         `json:"replaced_by,omitempty"`
	Nodes          []NodeProgress `json:"nodes,omitempty"`
}

func (job Job) String() string {
	return fmt.Sprintf("engine.Job{ID:%q Kind:%q State:%q Total:%d Queued:%d Running:%d Succeeded:%d Failed:%d CancelledNodes:%d Nodes:<redacted>}", job.ID, job.Kind, job.State, job.Total, job.Queued, job.Running, job.Succeeded, job.Failed, job.CancelledNodes)
}

func (job Job) GoString() string { return job.String() }

func (job Job) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, job.String())
}

type NodeProgress struct {
	NodeID    string             `json:"node_id"`
	Step      string             `json:"step"`
	State     model.RuntimeState `json:"state"`
	Attempt   uint64             `json:"attempt"`
	Deadline  time.Time          `json:"deadline,omitempty"`
	Error     *PublicError       `json:"error,omitempty"`
	Cancelled bool               `json:"cancelled"`
}

func (progress NodeProgress) String() string {
	return fmt.Sprintf("engine.NodeProgress{NodeID:%q State:%q Attempt:%d Cancelled:%t Error:%s}", progress.NodeID, progress.State, progress.Attempt, progress.Cancelled, publicErrorString(progress.Error))
}

func (progress NodeProgress) GoString() string { return progress.String() }

func (progress NodeProgress) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, progress.String())
}

type NodeEvent struct {
	Sequence   uint64             `json:"sequence"`
	JobID      string             `json:"job_id"`
	NodeID     string             `json:"node_id"`
	Generation uint64             `json:"generation"`
	State      model.RuntimeState `json:"state"`
	Attempt    uint64             `json:"attempt"`
	At         time.Time          `json:"at"`
	Error      *PublicError       `json:"error,omitempty"`
}

func (event NodeEvent) String() string {
	return fmt.Sprintf("engine.NodeEvent{Sequence:%d JobID:%q NodeID:%q Generation:%d State:%q Attempt:%d Error:%s At:%q}", event.Sequence, event.JobID, event.NodeID, event.Generation, event.State, event.Attempt, publicErrorString(event.Error), event.At.Format(time.RFC3339Nano))
}

func (event NodeEvent) GoString() string { return event.String() }

func (event NodeEvent) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, event.String())
}

type JobStore struct {
	mu           sync.RWMutex
	jobs         map[string]Job
	jobOrder     []string
	events       []NodeEvent
	nextSequence uint64
}

// JobSnapshot is the complete bounded, credential-free durable projection of
// JobStore. Jobs and events retain insertion order; NextEventSequence prevents
// sequence reuse after a daemon restart.
type JobSnapshot struct {
	Jobs              []Job       `json:"jobs"`
	Events            []NodeEvent `json:"events"`
	NextEventSequence uint64      `json:"next_event_sequence"`
}

func NewJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]Job)}
}

// Snapshot returns an independent copy suitable for durable storage.
func (s *JobStore) Snapshot() JobSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

// Restore replaces the retained state only after the entire snapshot has been
// validated. Invalid snapshots never partially mutate live work.
func (s *JobStore) Restore(snapshot JobSnapshot) error {
	normalized, err := normalizeJobSnapshot(snapshot)
	if err != nil {
		return err
	}

	jobs := make(map[string]Job, len(normalized.Jobs))
	order := make([]string, 0, len(normalized.Jobs))
	for _, job := range normalized.Jobs {
		jobs[job.ID] = cloneJob(job)
		order = append(order, job.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = jobs
	s.jobOrder = order
	s.events = cloneNodeEvents(normalized.Events)
	s.nextSequence = normalized.NextEventSequence
	return nil
}

func (s *JobStore) snapshotLocked() JobSnapshot {
	jobs := make([]Job, 0, len(s.jobOrder))
	for _, id := range s.jobOrder {
		jobs = append(jobs, cloneJob(s.jobs[id]))
	}
	return JobSnapshot{
		Jobs:              jobs,
		Events:            cloneNodeEvents(s.events),
		NextEventSequence: s.nextSequence,
	}
}

func normalizeJobSnapshot(snapshot JobSnapshot) (JobSnapshot, error) {
	if len(snapshot.Jobs) > MaxRetainedJobs || len(snapshot.Events) > MaxRetainedNodeEvents {
		return JobSnapshot{}, codeError(ErrorCodeCapacityExceeded, "runtime snapshot capacity is exceeded")
	}
	normalized := JobSnapshot{
		Jobs:              make([]Job, 0, len(snapshot.Jobs)),
		Events:            make([]NodeEvent, 0, len(snapshot.Events)),
		NextEventSequence: snapshot.NextEventSequence,
	}
	seenJobs := make(map[string]struct{}, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		if !validJob(job) {
			return JobSnapshot{}, codeError(ErrorCodeInvalidConfig, "runtime job snapshot is invalid")
		}
		if _, exists := seenJobs[job.ID]; exists {
			return JobSnapshot{}, codeError(ErrorCodeDuplicate, "runtime job snapshot contains a duplicate")
		}
		seenJobs[job.ID] = struct{}{}
		normalized.Jobs = append(normalized.Jobs, cloneJob(job))
	}

	var previousSequence uint64
	for _, event := range snapshot.Events {
		if !validStoredNodeEvent(event) || event.Sequence <= previousSequence {
			return JobSnapshot{}, codeError(ErrorCodeInvalidConfig, "runtime event snapshot is invalid")
		}
		previousSequence = event.Sequence
		normalized.Events = append(normalized.Events, cloneNodeEvent(event))
	}
	if len(normalized.Events) > 0 && previousSequence != normalized.NextEventSequence {
		return JobSnapshot{}, codeError(ErrorCodeInvalidConfig, "runtime event sequence is invalid")
	}
	return normalized, nil
}

func validStoredNodeEvent(event NodeEvent) bool {
	return event.Sequence > 0 && event.JobID != "" && event.NodeID != "" && event.Generation > 0 &&
		!event.At.IsZero() && validRuntimeState(event.State)
}

func cloneNodeEvents(events []NodeEvent) []NodeEvent {
	cloned := make([]NodeEvent, len(events))
	for index := range events {
		cloned[index] = cloneNodeEvent(events[index])
	}
	return cloned
}

// Put inserts a job or advances an existing job through a legal monotonic
// update. Updating an existing job does not change its creation order. When
// full, only the oldest terminal job may be evicted; active work is never
// silently discarded.
func (s *JobStore) Put(job Job) error {
	if !validJob(job) {
		return codeError(ErrorCodeInvalidConfig, "job is invalid")
	}
	job = cloneJob(job)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = make(map[string]Job)
	}
	if current, exists := s.jobs[job.ID]; exists {
		if !sameJobIdentity(current, job) {
			return codeError(ErrorCodeDuplicate, "job ID is already used by different work")
		}
		if !legalJobUpdate(current, job) {
			return codeError(ErrorCodeRevisionConflict, "job update conflicts with retained progress")
		}
		s.jobs[job.ID] = job
		return nil
	}
	if len(s.jobOrder) == MaxRetainedJobs {
		evictionIndex := -1
		for index, id := range s.jobOrder {
			if isTerminalJob(s.jobs[id].State) {
				evictionIndex = index
				break
			}
		}
		if evictionIndex < 0 {
			return codeError(ErrorCodeCapacityExceeded, "job capacity is exhausted")
		}
		delete(s.jobs, s.jobOrder[evictionIndex])
		copy(s.jobOrder[evictionIndex:], s.jobOrder[evictionIndex+1:])
		s.jobOrder = s.jobOrder[:len(s.jobOrder)-1]
	}
	s.jobs[job.ID] = job
	s.jobOrder = append(s.jobOrder, job.ID)
	return nil
}

func (s *JobStore) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return cloneJob(job), true
}

// List returns retained jobs from oldest to newest insertion.
func (s *JobStore) List() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	jobs := make([]Job, 0, len(s.jobOrder))
	for _, id := range s.jobOrder {
		jobs = append(jobs, cloneJob(s.jobs[id]))
	}
	return jobs
}

func (s *JobStore) AppendEvent(event NodeEvent) (NodeEvent, error) {
	if event.JobID == "" || event.NodeID == "" || event.Generation == 0 || event.At.IsZero() || !validRuntimeState(event.State) {
		return NodeEvent{}, codeError(ErrorCodeInvalidConfig, "node event is invalid")
	}
	event = cloneNodeEvent(event)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextSequence == math.MaxUint64 {
		return NodeEvent{}, codeError(ErrorCodeCapacityExceeded, "event sequence is exhausted")
	}
	s.nextSequence++
	event.Sequence = s.nextSequence
	if len(s.events) == MaxRetainedNodeEvents {
		copy(s.events, s.events[1:])
		s.events[len(s.events)-1] = event
	} else {
		s.events = append(s.events, event)
	}
	return cloneNodeEvent(event), nil
}

// Events returns retained events from oldest to newest insertion.
func (s *JobStore) Events() []NodeEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	events := make([]NodeEvent, len(s.events))
	for index := range s.events {
		events[index] = cloneNodeEvent(s.events[index])
	}
	return events
}

func validJob(job Job) bool {
	if job.ID == "" || job.Kind == "" || job.Creator == "" || job.CreatedAt.IsZero() {
		return false
	}
	if job.Total < 0 || job.Queued < 0 || job.Running < 0 || job.Succeeded < 0 || job.Failed < 0 || job.CancelledNodes < 0 {
		return false
	}
	if job.Queued > job.Total || job.Running > job.Total || job.Succeeded > job.Total || job.Failed > job.Total || job.CancelledNodes > job.Total {
		return false
	}
	if job.Queued+job.Running+job.Succeeded+job.Failed+job.CancelledNodes != job.Total || len(job.Nodes) != job.Total {
		return false
	}
	derivedQueued, derivedRunning, derivedSucceeded, derivedFailed, derivedCancelled := 0, 0, 0, 0, 0
	seenNodes := make(map[string]struct{}, len(job.Nodes))
	for _, node := range job.Nodes {
		if node.NodeID == "" || node.Step == "" || !validRuntimeState(node.State) {
			return false
		}
		if _, exists := seenNodes[node.NodeID]; exists {
			return false
		}
		seenNodes[node.NodeID] = struct{}{}
		switch nodeProgressBucket(node) {
		case jobBucketQueued:
			derivedQueued++
		case jobBucketRunning:
			derivedRunning++
		case jobBucketSucceeded:
			derivedSucceeded++
		case jobBucketFailed:
			if node.Error == nil {
				return false
			}
			derivedFailed++
		case jobBucketCancelled:
			if node.Error != nil {
				return false
			}
			derivedCancelled++
		default:
			return false
		}
	}
	if job.Queued != derivedQueued || job.Running != derivedRunning || job.Succeeded != derivedSucceeded || job.Failed != derivedFailed || job.CancelledNodes != derivedCancelled {
		return false
	}
	switch job.State {
	case JobQueued:
		return job.Total > 0 && job.Queued == job.Total && job.CancelledNodes == 0 && !job.Cancelled && job.ReplacedBy == ""
	case JobRunning:
		return job.Queued+job.Running > 0 && !job.Cancelled && job.ReplacedBy == ""
	case JobSucceeded:
		return job.Succeeded == job.Total && job.Queued == 0 && job.Running == 0 && job.Failed == 0 && job.CancelledNodes == 0 && !job.Cancelled && job.ReplacedBy == ""
	case JobFailed:
		return job.Total > 0 && job.Failed > 0 && job.Queued == 0 && job.Running == 0 && job.CancelledNodes == 0 && job.Succeeded+job.Failed == job.Total && !job.Cancelled && job.ReplacedBy == ""
	case JobCancelled:
		return job.Cancelled && job.CancelledNodes > 0 && job.ReplacedBy == "" && job.Queued == 0 && job.Running == 0
	case JobReplaced:
		return job.Cancelled && job.CancelledNodes > 0 && job.ReplacedBy != "" && job.ReplacedBy != job.ID && job.Queued == 0 && job.Running == 0
	default:
		return false
	}
}

type jobBucket uint8

const (
	jobBucketInvalid jobBucket = iota
	jobBucketQueued
	jobBucketRunning
	jobBucketSucceeded
	jobBucketFailed
	jobBucketCancelled
)

func nodeProgressBucket(progress NodeProgress) jobBucket {
	if progress.Cancelled {
		return jobBucketCancelled
	}
	return nodeJobBucket(progress.State)
}

func nodeJobBucket(state model.RuntimeState) jobBucket {
	switch state {
	case model.StateQueued, model.StateBackoff:
		return jobBucketQueued
	case model.StateStarting, model.StateValidating, model.StateStopping, model.StateRecovering:
		return jobBucketRunning
	case model.StateOnline, model.StateDisabled:
		return jobBucketSucceeded
	case model.StateDegraded, model.StateFailed:
		return jobBucketFailed
	default:
		return jobBucketInvalid
	}
}

func sameJobIdentity(current, candidate Job) bool {
	if current.ID != candidate.ID || current.Kind != candidate.Kind || current.Creator != candidate.Creator ||
		!current.CreatedAt.Equal(candidate.CreatedAt) || current.ConfigRevision != candidate.ConfigRevision ||
		current.Total != candidate.Total || len(current.Nodes) != len(candidate.Nodes) {
		return false
	}
	for index := range current.Nodes {
		if current.Nodes[index].NodeID != candidate.Nodes[index].NodeID {
			return false
		}
	}
	return true
}

func legalJobUpdate(current, candidate Job) bool {
	if isTerminalJob(current.State) {
		return reflect.DeepEqual(current, candidate)
	}
	if candidate.Succeeded < current.Succeeded || candidate.Failed < current.Failed || candidate.CancelledNodes < current.CancelledNodes ||
		!legalJobStateTransition(current.State, candidate.State) {
		return false
	}
	for index := range current.Nodes {
		oldNode := current.Nodes[index]
		newNode := candidate.Nodes[index]
		if !legalNodeProgressUpdate(oldNode, newNode) {
			return false
		}
	}
	return true
}

func legalNodeProgressUpdate(current, candidate NodeProgress) bool {
	if current.NodeID != candidate.NodeID || candidate.Attempt < current.Attempt {
		return false
	}
	if current.Cancelled {
		return reflect.DeepEqual(current, candidate)
	}
	currentBucket := nodeJobBucket(current.State)
	if currentBucket == jobBucketSucceeded || currentBucket == jobBucketFailed {
		// Once this job has recorded a node outcome, the complete evidence for
		// that outcome is immutable, not merely its runtime-state label.
		return reflect.DeepEqual(current, candidate)
	}
	if candidate.Cancelled {
		// Cancellation is an outcome for the current observation, not a new
		// attempt or a license to fabricate another runtime state.
		return candidate.State == current.State && candidate.Attempt == current.Attempt
	}
	if candidate.Attempt > current.Attempt {
		// A higher attempt begins a new per-node lifecycle. validJob has already
		// checked the candidate state and its aggregate bucket.
		return true
	}
	return legalSameAttemptNodeTransition(current.State, candidate.State)
}

func legalSameAttemptNodeTransition(current, candidate model.RuntimeState) bool {
	if current == candidate {
		return true
	}
	switch current {
	case model.StateQueued:
		// A store update may coalesce intermediate states after initial queueing.
		return validRuntimeState(candidate)
	case model.StateBackoff:
		switch candidate {
		case model.StateQueued, model.StateStarting, model.StateValidating, model.StateOnline,
			model.StateDegraded, model.StateStopping, model.StateFailed, model.StateRecovering,
			model.StateDisabled:
			return true
		}
	case model.StateStarting:
		switch candidate {
		case model.StateValidating, model.StateOnline, model.StateDegraded, model.StateStopping,
			model.StateFailed, model.StateBackoff, model.StateRecovering, model.StateDisabled:
			return true
		}
	case model.StateValidating:
		switch candidate {
		case model.StateOnline, model.StateDegraded, model.StateStopping, model.StateFailed,
			model.StateBackoff, model.StateRecovering, model.StateDisabled:
			return true
		}
	case model.StateStopping:
		switch candidate {
		case model.StateDisabled, model.StateRecovering, model.StateQueued:
			return true
		}
	case model.StateRecovering:
		switch candidate {
		case model.StateQueued, model.StateDisabled, model.StateBackoff, model.StateFailed:
			return true
		}
	}
	return false
}

func legalJobStateTransition(current, candidate JobState) bool {
	switch current {
	case JobQueued:
		switch candidate {
		case JobQueued, JobRunning, JobSucceeded, JobFailed, JobCancelled, JobReplaced:
			return true
		}
	case JobRunning:
		switch candidate {
		case JobRunning, JobSucceeded, JobFailed, JobCancelled, JobReplaced:
			return true
		}
	}
	return false
}

func isTerminalJob(state JobState) bool {
	switch state {
	case JobSucceeded, JobFailed, JobCancelled, JobReplaced:
		return true
	default:
		return false
	}
}

func validRuntimeState(state model.RuntimeState) bool {
	switch state {
	case model.StateDisabled, model.StateQueued, model.StateStarting, model.StateValidating,
		model.StateOnline, model.StateDegraded, model.StateStopping, model.StateFailed,
		model.StateBackoff, model.StateRecovering:
		return true
	default:
		return false
	}
}

func cloneJob(job Job) Job {
	job.Nodes = append([]NodeProgress(nil), job.Nodes...)
	for index := range job.Nodes {
		job.Nodes[index].Error = clonePublicError(job.Nodes[index].Error)
	}
	return job
}

func cloneNodeEvent(event NodeEvent) NodeEvent {
	event.Error = clonePublicError(event.Error)
	return event
}
