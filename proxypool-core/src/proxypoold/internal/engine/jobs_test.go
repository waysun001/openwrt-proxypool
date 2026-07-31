package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/model"
)

func TestJobStoreRetainsNewest256CompletedJobsInInsertionOrder(t *testing.T) {
	store := NewJobStore()
	for index := 0; index < 300; index++ {
		job := succeededJob(testJob(index))
		if err := store.Put(job); err != nil {
			t.Fatalf("Put(%s) error = %v", job.ID, err)
		}
	}

	jobs := store.List()
	if len(jobs) != MaxRetainedJobs {
		t.Fatalf("len(List()) = %d, want %d", len(jobs), MaxRetainedJobs)
	}
	if jobs[0].ID != "job-044" || jobs[len(jobs)-1].ID != "job-299" {
		t.Fatalf("retained range = %q..%q, want job-044..job-299", jobs[0].ID, jobs[len(jobs)-1].ID)
	}
	if _, ok := store.Get("job-043"); ok {
		t.Fatal("evicted job-043 is still present")
	}
}

func TestJobStoreUpdateDoesNotPromoteOldJobDuringEviction(t *testing.T) {
	store := NewJobStore()
	for index := 0; index < MaxRetainedJobs; index++ {
		job := succeededJob(testJob(index))
		if err := store.Put(job); err != nil {
			t.Fatal(err)
		}
	}
	updated := succeededJob(testJob(0))
	if err := store.Put(updated); err != nil {
		t.Fatal(err)
	}
	newest := succeededJob(testJob(MaxRetainedJobs))
	if err := store.Put(newest); err != nil {
		t.Fatal(err)
	}

	if _, ok := store.Get("job-000"); ok {
		t.Fatal("updating the oldest job incorrectly promoted it")
	}
	jobs := store.List()
	if jobs[0].ID != "job-001" || jobs[len(jobs)-1].ID != "job-256" {
		t.Fatalf("retained range = %q..%q, want job-001..job-256", jobs[0].ID, jobs[len(jobs)-1].ID)
	}
}

func TestJobStoreNeverSilentlyEvictsActiveJobs(t *testing.T) {
	store := NewJobStore()
	for index := 0; index < MaxRetainedJobs; index++ {
		job := runningJob(testJob(index))
		if err := store.Put(job); err != nil {
			t.Fatal(err)
		}
	}
	before := store.List()

	err := store.Put(testJob(MaxRetainedJobs))
	var codeErr *model.CodeError
	if !errors.As(err, &codeErr) || codeErr.Code != ErrorCodeCapacityExceeded {
		t.Fatalf("Put() error = %#v, want %q", err, ErrorCodeCapacityExceeded)
	}
	after := store.List()
	if !reflect.DeepEqual(after, before) {
		t.Fatal("capacity error changed active jobs")
	}
}

func TestJobStoreEvictsOldestTerminalJobAroundActiveJobs(t *testing.T) {
	store := NewJobStore()
	for index := 0; index < MaxRetainedJobs; index++ {
		job := runningJob(testJob(index))
		if index == 5 || index == 9 {
			job = succeededJob(testJob(index))
		}
		if err := store.Put(job); err != nil {
			t.Fatal(err)
		}
	}
	newJob := runningJob(testJob(MaxRetainedJobs))
	if err := store.Put(newJob); err != nil {
		t.Fatal(err)
	}

	if _, ok := store.Get("job-005"); ok {
		t.Fatal("oldest terminal job was not evicted")
	}
	if _, ok := store.Get("job-000"); !ok {
		t.Fatal("older active job was evicted")
	}
	if _, ok := store.Get("job-009"); !ok {
		t.Fatal("newer terminal job was evicted before oldest terminal job")
	}
}

func TestJobStoreEvictsOldestCancelledOrReplacedJob(t *testing.T) {
	for _, terminalState := range []JobState{JobCancelled, JobReplaced} {
		t.Run(string(terminalState), func(t *testing.T) {
			store := NewJobStore()
			for index := 0; index < MaxRetainedJobs; index++ {
				if err := store.Put(runningJob(testJob(index))); err != nil {
					t.Fatal(err)
				}
			}
			terminal := runningJob(testJob(0))
			terminal.State = terminalState
			terminal.Running = 0
			terminal.CancelledNodes = 1
			terminal.Cancelled = true
			terminal.Nodes[0].Step = "cancelled"
			terminal.Nodes[0].Cancelled = true
			if terminalState == JobReplaced {
				terminal.ReplacedBy = "job-replacement"
			}
			if err := store.Put(terminal); err != nil {
				t.Fatalf("Put(%s update) error = %v", terminalState, err)
			}
			if err := store.Put(runningJob(testJob(MaxRetainedJobs))); err != nil {
				t.Fatal(err)
			}
			if _, exists := store.Get(terminal.ID); exists {
				t.Fatalf("oldest %s job was not evicted", terminalState)
			}
			jobs := store.List()
			if jobs[0].ID != "job-001" || jobs[len(jobs)-1].ID != "job-256" {
				t.Fatalf("retained range = %q..%q, want job-001..job-256", jobs[0].ID, jobs[len(jobs)-1].ID)
			}
		})
	}
}

