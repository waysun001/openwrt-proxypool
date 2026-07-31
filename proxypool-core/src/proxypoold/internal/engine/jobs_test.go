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
		job := testJob(index)
		job.State = JobSucceeded
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
		job := testJob(index)
		job.State = JobSucceeded
		if err := store.Put(job); err != nil {
			t.Fatal(err)
		}
	}
	updated := testJob(0)
	updated.State = JobFailed
	updated.Failed = 1
	if err := store.Put(updated); err != nil {
		t.Fatal(err)
	}
	newest := testJob(MaxRetainedJobs)
	newest.State = JobSucceeded
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
		job := testJob(index)
		job.State = JobRunning
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
		job := testJob(index)
		job.State = JobRunning
		if index == 5 || index == 9 {
			job.State = JobSucceeded
		}
		if err := store.Put(job); err != nil {
			t.Fatal(err)
		}
	}
	newJob := testJob(MaxRetainedJobs)
	newJob.State = JobRunning
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
	job := testJob(1)
	job.State = JobFailed
	job.Nodes = []NodeProgress{{
		NodeID: "node-a",
		Step:   "validate",
		State:  model.StateFailed,
		Error:  &PublicError{Code: ErrorCodeAuthentication, Message: "credentials rejected"},
	}}
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
	if second.Nodes[0].Step != "validate" || second.Nodes[0].Error.Message != "credentials rejected" {
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
	if got := store.Events()[0].Error.Message; got != "adapter unavailable" {
		t.Fatalf("stored event error = %q, want independent copy", got)
	}
}

func TestJobAndEventDTOsCannotContainNodeCredentials(t *testing.T) {
	for _, value := range []any{Job{}, NodeProgress{}, NodeEvent{}, NodeStatus{}} {
		assertNoCredentialFields(t, reflect.TypeOf(value), map[reflect.Type]bool{})
	}

	job := testJob(1)
	job.Nodes = []NodeProgress{{NodeID: "node-a", Step: "connect", State: model.StateStarting}}
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
