package engine

import (
	"math"
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
	Cancelled      bool           `json:"cancelled"`
	ReplacedBy     string         `json:"replaced_by,omitempty"`
	Nodes          []NodeProgress `json:"nodes,omitempty"`
}

type NodeProgress struct {
	NodeID   string             `json:"node_id"`
	Step     string             `json:"step"`
	State    model.RuntimeState `json:"state"`
	Attempt  uint64             `json:"attempt"`
	Deadline time.Time          `json:"deadline,omitempty"`
	Error    *PublicError       `json:"error,omitempty"`
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

type JobStore struct {
	mu           sync.RWMutex
	jobs         map[string]Job
	jobOrder     []string
	events       []NodeEvent
	nextSequence uint64
}

func NewJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]Job)}
}

// Put inserts or replaces a job. Updating an existing job does not change its
// creation order. When full, only the oldest terminal job may be evicted;
// active work is never silently discarded.
func (s *JobStore) Put(job Job) error {
	if !validJob(job) {
		return codeError(ErrorCodeInvalidJob, "job is invalid")
	}
	job = cloneJob(job)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = make(map[string]Job)
	}
	if _, exists := s.jobs[job.ID]; exists {
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
		return NodeEvent{}, codeError(ErrorCodeInvalidJob, "node event is invalid")
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
	if job.Total < 0 || job.Queued < 0 || job.Running < 0 || job.Succeeded < 0 || job.Failed < 0 {
		return false
	}
	switch job.State {
	case JobQueued, JobRunning, JobSucceeded, JobFailed, JobCancelled, JobReplaced:
		return true
	default:
		return false
	}
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