func TestJobStoreRetainsNewest2048EventsWithMonotonicSequence(t *testing.T) {
	store := NewJobStore()
	for index := 0; index < 2100; index++ {
		stored, err := store.AppendEvent(NodeEvent{
			JobID:      "job-a",
			NodeID:     fmt.Sprintf("node-%04d", index),
			Generation: 1,
			State:      model.StateQueued,
			At:         stateTestEpoch.Add(time.Duration(index) * time.Second),
		})
		if err != nil {
			t.Fatalf("AppendEvent(%d) error = %v", index, err)
		}
		if stored.Sequence != uint64(index+1) {
			t.Fatalf("AppendEvent(%d) sequence = %d, want %d", index, stored.Sequence, index+1)
		}
	}

	events := store.Events()
	if len(events) != MaxRetainedNodeEvents {
		t.Fatalf("len(Events()) = %d, want %d", len(events), MaxRetainedNodeEvents)
	}
	if events[0].Sequence != 53 || events[0].NodeID != "node-0052" {
		t.Fatalf("first retained event = %#v, want sequence 53/node-0052", events[0])
	}
	if events[len(events)-1].Sequence != 2100 || events[len(events)-1].NodeID != "node-2099" {
		t.Fatalf("last retained event = %#v, want sequence 2100/node-2099", events[len(events)-1])
	}
}

func TestJobStoreCopiesInputsAndOutputs(t *testing.T) {
	store := NewJobStore()
	job := failedJob(testJob(1))
	job.Nodes[0] = NodeProgress{
		NodeID: job.Nodes[0].NodeID,
		Step:   "validate",
		State:  model.StateFailed,
		Error:  &PublicError{Code: ErrorCodeAuthentication, Message: "credentials rejected"},
	}
	if err := store.Put(job); err != nil {
		t.Fatal(err)
	}
	job.Nodes[0].Step = "mutated input"
	job.Nodes[0].Error.Message = "mutated input"

	first, ok := store.Get(job.ID)
	if !ok {
		t.Fatal("Get() did not find job")
	}
	first.Nodes[0].Step = "mutated output"
	first.Nodes[0].Error.Message = "mutated output"
	second, _ := store.Get(job.ID)
	if second.Nodes[0].Step != "validate" || second.Nodes[0].Error.Message != "authentication failed" {
		t.Fatalf("stored job was aliased: %#v", second)
	}

	inputError := &PublicError{Code: "temporary", Message: "adapter unavailable"}
	_, err := store.AppendEvent(NodeEvent{JobID: job.ID, NodeID: "node-a", Generation: 1, State: model.StateBackoff, Attempt: 1, Error: inputError, At: stateTestEpoch})
	if err != nil {
		t.Fatal(err)
	}
	inputError.Message = "mutated input"
	events := store.Events()
	events[0].Error.Message = "mutated output"
	if got := store.Events()[0].Error.Message; got != "internal node error" {
		t.Fatalf("stored event error = %q, want independent copy", got)
	}
}

func TestJobAndEventDTOsCannotContainNodeCredentials(t *testing.T) {
	for _, value := range []any{Job{}, NodeProgress{}, NodeEvent{}, NodeStatus{}} {
		assertNoCredentialFields(t, reflect.TypeOf(value), map[reflect.Type]bool{})
	}

	job := testJob(1)
	job = runningJob(job)
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"password", "token", "obfs_key"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("public JSON contains credential field %q: %s", forbidden, encoded)
		}
	}
}

func TestPublicRuntimeDTOsRedactCredentialBearingErrorsInJSONAndFormatting(t *testing.T) {
	const secret = "credential-DO-NOT-LEAK"
	unsafeError := &PublicError{Code: "adapter_" + secret, Message: "password=" + secret}
	rawError := &model.CodeError{Code: "adapter_" + secret, Message: "token=" + secret}
	job := testJob(1)
	job.Nodes = []NodeProgress{{NodeID: "node-a", Step: "connect", State: model.StateQueued, Error: unsafeError}}
	event := Event{NodeID: "node-a", JobID: job.ID, Generation: 1, Kind: EventFailure, Err: rawError, At: stateTestEpoch}
	nodeEvent := NodeEvent{Sequence: 1, JobID: job.ID, NodeID: "node-a", Generation: 1, State: model.StateFailed, Attempt: 1, At: stateTestEpoch, Error: unsafeError}
	status := NodeStatus{NodeID: "node-a", JobID: job.ID, Generation: 1, State: model.StateFailed, LastError: unsafeError, UpdatedAt: stateTestEpoch}

	for label, value := range map[string]any{
		"public error": unsafeError,
		"raw event":    event,
		"job":          job,
		"node event":   nodeEvent,
		"node status":  status,
	} {
		t.Run(label, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("JSON leaked credential: %s", encoded)
			}
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
				formatted := fmt.Sprintf(verb, value)
				if strings.Contains(formatted, secret) {
					t.Fatalf("format %q leaked credential: %s", verb, formatted)
				}
			}
		})
	}
}

