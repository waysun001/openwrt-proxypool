package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

func TestSchedulerPersistsBeforeStartAndRequiresProbeAndEveryGate(t *testing.T) {
	desired := controllerConfig()
	runtime := &memoryRuntimePersistence{}
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore(),
		WithControllerClock(func() time.Time { return stateTestEpoch }),
		WithControllerJobIDSource(func() string { return "job-scheduled" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	gateRelease := make(chan struct{})
	adapter := &schedulerAdapter{runtimeSaves: func() int { return runtime.saveCount }}
	gate := &schedulerGate{openRelease: gateRelease}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{L2TPConcurrency: 4, ProxyConcurrency: 8, ConnectTimeout: time.Second, StopTimeout: time.Second}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()

	response := controller.Handle(context.Background(), controllerRequest("schedule-one", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":3}`))
	assertControllerSuccess(t, response)
	waitForAdapterCalls(t, adapter, 1)
	if adapter.saveCountAtStart < 3 {
		t.Fatalf("Start observed only %d runtime saves; queued/generation/start were not persisted first", adapter.saveCountAtStart)
	}
	waitForNodeState(t, controller, "node_a", model.StateValidating)
	if job, _ := controller.jobs.Get("job-scheduled"); job.State != JobRunning {
		t.Fatalf("job state before gate = %q, want running", job.State)
	}
	close(gateRelease)
	waitForJobState(t, controller.jobs, "job-scheduled", JobSucceeded)
	waitForNodeState(t, controller, "node_a", model.StateOnline)
	if adapter.probeCalls != 1 || gate.openCalls != 1 {
		t.Fatalf("probe/gate calls = %d/%d", adapter.probeCalls, gate.openCalls)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestSchedulerLimitsL2TPConcurrencyToFourAndIsolatesNodeFailure(t *testing.T) {
	desired := controllerConfig()
	desired.Nodes = make(map[string]model.Node)
	for index := 1; index <= 6; index++ {
		id := fmt.Sprintf("node_%d", index)
		desired.Nodes[id] = model.Node{
			ID: id, Name: fmt.Sprintf("Node %d", index), Protocol: model.ProtocolL2TP, Enabled: true,
			Server: fmt.Sprintf("node-%d.example", index), Port: 1701, Username: "user", Password: "password",
			PolicyID: uint16(index), Revision: 3,
		}
	}
	desired.Devices = map[string]model.Device{}
	runtime := &memoryRuntimePersistence{}
	controller, err := NewController(&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	adapter := &schedulerAdapter{startRelease: release, failNode: "node_2"}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{L2TPConcurrency: 4, ProxyConcurrency: 8, ConnectTimeout: time.Second, StopTimeout: time.Second})
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	for index := 1; index <= 6; index++ {
		id := fmt.Sprintf("node_%d", index)
		requestID := fmt.Sprintf("request-%d", index)
		response := controller.Handle(context.Background(), controllerRequest(requestID, "node.action", fmt.Sprintf(`{"node_id":%q,"action":"reconnect","expected_revision":3}`, id)))
		assertControllerSuccess(t, response)
	}
	waitForActiveStarts(t, adapter, 4)
	adapter.mu.Lock()
	maxActive := adapter.maxActive
	adapter.mu.Unlock()
	if maxActive != 4 {
		t.Fatalf("max concurrent L2TP starts = %d, want exactly 4", maxActive)
	}
	close(release)
	waitForNodeState(t, controller, "node_1", model.StateOnline)
	waitForNodeState(t, controller, "node_3", model.StateOnline)
	waitForNodeStateOneOf(t, controller, "node_2", model.StateBackoff, model.StateFailed)
	if status, _ := controller.schedulerStatus("node_1"); status.State != model.StateOnline {
		t.Fatalf("node_2 failure contaminated node_1: %#v", status)
	}
}

func TestSchedulerDeadlineStopsPartialSessionAndNeverPublishesOnline(t *testing.T) {
	desired := controllerConfig()
	desired.Global.ConnectTimeout = 20 * time.Millisecond
	runtime := &memoryRuntimePersistence{}
	controller, err := NewController(&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore(), WithControllerJobIDSource(func() string { return "job-timeout" }))
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{blockUntilContext: true}
	gate := &schedulerGate{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{L2TPConcurrency: 4, ProxyConcurrency: 8, ConnectTimeout: 20 * time.Millisecond, StopTimeout: time.Second}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("timeout", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":3}`)))
	waitForNodeStateOneOf(t, controller, "node_a", model.StateBackoff, model.StateFailed)
	if gate.openCalls != 0 {
		t.Fatal("timed-out Start reached an online gate")
	}
	if adapter.stopCalls == 0 {
		t.Fatal("timed-out partial session was not stopped")
	}
}

func TestSchedulerRecoversPersistedQueuedJobAfterControllerRestart(t *testing.T) {
	desiredStore := &memoryDesiredStore{cfg: controllerConfig()}
	runtime := &memoryRuntimePersistence{}
	first, err := NewController(desiredStore, runtime, NewMachine(nil), NewJobStore(), WithControllerJobIDSource(func() string { return "job-restart" }))
	if err != nil {
		t.Fatal(err)
	}
	assertControllerSuccess(t, first.Handle(context.Background(), controllerRequest("restart-request", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":3}`)))
	if job, _ := first.jobs.Get("job-restart"); job.State != JobQueued {
		t.Fatalf("pre-restart job = %q, want queued", job.State)
	}

	restartedJobs := NewJobStore()
	restarted, err := NewController(desiredStore, runtime, NewMachine(nil), restartedJobs)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	scheduler := NewScheduler(restarted, adapter, SchedulerConfig{L2TPConcurrency: 4, ProxyConcurrency: 8, ConnectTimeout: time.Second, StopTimeout: time.Second})
	restarted.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	waitForJobState(t, restartedJobs, "job-restart", JobSucceeded)
	if adapter.startCalls != 1 {
		t.Fatalf("restart Start calls = %d, want 1", adapter.startCalls)
	}
}

func TestSchedulerClosesOpenedGatesInReverseBeforeStoppingOnValidationFailure(t *testing.T) {
	runtime := &memoryRuntimePersistence{}
	controller, err := NewController(&memoryDesiredStore{cfg: controllerConfig()}, runtime, NewMachine(nil), NewJobStore(), WithControllerJobIDSource(func() string { return "job-gate-failure" }))
	if err != nil {
		t.Fatal(err)
	}
	var orderMu sync.Mutex
	var order []string
	adapter := &schedulerAdapter{onStop: func() { orderMu.Lock(); order = append(order, "stop"); orderMu.Unlock() }}
	first := &schedulerGate{name: "first", order: &order, orderMu: &orderMu}
	second := &schedulerGate{name: "second", openErr: errors.New("DNS not ready"), order: &order, orderMu: &orderMu}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{L2TPConcurrency: 4, ProxyConcurrency: 8, ConnectTimeout: time.Second, StopTimeout: time.Second}, first, second)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("gate-failure", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":3}`)))
	waitForNodeStateOneOf(t, controller, "node_a", model.StateBackoff, model.StateFailed)
	orderMu.Lock()
	got := append([]string(nil), order...)
	orderMu.Unlock()
	want := []string{"open:first", "open:second", "close:first", "stop"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("cleanup order = %v, want %v", got, want)
	}
}

func TestSchedulerStopWithoutRuntimeSessionCompletesWithoutCreatingQueuedState(t *testing.T) {
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-stop-idle" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(controller, &schedulerAdapter{}, SchedulerConfig{})
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("stop-idle", "node.action", `{"node_id":"node_a","action":"stop","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-stop-idle", JobSucceeded)
	if status, exists := controller.schedulerStatus("node_a"); exists && status.State != model.StateDisabled {
		t.Fatalf("idle stop fabricated runtime state: %#v", status)
	}
}

func TestSchedulerReconnectEnablesPersistedDisabledNode(t *testing.T) {
	desired := controllerConfig()
	runtime := &memoryRuntimePersistence{
		exists: true,
		snapshot: RuntimeSnapshot{
			SchemaVersion:  RuntimeSnapshotSchemaVersion,
			ConfigRevision: desired.Revision,
			NodeStatuses: []NodeStatus{{
				NodeID: "node_a", JobID: "old-stop", Generation: 7,
				State: model.StateDisabled, UpdatedAt: stateTestEpoch,
			}},
		},
	}
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-enable-disabled" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{})
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("enable-disabled", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-enable-disabled", JobSucceeded)
	waitForNodeState(t, controller, "node_a", model.StateOnline)
	if status, _ := controller.schedulerStatus("node_a"); status.Generation <= 7 {
		t.Fatalf("disabled generation was not advanced: %#v", status)
	}
}

type memoryDesiredStore struct {
	mu  sync.Mutex
	cfg model.DesiredConfig
}

func (store *memoryDesiredStore) Load() (model.DesiredConfig, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneControllerConfig(store.cfg), nil
}

func (store *memoryDesiredStore) Replace(_ context.Context, expected uint64, next model.DesiredConfig) (model.DesiredConfig, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.cfg.Revision != expected {
		return model.DesiredConfig{}, codeError(ErrorCodeRevisionConflict, "revision conflict")
	}
	next.Revision = expected + 1
	for id, node := range next.Nodes {
		node.Revision = next.Revision
		next.Nodes[id] = node
	}
	store.cfg = cloneControllerConfig(next)
	return cloneControllerConfig(next), nil
}

type memoryRuntimePersistence struct {
	mu        sync.Mutex
	snapshot  RuntimeSnapshot
	exists    bool
	saveCount int
}

func (store *memoryRuntimePersistence) Load() (RuntimeSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.exists {
		return RuntimeSnapshot{}, ErrRuntimeSnapshotNotFound
	}
	return normalizeRuntimeSnapshot(store.snapshot)
}

func (store *memoryRuntimePersistence) Save(_ context.Context, snapshot RuntimeSnapshot) error {
	normalized, err := normalizeRuntimeSnapshot(snapshot)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.snapshot, store.exists = normalized, true
	store.saveCount++
	return nil
}

type schedulerAdapter struct {
	mu                sync.Mutex
	startRelease      <-chan struct{}
	blockUntilContext bool
	failNode          string
	runtimeSaves      func() int
	onStop            func()
	active            int
	maxActive         int
	startCalls        int
	probeCalls        int
	stopCalls         int
	saveCountAtStart  int
}

func (adapter *schedulerAdapter) Start(ctx context.Context, request platform.NodeRequest) (platform.Session, error) {
	adapter.mu.Lock()
	adapter.startCalls++
	adapter.active++
	if adapter.active > adapter.maxActive {
		adapter.maxActive = adapter.active
	}
	if adapter.runtimeSaves != nil {
		adapter.saveCountAtStart = adapter.runtimeSaves()
	}
	adapter.mu.Unlock()
	defer func() { adapter.mu.Lock(); adapter.active--; adapter.mu.Unlock() }()
	if adapter.blockUntilContext {
		<-ctx.Done()
		return platform.Session{NodeID: request.Node.ID, Generation: request.Generation, Protocol: request.Node.Protocol}, ctx.Err()
	}
	if adapter.startRelease != nil {
		select {
		case <-adapter.startRelease:
		case <-ctx.Done():
			return platform.Session{NodeID: request.Node.ID, Generation: request.Generation, Protocol: request.Node.Protocol}, ctx.Err()
		}
	}
	if request.Node.ID == adapter.failNode {
		return platform.Session{NodeID: request.Node.ID, Generation: request.Generation, Protocol: request.Node.Protocol}, errors.New("isolated start failure")
	}
	return platform.Session{NodeID: request.Node.ID, Generation: request.Generation, Protocol: request.Node.Protocol, Interface: "l2tp-test", OwnershipDigest: "owned"}, nil
}

func (adapter *schedulerAdapter) Probe(_ context.Context, _ platform.NodeRequest, _ platform.Session) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.probeCalls++
	return nil
}

func (adapter *schedulerAdapter) Stop(_ context.Context, _ platform.NodeRequest, _ platform.Session) error {
	adapter.mu.Lock()
	adapter.stopCalls++
	adapter.mu.Unlock()
	if adapter.onStop != nil {
		adapter.onStop()
	}
	return nil
}

type schedulerGate struct {
	name        string
	openRelease <-chan struct{}
	openErr     error
	order       *[]string
	orderMu     *sync.Mutex
	openCalls   int
	closeCalls  int
}

func (gate *schedulerGate) Open(ctx context.Context, _ platform.NodeRequest, _ platform.Session) error {
	gate.openCalls++
	gate.record("open:" + gate.name)
	if gate.openRelease != nil {
		select {
		case <-gate.openRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return gate.openErr
}

func (gate *schedulerGate) Close(_ context.Context, _ platform.NodeRequest, _ platform.Session) error {
	gate.closeCalls++
	gate.record("close:" + gate.name)
	return nil
}

func (gate *schedulerGate) record(value string) {
	if gate.order == nil || gate.orderMu == nil {
		return
	}
	gate.orderMu.Lock()
	*gate.order = append(*gate.order, value)
	gate.orderMu.Unlock()
}

func waitForAdapterCalls(t *testing.T, adapter *schedulerAdapter, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		adapter.mu.Lock()
		calls := adapter.startCalls
		adapter.mu.Unlock()
		if calls >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("adapter did not reach %d Start calls", count)
}

func waitForActiveStarts(t *testing.T, adapter *schedulerAdapter, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		adapter.mu.Lock()
		active := adapter.active
		adapter.mu.Unlock()
		if active == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("adapter did not reach %d active starts", count)
}

func waitForJobState(t *testing.T, jobs *JobStore, id string, want JobState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job, exists := jobs.Get(id); exists && job.State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	job, _ := jobs.Get(id)
	t.Fatalf("job %q state = %q, want %q", id, job.State, want)
}

func waitForNodeState(t *testing.T, controller *Controller, id string, want model.RuntimeState) {
	t.Helper()
	waitForNodeStateOneOf(t, controller, id, want)
}

func waitForNodeStateOneOf(t *testing.T, controller *Controller, id string, wants ...model.RuntimeState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, exists := controller.schedulerStatus(id)
		if exists {
			for _, want := range wants {
				if status.State == want {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	status, _ := controller.schedulerStatus(id)
	t.Fatalf("node %q state = %q, want one of %v", id, status.State, wants)
}