func TestJobStoreSanitizesErrorsBeforeRetention(t *testing.T) {
	const secret = "credential-DO-NOT-RETAIN"
	store := NewJobStore()
	job := testJob(1)
	job.Nodes = []NodeProgress{{NodeID: "node-a", Step: "connect", State: model.StateQueued, Error: &PublicError{Code: "raw_" + secret, Message: secret}}}
	if err := store.Put(job); err != nil {
		t.Fatal(err)
	}
	storedJob, _ := store.Get(job.ID)
	if storedJob.Nodes[0].Error == nil || storedJob.Nodes[0].Error.Code != ErrorCodeInternal || storedJob.Nodes[0].Error.Message != "internal node error" {
		t.Fatalf("stored job error = %#v, want sanitized internal", storedJob.Nodes[0].Error)
	}

	_, err := store.AppendEvent(NodeEvent{JobID: job.ID, NodeID: "node-a", Generation: 1, State: model.StateFailed, Attempt: 1, At: stateTestEpoch, Error: &PublicError{Code: "raw_" + secret, Message: secret}})
	if err != nil {
		t.Fatal(err)
	}
	storedEvent := store.Events()[0]
	if storedEvent.Error == nil || storedEvent.Error.Code != ErrorCodeInternal || storedEvent.Error.Message != "internal node error" {
		t.Fatalf("stored event error = %#v, want sanitized internal", storedEvent.Error)
	}
}

func TestJobStoreRejectsImpossibleSnapshotsAtomically(t *testing.T) {
	twoNodes := testJob(100)
	twoNodes.Total = 2
	twoNodes.Queued = 2
	twoNodes.Nodes = append(twoNodes.Nodes, NodeProgress{NodeID: "node-extra", Step: "queued", State: model.StateQueued})
	cancelledWithError := testJob(112)
	cancelledWithError.State = JobCancelled
	cancelledWithError.Queued = 0
	cancelledWithError.CancelledNodes = 1
	cancelledWithError.Cancelled = true
	cancelledWithError.Nodes[0].Cancelled = true
	cancelledWithError.Nodes[0].Error = &PublicError{Code: ErrorCodeInternal, Message: "internal node error"}
	zeroCancelled := succeededJob(testJob(113))
	zeroCancelled.State = JobCancelled
	zeroCancelled.Cancelled = true
	zeroReplaced := succeededJob(testJob(114))
	zeroReplaced.State = JobReplaced
	zeroReplaced.Cancelled = true
	zeroReplaced.ReplacedBy = "job-next"
	tests := []struct {
		name string
		job  Job
	}{
		{"aggregate sum exceeds total", func() Job { job := testJob(101); job.Running = 1; return job }()},
		{"succeeded job has queued work", func() Job { job := testJob(102); job.State = JobSucceeded; return job }()},
		{"cancel flag on live job", func() Job { job := testJob(103); job.Cancelled = true; return job }()},
		{"cancelled state missing flag", func() Job { job := failedJob(testJob(104)); job.State = JobCancelled; return job }()},
		{"replaced state missing replacement", func() Job { job := failedJob(testJob(105)); job.State = JobReplaced; job.Cancelled = true; return job }()},
		{"replacement on ordinary state", func() Job { job := testJob(106); job.ReplacedBy = "job-new"; return job }()},
		{"node count differs from total", func() Job { job := testJob(107); job.Nodes = nil; return job }()},
		{"node identity empty", func() Job { job := testJob(108); job.Nodes[0].NodeID = ""; return job }()},
		{"node identities duplicate", func() Job { job := twoNodes; job.ID = "job-109"; job.Nodes[1].NodeID = job.Nodes[0].NodeID; return job }()},
		{"node runtime state invalid", func() Job { job := testJob(110); job.Nodes[0].State = model.RuntimeState("unknown"); return job }()},
		{"node state disagrees with aggregate", func() Job { job := testJob(111); job.Nodes[0].State = model.StateStarting; return job }()},
		{"cancelled outcome carries failure", cancelledWithError},
		{"cancelled job has no cancelled outcome", zeroCancelled},
		{"replaced job has no cancelled outcome", zeroReplaced},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewJobStore()
			baseline := testJob(0)
			if err := store.Put(baseline); err != nil {
				t.Fatal(err)
			}
			before := store.List()
			assertJobCode(t, store.Put(test.job), ErrorCodeInvalidConfig)
			if after := store.List(); !reflect.DeepEqual(after, before) {
				t.Fatalf("invalid snapshot mutated store:\n got: %#v\nwant: %#v", after, before)
			}
		})
	}
}

func TestJobStoreAcceptsConsistentCancellationAndReplacementSnapshots(t *testing.T) {
	store := NewJobStore()
	cancelled := testJob(120)
	cancelled.State = JobCancelled
	cancelled.Queued = 0
	cancelled.CancelledNodes = 1
	cancelled.Cancelled = true
	cancelled.Nodes[0].Step = "cancelled"
	cancelled.Nodes[0].Cancelled = true
	if err := store.Put(cancelled); err != nil {
		t.Fatalf("Put(cancelled) error = %v", err)
	}
	replaced := testJob(121)
	replaced.State = JobReplaced
	replaced.Queued = 0
	replaced.CancelledNodes = 1
	replaced.Cancelled = true
	replaced.ReplacedBy = "job-122"
	replaced.Nodes[0].Step = "replaced"
	replaced.Nodes[0].Cancelled = true
	if err := store.Put(replaced); err != nil {
		t.Fatalf("Put(replaced) error = %v", err)
	}
}

func TestJobStoreRepresentsCancelledAndReplacedNodesTruthfully(t *testing.T) {
	const secret = "cancel-outcome-secret"
	tests := []struct {
		name string
		job  Job
	}{
		{
			name: "cancelled",
			job: Job{
				ID: "job-cancelled", Kind: "reconcile", Creator: "operator", CreatedAt: stateTestEpoch,
				ConfigRevision: 1, State: JobCancelled, Total: 3, Succeeded: 1, CancelledNodes: 2, Cancelled: true,
				Nodes: []NodeProgress{
					{NodeID: "node-complete", Step: "done", State: model.StateOnline, Attempt: 1},
					{NodeID: "node-not-started", Step: "cancelled", State: model.StateQueued, Cancelled: true},
					{NodeID: "node-interrupted", Step: "cancelled", State: model.StateStarting, Attempt: 1, Cancelled: true},
				},
			},
		},
		{
			name: "replaced",
			job: Job{
				ID: "job-replaced", Kind: "import", Creator: "operator", CreatedAt: stateTestEpoch.Add(time.Second),
				ConfigRevision: 2, State: JobReplaced, Total: 3, Failed: 1, CancelledNodes: 2,
				Cancelled: true, ReplacedBy: "job-replacement",
				Nodes: []NodeProgress{
					{
						NodeID: "node-failed", Step: "failed", State: model.StateFailed, Attempt: 1,
						Error: &PublicError{Code: ErrorCodeProbeFailed, Message: secret},
					},
					{NodeID: "node-not-started", Step: "replaced", State: model.StateOnline, Cancelled: true},
					{NodeID: "node-interrupted", Step: "replaced", State: model.StateDisabled, Attempt: 1, Cancelled: true},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewJobStore()
			if err := store.Put(test.job); err != nil {
				t.Fatalf("Put(%s) error = %v", test.name, err)
			}
			stored, _ := store.Get(test.job.ID)
			if stored.CancelledNodes != 2 || stored.Succeeded+stored.Failed+stored.CancelledNodes != stored.Total {
				t.Fatalf("stored aggregates = %#v, want two truthful cancelled outcomes", stored)
			}
			if !stored.Nodes[1].Cancelled || !stored.Nodes[2].Cancelled {
				t.Fatalf("stored nodes = %#v, want explicit cancelled outcomes", stored.Nodes)
			}
			if test.name == "replaced" && (stored.Nodes[0].Error == nil || stored.Nodes[0].Error.Message != "connection probe failed") {
				t.Fatalf("stored failed outcome error = %#v, want sanitized public error", stored.Nodes[0].Error)
			}

			encoded, err := json.Marshal(test.job)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				CancelledNodes int `json:"cancelled_nodes"`
				Nodes          []struct {
					Cancelled bool `json:"cancelled"`
				} `json:"nodes"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.CancelledNodes != 2 || !wire.Nodes[1].Cancelled || !wire.Nodes[2].Cancelled {
				t.Fatalf("JSON = %s, want cancelled count and per-node flags", encoded)
			}
			for _, formatted := range []string{
				fmt.Sprintf("%#v", test.job),
				fmt.Sprintf("%#v", test.job.Nodes[0]),
				fmt.Sprintf("%#v", test.job.Nodes[1]),
			} {
				if strings.Contains(formatted, secret) {
					t.Fatalf("formatted cancelled outcome leaked secret: %s", formatted)
				}
			}
		})
	}
}

func TestJobStoreAcceptsTruthfulCancellationAndReplacementUpdates(t *testing.T) {
	terminalStates := []struct {
		state      JobState
		replacedBy string
	}{
		{state: JobCancelled},
		{state: JobReplaced, replacedBy: "job-next"},
	}

	for _, terminal := range terminalStates {
		t.Run(string(terminal.state)+" from queued", func(t *testing.T) {
			store := NewJobStore()
			current := testJob(123)
			if err := store.Put(current); err != nil {
				t.Fatal(err)
			}
			candidate := cloneJob(current)
			candidate.State = terminal.state
			candidate.Queued = 0
			candidate.CancelledNodes = 1
			candidate.Cancelled = true
			candidate.ReplacedBy = terminal.replacedBy
			candidate.Nodes[0].Step = "cancelled"
			candidate.Nodes[0].Cancelled = true
			if err := store.Put(candidate); err != nil {
				t.Fatalf("Put(%s update) error = %v", terminal.state, err)
			}
			if err := store.Put(candidate); err != nil {
				t.Fatalf("Put(idempotent %s update) error = %v", terminal.state, err)
			}
			stored, _ := store.Get(current.ID)
			if !reflect.DeepEqual(stored, candidate) {
				t.Fatalf("stored %s job = %#v, want %#v", terminal.state, stored, candidate)
			}
		})

		t.Run(string(terminal.state)+" from mixed running", func(t *testing.T) {
			store := NewJobStore()
			current := Job{
				ID: "job-mixed-" + string(terminal.state), Kind: "reconcile", Creator: "system",
				CreatedAt: stateTestEpoch, ConfigRevision: 7, State: JobRunning,
				Total: 4, Queued: 1, Running: 1, Succeeded: 1, Failed: 1,
				Nodes: []NodeProgress{
					{NodeID: "node-success", Step: "done", State: model.StateOnline, Attempt: 1},
					{NodeID: "node-failed", Step: "failed", State: model.StateFailed, Attempt: 1, Error: &PublicError{Code: ErrorCodeProbeFailed, Message: "connection probe failed"}},
					{NodeID: "node-queued", Step: "queued", State: model.StateQueued},
					{NodeID: "node-running", Step: "start", State: model.StateStarting, Attempt: 1},
				},
			}
			if err := store.Put(current); err != nil {
				t.Fatal(err)
			}
			candidate := cloneJob(current)
			candidate.State = terminal.state
			candidate.Queued = 0
			candidate.Running = 0
			candidate.CancelledNodes = 2
			candidate.Cancelled = true
			candidate.ReplacedBy = terminal.replacedBy
			for index := 2; index < len(candidate.Nodes); index++ {
				candidate.Nodes[index].Step = "cancelled"
				candidate.Nodes[index].Cancelled = true
			}
			if err := store.Put(candidate); err != nil {
				t.Fatalf("Put(mixed %s update) error = %v", terminal.state, err)
			}
			stored, _ := store.Get(current.ID)
			if !reflect.DeepEqual(stored, candidate) {
				t.Fatalf("stored mixed %s job = %#v, want %#v", terminal.state, stored, candidate)
			}
		})
	}
}

func TestJobStoreAllowsOnlyIdentityPreservingMonotonicUpdates(t *testing.T) {
	store := NewJobStore()
	queued := testJob(130)
	if err := store.Put(queued); err != nil {
		t.Fatal(err)
	}
	running := runningJob(queued)
	running.Nodes[0].Attempt = 1
	if err := store.Put(running); err != nil {
		t.Fatalf("Put(running update) error = %v", err)
	}
	succeeded := succeededJob(queued)
	succeeded.Nodes[0].Attempt = 1
	if err := store.Put(succeeded); err != nil {
		t.Fatalf("Put(succeeded update) error = %v", err)
	}
	if err := store.Put(succeeded); err != nil {
		t.Fatalf("Put(idempotent terminal update) error = %v", err)
	}
	stored, _ := store.Get(queued.ID)
	if !reflect.DeepEqual(stored, succeeded) {
		t.Fatalf("stored job = %#v, want succeeded update", stored)
	}
}

func TestJobStoreRejectsImmutableIdentityChangesAsDuplicate(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*Job)
	}{
		{"kind", func(job *Job) { job.Kind = "delete" }},
		{"creator", func(job *Job) { job.Creator = "operator" }},
		{"created at", func(job *Job) { job.CreatedAt = job.CreatedAt.Add(time.Second) }},
		{"revision", func(job *Job) { job.ConfigRevision++ }},
		{"total", func(job *Job) {
			job.Total = 2
			job.Queued = 2
			job.Nodes = append(job.Nodes, NodeProgress{NodeID: "node-extra", Step: "queued", State: model.StateQueued})
		}},
		{"node identity", func(job *Job) { job.Nodes[0].NodeID = "other-node" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			store := NewJobStore()
			original := testJob(140)
			if err := store.Put(original); err != nil {
				t.Fatal(err)
			}
			candidate := cloneJob(original)
			mutation.mutate(&candidate)
			assertJobCode(t, store.Put(candidate), ErrorCodeDuplicate)
			stored, _ := store.Get(original.ID)
			if !reflect.DeepEqual(stored, original) {
				t.Fatal("duplicate identity mutation changed stored job")
			}
		})
	}
}

func TestJobStoreRejectsTerminalResurrectionAndProgressRegression(t *testing.T) {
	t.Run("terminal resurrection", func(t *testing.T) {
		store := NewJobStore()
		terminal := succeededJob(testJob(150))
		if err := store.Put(terminal); err != nil {
			t.Fatal(err)
		}
		assertJobCode(t, store.Put(runningJob(testJob(150))), ErrorCodeRevisionConflict)
		stored, _ := store.Get(terminal.ID)
		if !reflect.DeepEqual(stored, terminal) {
			t.Fatal("terminal resurrection changed stored job")
		}
	})

	t.Run("completed count regression", func(t *testing.T) {
		store := NewJobStore()
		progress := twoNodeRunningJob(151)
		progress.Succeeded = 1
		progress.Running = 1
		progress.Nodes[0].State = model.StateOnline
		progress.Nodes[0].Step = "done"
		progress.Nodes[1].State = model.StateStarting
		progress.Nodes[1].Step = "start"
		if err := store.Put(progress); err != nil {
			t.Fatal(err)
		}
		regressed := twoNodeRunningJob(151)
		assertJobCode(t, store.Put(regressed), ErrorCodeRevisionConflict)
		stored, _ := store.Get(progress.ID)
		if !reflect.DeepEqual(stored, progress) {
			t.Fatal("progress regression changed stored job")
		}
	})
}

func TestJobStoreCancellationCannotRewriteResidualProgress(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*NodeProgress)
	}{
		{name: "runtime state", mutate: func(node *NodeProgress) { node.State = model.StateOnline }},
		{name: "attempt", mutate: func(node *NodeProgress) { node.Attempt++ }},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			store := NewJobStore()
			current := testJob(154)
			if err := store.Put(current); err != nil {
				t.Fatal(err)
			}
			candidate := cloneJob(current)
			candidate.State = JobCancelled
			candidate.Queued = 0
			candidate.CancelledNodes = 1
			candidate.Cancelled = true
			candidate.Nodes[0].Step = "cancelled"
			candidate.Nodes[0].Cancelled = true
			mutation.mutate(&candidate.Nodes[0])

			assertJobCode(t, store.Put(candidate), ErrorCodeRevisionConflict)
			stored, _ := store.Get(current.ID)
			if !reflect.DeepEqual(stored, current) {
				t.Fatal("illegal cancellation rewrite changed stored job")
			}
		})
	}
}

func TestJobStoreActiveCancelledNodeOutcomeIsIrreversible(t *testing.T) {
	active := Job{
		ID: "job-active-cancel", Kind: "reconcile", Creator: "system", CreatedAt: stateTestEpoch,
		ConfigRevision: 8, State: JobRunning, Total: 2, Running: 1, CancelledNodes: 1,
		Nodes: []NodeProgress{
			{NodeID: "node-cancelled", Step: "cancelled", State: model.StateQueued, Cancelled: true},
			{NodeID: "node-active", Step: "start", State: model.StateStarting, Attempt: 1},
		},
	}
	mutations := []struct {
		name     string
		wantCode string
		mutate   func(*Job)
	}{
		{name: "clear cancelled flag", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) {
			job.CancelledNodes = 0
			job.Queued = 1
			job.Nodes[0].Cancelled = false
			job.Nodes[0].Step = "queued"
		}},
		{name: "step", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) { job.Nodes[0].Step = "rewritten" }},
		{name: "state", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) { job.Nodes[0].State = model.StateOnline }},
		{name: "attempt", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) { job.Nodes[0].Attempt++ }},
		{name: "deadline", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) { job.Nodes[0].Deadline = stateTestEpoch.Add(time.Hour) }},
		{name: "error", wantCode: ErrorCodeInvalidConfig, mutate: func(job *Job) {
			job.Nodes[0].Error = &PublicError{Code: ErrorCodeInternal, Message: "internal node error"}
		}},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			store := NewJobStore()
			if err := store.Put(active); err != nil {
				t.Fatalf("Put(active cancellation) error = %v", err)
			}
			candidate := cloneJob(active)
			mutation.mutate(&candidate)
			assertJobCode(t, store.Put(candidate), mutation.wantCode)
			stored, _ := store.Get(active.ID)
			if !reflect.DeepEqual(stored, active) {
				t.Fatal("active cancelled outcome mutation changed stored job")
			}
		})
	}

	t.Run("terminalize remaining node", func(t *testing.T) {
		store := NewJobStore()
		if err := store.Put(active); err != nil {
			t.Fatalf("Put(active cancellation) error = %v", err)
		}
		terminal := cloneJob(active)
		terminal.State = JobCancelled
		terminal.Running = 0
		terminal.CancelledNodes = 2
		terminal.Cancelled = true
		terminal.Nodes[1].Step = "cancelled"
		terminal.Nodes[1].Cancelled = true
		if err := store.Put(terminal); err != nil {
			t.Fatalf("Put(terminal cancellation) error = %v", err)
		}
		stored, _ := store.Get(active.ID)
		if !reflect.DeepEqual(stored, terminal) {
			t.Fatalf("stored terminal cancellation = %#v, want %#v", stored, terminal)
		}
	})
}

func TestJobStoreCancelledTerminalNodeOutcomeIsFullyImmutable(t *testing.T) {
	mutations := []struct {
		name     string
		wantCode string
		mutate   func(*Job)
	}{
		{name: "step", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) { job.Nodes[0].Step = "rewritten" }},
		{name: "state", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) { job.Nodes[0].State = model.StateStarting }},
		{name: "attempt", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) { job.Nodes[0].Attempt++ }},
		{name: "deadline", wantCode: ErrorCodeRevisionConflict, mutate: func(job *Job) { job.Nodes[0].Deadline = stateTestEpoch.Add(time.Hour) }},
		{name: "error", wantCode: ErrorCodeInvalidConfig, mutate: func(job *Job) {
			job.Nodes[0].Error = &PublicError{Code: ErrorCodeInternal, Message: "internal node error"}
		}},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			store := NewJobStore()
			terminal := testJob(155)
			terminal.State = JobCancelled
			terminal.Queued = 0
			terminal.CancelledNodes = 1
			terminal.Cancelled = true
			terminal.Nodes[0].Step = "cancelled"
			terminal.Nodes[0].Cancelled = true
			if err := store.Put(terminal); err != nil {
				t.Fatal(err)
			}
			candidate := cloneJob(terminal)
			mutation.mutate(&candidate)
			assertJobCode(t, store.Put(candidate), mutation.wantCode)
			stored, _ := store.Get(terminal.ID)
			if !reflect.DeepEqual(stored, terminal) {
				t.Fatal("terminal cancelled outcome mutation changed stored job")
			}
		})
	}
}

func TestJobStoreRejectsPerNodeStateRegressionAtomically(t *testing.T) {
	tests := []struct {
		name      string
		current   model.RuntimeState
		candidate model.RuntimeState
	}{
		{name: "starting to queued", current: model.StateStarting, candidate: model.StateQueued},
		{name: "validating to starting", current: model.StateValidating, candidate: model.StateStarting},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewJobStore()
			current := twoNodeRunningJob(152)
			current.Nodes[0].State = test.current
			current.Nodes[0].Step = string(test.current)
			if err := store.Put(current); err != nil {
				t.Fatal(err)
			}

			candidate := cloneJob(current)
			candidate.Nodes[0].State = test.candidate
			candidate.Nodes[0].Step = string(test.candidate)
			if nodeJobBucket(test.candidate) == jobBucketQueued {
				candidate.Running--
				candidate.Queued++
			}
			assertJobCode(t, store.Put(candidate), ErrorCodeRevisionConflict)
			stored, _ := store.Get(current.ID)
			if !reflect.DeepEqual(stored, current) {
				t.Fatal("per-node state regression changed stored job")
			}
		})
	}
}

func TestJobStoreRejectsTerminalNodeOutcomeRewritesAtomically(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*NodeProgress)
	}{
		{name: "step", mutate: func(node *NodeProgress) { node.Step = "rewritten" }},
		{name: "deadline", mutate: func(node *NodeProgress) { node.Deadline = stateTestEpoch.Add(time.Hour) }},
		{name: "error", mutate: func(node *NodeProgress) {
			node.Error = &PublicError{Code: ErrorCodeAuthentication, Message: "authentication failed"}
		}},
		{name: "attempt", mutate: func(node *NodeProgress) { node.Attempt++ }},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			store := NewJobStore()
			current := twoNodeRunningJob(153)
			current.Running = 1
			current.Succeeded = 1
			current.Nodes[0] = NodeProgress{NodeID: current.Nodes[0].NodeID, Step: "done", State: model.StateOnline, Attempt: 1}
			if err := store.Put(current); err != nil {
				t.Fatal(err)
			}

			candidate := cloneJob(current)
			mutation.mutate(&candidate.Nodes[0])
			assertJobCode(t, store.Put(candidate), ErrorCodeRevisionConflict)
			stored, _ := store.Get(current.ID)
			if !reflect.DeepEqual(stored, current) {
				t.Fatal("terminal node outcome rewrite changed stored job")
			}
		})
	}
}

func TestExistingJobIDCannotBypassAllActiveCapacity(t *testing.T) {
	store := NewJobStore()
	for index := 0; index < MaxRetainedJobs; index++ {
		if err := store.Put(runningJob(testJob(index))); err != nil {
			t.Fatal(err)
		}
	}
	before := store.List()
	reused := runningJob(testJob(0))
	reused.Kind = "delete"
	assertJobCode(t, store.Put(reused), ErrorCodeDuplicate)
	if after := store.List(); !reflect.DeepEqual(after, before) {
		t.Fatal("reused active job ID bypassed capacity or changed live work")
	}
}

func TestJobStoreIsConcurrencySafeAndStrictlyBounded(t *testing.T) {
	store := NewJobStore()
	const writers = 32
	const perWriter = 100
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter*2)
	for writer := 0; writer < writers; writer++ {
		writer := writer
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := 0; index < perWriter; index++ {
				job := Job{
					ID:        fmt.Sprintf("job-%02d-%03d", writer, index),
					Kind:      "reconcile",
					Creator:   "system",
					CreatedAt: stateTestEpoch.Add(time.Duration(writer*perWriter+index) * time.Second),
					State:     JobSucceeded,
				}
				if err := store.Put(job); err != nil {
					errs <- err
				}
				if _, err := store.AppendEvent(NodeEvent{JobID: job.ID, NodeID: fmt.Sprintf("node-%02d", writer), Generation: 1, State: model.StateOnline, At: job.CreatedAt}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent store operation error = %v", err)
	}
	if got := len(store.List()); got != MaxRetainedJobs {
		t.Fatalf("len(List()) = %d, want %d", got, MaxRetainedJobs)
	}
	if got := len(store.Events()); got != MaxRetainedNodeEvents {
		t.Fatalf("len(Events()) = %d, want %d", got, MaxRetainedNodeEvents)
	}
}

func testJob(index int) Job {
	return Job{
		ID:             fmt.Sprintf("job-%03d", index),
		Kind:           "reconcile",
		Creator:        "system",
		CreatedAt:      stateTestEpoch.Add(time.Duration(index) * time.Second),
		ConfigRevision: uint64(index + 1),
		Total:          1,
		Queued:         1,
		State:          JobQueued,
		Nodes: []NodeProgress{{
			NodeID: fmt.Sprintf("node-%03d", index),
			Step:   "queued",
			State:  model.StateQueued,
		}},
	}
}

func runningJob(job Job) Job {
	job.State = JobRunning
	job.Queued = 0
	job.Running = job.Total
	for index := range job.Nodes {
		job.Nodes[index].Step = "start"
		job.Nodes[index].State = model.StateStarting
	}
	return job
}

func succeededJob(job Job) Job {
	job.State = JobSucceeded
	job.Queued = 0
	job.Running = 0
	job.Succeeded = job.Total
	for index := range job.Nodes {
		job.Nodes[index].Step = "done"
		job.Nodes[index].State = model.StateOnline
	}
	return job
}

func failedJob(job Job) Job {
	job.State = JobFailed
	job.Queued = 0
	job.Running = 0
	job.Failed = job.Total
	for index := range job.Nodes {
		job.Nodes[index].Step = "failed"
		job.Nodes[index].State = model.StateFailed
		job.Nodes[index].Error = &PublicError{Code: ErrorCodeInternal, Message: "internal node error"}
	}
	return job
}

func twoNodeRunningJob(index int) Job {
	job := testJob(index)
	job.Total = 2
	job.Queued = 0
	job.Running = 2
	job.State = JobRunning
	job.Nodes = []NodeProgress{
		{NodeID: fmt.Sprintf("node-%03d-a", index), Step: "start", State: model.StateStarting},
		{NodeID: fmt.Sprintf("node-%03d-b", index), Step: "start", State: model.StateStarting},
	}
	return job
}

func assertJobCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %q", want)
	}
	var codeErr *model.CodeError
	if !errors.As(err, &codeErr) || codeErr.Code != want {
		t.Fatalf("error = %#v, want CodeError %q", err, want)
	}
}

func assertNoCredentialFields(t *testing.T, typ reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(model.Node{}) {
		t.Fatalf("public DTO reaches credential-bearing model.Node through %v", typ)
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return
	}
	seen[typ] = true
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		name := strings.ToLower(field.Name)
		if strings.Contains(name, "password") || strings.Contains(name, "token") || strings.Contains(name, "obfskey") {
			t.Fatalf("public DTO %v has credential field %q", typ, field.Name)
		}
		assertNoCredentialFields(t, field.Type, seen)
	}
}
