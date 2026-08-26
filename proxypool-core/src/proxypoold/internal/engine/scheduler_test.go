package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/importer"
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

func TestSchedulerTrafficFollowsValidatedSessionLifecycle(t *testing.T) {
	jobIDs := []string{"job-traffic-connect", "job-traffic-reconnect", "job-traffic-stop"}
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	reader := &schedulerTrafficReader{}
	reader.set(platform.InterfaceCounters{RXBytes: 100, TXBytes: 200}, nil)
	scheduler := NewSchedulerWithTraffic(controller, &schedulerAdapter{}, reader, SchedulerConfig{TrafficSampleInterval: 5 * time.Millisecond})
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("traffic-connect", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-traffic-connect", JobSucceeded)
	waitForTraffic(t, scheduler, "node_a", func(snapshot TrafficSnapshot) bool { return snapshot.SampledAt != "" })
	if got := scheduler.Traffic("node_a"); got.DownloadBytes != 0 || got.UploadBytes != 0 {
		t.Fatalf("initial online traffic = %#v", got)
	}

	reader.set(platform.InterfaceCounters{RXBytes: 612, TXBytes: 456}, nil)
	waitForTraffic(t, scheduler, "node_a", func(snapshot TrafficSnapshot) bool {
		return snapshot.DownloadBytes == 512 && snapshot.UploadBytes == 256 && snapshot.DownloadBytesPerSecond > 0 && snapshot.UploadBytesPerSecond > 0
	})
	reader.set(platform.InterfaceCounters{}, errors.New("sysfs temporarily unavailable"))
	waitForTraffic(t, scheduler, "node_a", func(snapshot TrafficSnapshot) bool {
		return snapshot.DownloadBytes == 512 && snapshot.UploadBytes == 256 && snapshot.DownloadBytesPerSecond == 0 && snapshot.UploadBytesPerSecond == 0
	})
	if status, exists := controller.schedulerStatus("node_a"); !exists || status.State != model.StateOnline {
		t.Fatalf("traffic read failure changed node status: %#v exists=%t", status, exists)
	}

	reader.set(platform.InterfaceCounters{RXBytes: 700, TXBytes: 500}, nil)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("traffic-reconnect", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-traffic-reconnect", JobSucceeded)
	waitForTraffic(t, scheduler, "node_a", func(snapshot TrafficSnapshot) bool {
		return snapshot.SampledAt != "" && snapshot.DownloadBytes == 0 && snapshot.UploadBytes == 0
	})

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("traffic-stop", "node.action", `{"node_id":"node_a","action":"stop","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-traffic-stop", JobSucceeded)
	waitForTraffic(t, scheduler, "node_a", func(snapshot TrafficSnapshot) bool { return snapshot == (TrafficSnapshot{}) })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestSchedulerNodeSaveReconnectsChangedOnlineNodeAndStopsDisabledNode(t *testing.T) {
	jobIDs := []string{"job-connect-before-save", "job-save-reconnect", "job-save-disable"}
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
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

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("connect-before-save", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-connect-before-save", JobSucceeded)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"save-online", "node.save", `{"node_id":"node_a","name":"Node A","protocol":"l2tp","enabled":true,"server":"a-new.example","port":1701,"username":"user-a","password":"","expected_revision":3}`,
	)))
	waitForJobState(t, controller.jobs, "job-save-reconnect", JobSucceeded)
	adapter.mu.Lock()
	startsAfterReconnect, stopsAfterReconnect := adapter.startCalls, adapter.stopCalls
	adapter.mu.Unlock()
	if startsAfterReconnect != 2 || stopsAfterReconnect != 1 {
		t.Fatalf("online save start/stop calls = %d/%d, want 2/1", startsAfterReconnect, stopsAfterReconnect)
	}

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"save-disabled", "node.save", `{"node_id":"node_a","name":"Node A","protocol":"l2tp","enabled":false,"server":"a-new.example","port":1701,"username":"user-a","password":"","expected_revision":4}`,
	)))
	waitForJobState(t, controller.jobs, "job-save-disable", JobSucceeded)
	adapter.mu.Lock()
	startsAfterDisable, stopsAfterDisable := adapter.startCalls, adapter.stopCalls
	adapter.mu.Unlock()
	if startsAfterDisable != 2 || stopsAfterDisable != 2 {
		t.Fatalf("disabled save start/stop calls = %d/%d, want 2/2", startsAfterDisable, stopsAfterDisable)
	}
	waitForNodeState(t, controller, "node_a", model.StateDisabled)
}

func TestSchedulerNodeDeleteCleansOwnedSessionBeforeRemovingTombstone(t *testing.T) {
	jobIDs := []string{"job-connect-before-delete", "job-delete-finalize"}
	desiredStore := &memoryDesiredStore{cfg: controllerConfig()}
	controller, err := NewController(
		desiredStore, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
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

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("connect-before-delete", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-connect-before-delete", JobSucceeded)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("delete-online", "node.delete", `{"node_id":"node_a","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-delete-finalize", JobSucceeded)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stored, _ := desiredStore.Load()
		if _, exists := stored.Nodes["node_a"]; !exists {
			if stored.Revision != 5 || stored.Devices["device_a"].NodeID != "" || stored.Devices["device_a"].Enabled {
				t.Fatalf("final delete config = revision %d device %#v", stored.Revision, stored.Devices["device_a"])
			}
			adapter.mu.Lock()
			starts, stops := adapter.startCalls, adapter.stopCalls
			adapter.mu.Unlock()
			if starts != 1 || stops != 1 {
				t.Fatalf("delete start/stop calls = %d/%d, want 1/1", starts, stops)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("delete job succeeded without removing the tombstone")
}

func TestSchedulerDeleteKeepsTombstoneWhenGateRevocationFails(t *testing.T) {
	jobIDs := []string{"job-connect-gate-delete", "job-delete-gate-failure"}
	desiredStore := &memoryDesiredStore{cfg: controllerConfig()}
	controller, err := NewController(
		desiredStore, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	gate := &schedulerGate{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("connect-gate-delete", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-connect-gate-delete", JobSucceeded)
	gate.closeErr = errors.New("authorization revoke failed")
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("delete-gate-failure", "node.delete", `{"node_id":"node_a","expected_revision":3}`)))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := controller.jobs.Get("job-delete-gate-failure")
		if len(job.Nodes) == 1 && job.Nodes[0].Step == "cleanup_failed" {
			stored, _ := desiredStore.Load()
			if node, exists := stored.Nodes["node_a"]; !exists || !node.DeletePending {
				t.Fatalf("failed gate revocation removed tombstone: %#v exists=%t", node, exists)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("gate revocation failure was not retained as cleanup_failed")
}

func TestSchedulerDeleteRetriesCleanupWithOriginalSessionGeneration(t *testing.T) {
	jobIDs := []string{"job-connect-cleanup-retry", "job-stop-cleanup-retry", "job-delete-cleanup-retry"}
	desiredStore := &memoryDesiredStore{cfg: controllerConfig()}
	controller, err := NewController(
		desiredStore, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	gate := &schedulerGate{closeFailures: 1}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"connect-cleanup-retry", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`,
	)))
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-connect-cleanup-retry", nodeID: "node_a"})
	status, exists := controller.schedulerStatus("node_a")
	if !exists || status.State != model.StateOnline {
		t.Fatalf("connected status = %#v exists=%t", status, exists)
	}
	ownedGeneration := status.Generation
	adapter.mu.Lock()
	adapter.requiredStopGeneration = ownedGeneration
	adapter.rejectWrongStopGeneration = true
	adapter.mu.Unlock()

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"stop-cleanup-retry", "node.action", `{"node_id":"node_a","action":"stop","expected_revision":3}`,
	)))
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-stop-cleanup-retry", nodeID: "node_a"})
	status, exists = controller.schedulerStatus("node_a")
	if !exists || status.State != model.StateRecovering || !status.CleanupPending {
		t.Fatalf("failed cleanup status = %#v exists=%t", status, exists)
	}

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"delete-cleanup-retry", "node.delete", `{"node_id":"node_a","expected_revision":4}`,
	)))
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-delete-cleanup-retry", nodeID: "node_a"})
	job, _ := controller.jobs.Get("job-delete-cleanup-retry")
	if job.State != JobSucceeded {
		adapter.mu.Lock()
		gotGenerations := append([]uint64(nil), adapter.stopGenerations...)
		adapter.mu.Unlock()
		t.Fatalf("delete after partial cleanup state=%s progress=%#v stop_generations=%v gate_closes=%d", job.State, job.Nodes, gotGenerations, gate.closeCalls)
	}
	stored, _ := desiredStore.Load()
	if _, exists := stored.Nodes["node_a"]; exists {
		t.Fatal("successful cleanup retry retained the delete tombstone")
	}
	adapter.mu.Lock()
	gotGenerations := append([]uint64(nil), adapter.stopGenerations...)
	adapter.mu.Unlock()
	wantGenerations := []uint64{ownedGeneration, ownedGeneration}
	if !reflect.DeepEqual(gotGenerations, wantGenerations) {
		t.Fatalf("cleanup retry generations = %v, want %v", gotGenerations, wantGenerations)
	}
}

func TestSchedulerReconnectTakesOverFailedCleanupAndStartsFreshGeneration(t *testing.T) {
	jobIDs := []string{"job-connect-recovery-retry", "job-stop-recovery-retry", "job-reconnect-recovery-retry"}
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	gate := &schedulerGate{closeFailures: 1}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"connect-recovery-retry", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`,
	)))
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-connect-recovery-retry", nodeID: "node_a"})
	status, _ := controller.schedulerStatus("node_a")
	ownedGeneration := status.Generation
	adapter.mu.Lock()
	adapter.requiredStopGeneration = ownedGeneration
	adapter.rejectWrongStopGeneration = true
	adapter.mu.Unlock()

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"stop-recovery-retry", "node.action", `{"node_id":"node_a","action":"stop","expected_revision":3}`,
	)))
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-stop-recovery-retry", nodeID: "node_a"})
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"reconnect-recovery-retry", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":4}`,
	)))
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-reconnect-recovery-retry", nodeID: "node_a"})
	status, exists := controller.schedulerStatus("node_a")
	if !exists || status.State != model.StateOnline || status.Generation <= ownedGeneration {
		t.Fatalf("reconnected status = %#v exists=%t", status, exists)
	}
	job, _ := controller.jobs.Get("job-reconnect-recovery-retry")
	if job.State != JobSucceeded {
		t.Fatalf("reconnect after failed cleanup state=%s progress=%#v", job.State, job.Nodes)
	}
	adapter.mu.Lock()
	starts := adapter.startCalls
	gotGenerations := append([]uint64(nil), adapter.stopGenerations...)
	adapter.mu.Unlock()
	if starts != 2 || !reflect.DeepEqual(gotGenerations, []uint64{ownedGeneration, ownedGeneration}) {
		t.Fatalf("cleanup takeover starts=%d stop_generations=%v", starts, gotGenerations)
	}
}

func TestSchedulerDeleteDoesNotSucceedWhenTombstoneFinalizationFails(t *testing.T) {
	desiredStore := &memoryDesiredStore{cfg: controllerConfig(), failReplaceAt: 2}
	controller, err := NewController(
		desiredStore, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-delete-finalize-failure" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("delete-finalize-failure", "node.delete", `{"node_id":"node_a","expected_revision":3}`)))
	scheduler := NewScheduler(controller, &schedulerAdapter{}, SchedulerConfig{})
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-delete-finalize-failure", nodeID: "node_a"})
	job, exists := controller.jobs.Get("job-delete-finalize-failure")
	if !exists || job.State == JobSucceeded {
		t.Fatalf("failed finalization reported success: %#v exists=%t", job, exists)
	}
	stored, _ := desiredStore.Load()
	if node, exists := stored.Nodes["node_a"]; !exists || !node.DeletePending {
		t.Fatalf("failed finalization lost tombstone: %#v exists=%t", node, exists)
	}
}

func TestSchedulerDeleteRetriesTerminalPersistenceAfterTombstoneRemoval(t *testing.T) {
	desiredStore := &memoryDesiredStore{cfg: controllerConfig()}
	runtime := &memoryRuntimePersistence{}
	jobIDs := []string{"job-connect-terminal-retry", "job-delete-terminal-retry"}
	controller, err := NewController(
		desiredStore, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{})
	// Drive the first connect directly so the runtime save sequence is stable.
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("connect-terminal-retry", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`)))
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-connect-terminal-retry", nodeID: "node_a"})
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("delete-terminal-retry", "node.delete", `{"node_id":"node_a","expected_revision":3}`)))
	// EventStop succeeds, then three consecutive terminal snapshot writes fail.
	// The worker must stay alive and finish once persistence recovers.
	runtime.mu.Lock()
	runtime.failSaveFrom = runtime.saveCount + 2
	runtime.failSaveThrough = runtime.saveCount + 4
	runtime.mu.Unlock()
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-delete-terminal-retry", nodeID: "node_a"})
	job, exists := controller.jobs.Get("job-delete-terminal-retry")
	if !exists || job.State != JobSucceeded {
		t.Fatalf("transient terminal persistence orphaned delete job: %#v exists=%t", job, exists)
	}
	stored, _ := desiredStore.Load()
	if _, exists := stored.Nodes["node_a"]; exists {
		t.Fatal("terminal persistence retry restored deleted tombstone")
	}
}

func TestSchedulerRestartRetainsDeletionOwnershipAndFinishesPendingDelete(t *testing.T) {
	desired := controllerConfig()
	desired.Revision = 4
	node := desired.Nodes["node_a"]
	node.Enabled = false
	node.DeletePending = true
	node.Revision = 4
	desired.Nodes[node.ID] = node
	device := desired.Devices["device_a"]
	device.Enabled = false
	device.NodeID = ""
	desired.Devices[device.ID] = device
	desiredStore := &memoryDesiredStore{cfg: desired}
	runtime := &memoryRuntimePersistence{exists: true, snapshot: RuntimeSnapshot{
		SchemaVersion: RuntimeSnapshotSchemaVersion, ConfigRevision: 3,
		NodeStatuses: []NodeStatus{{NodeID: "node_a", JobID: "old-online", Generation: 7, State: model.StateOnline, UpdatedAt: stateTestEpoch}},
	}}
	controller, err := NewController(
		desiredStore, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-recover-delete" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	status, exists := controller.schedulerStatus("node_a")
	if !exists || status.Generation != 7 {
		t.Fatalf("restart discarded delete ownership: %#v exists=%t", status, exists)
	}
	adapter := &schedulerAdapter{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{})
	controller.AttachScheduler(scheduler)
	if err := controller.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	waitForJobState(t, controller.jobs, "job-recover-delete", JobSucceeded)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stored, _ := desiredStore.Load()
		if _, exists := stored.Nodes["node_a"]; !exists {
			adapter.mu.Lock()
			stops := adapter.stopCalls
			mismatch := adapter.stopGenerationMismatch
			adapter.mu.Unlock()
			if stops != 1 || mismatch {
				t.Fatalf("recovered delete stop calls=%d generation mismatch=%t", stops, mismatch)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	stored, _ := desiredStore.Load()
	job, _ := controller.jobs.Get("job-recover-delete")
	status, statusExists := controller.schedulerStatus("node_a")
	t.Fatalf("restart did not finish pending delete: config=%#v job=%#v status=%#v exists=%t", stored.Nodes["node_a"], job, status, statusExists)
}

func TestSchedulerDeleteFinalizationDoesNotOrphanOlderNodeWork(t *testing.T) {
	jobIDs := []string{"job-save-before-delete", "job-delete-after-save"}
	desiredStore := &memoryDesiredStore{cfg: controllerConfig()}
	controller, err := NewController(
		desiredStore, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"save-before-delete", "node.save", `{"node_id":"node_a","name":"Node A","protocol":"l2tp","enabled":true,"server":"a-new.example","port":1701,"username":"","password":"","expected_revision":3}`,
	)))
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("delete-after-save", "node.delete", `{"node_id":"node_a","expected_revision":4}`)))
	scheduler := NewScheduler(controller, &schedulerAdapter{}, SchedulerConfig{})

	// Exercise the adverse queue order: deletion reaches cleanup before the
	// older save job has observed the node.
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-delete-after-save", nodeID: "node_a"})
	stored, _ := desiredStore.Load()
	if _, exists := stored.Nodes["node_a"]; exists {
		t.Fatal("delete did not finalize its tombstone")
	}
	scheduler.runNode(context.Background(), scheduledNode{jobID: "job-save-before-delete", nodeID: "node_a"})
	stored, _ = desiredStore.Load()
	if _, exists := stored.Nodes["node_a"]; exists {
		t.Fatal("last node job did not finalize the tombstone")
	}
	for _, jobID := range []string{"job-save-before-delete", "job-delete-after-save"} {
		job, exists := controller.jobs.Get(jobID)
		if !exists || !isTerminalJob(job.State) {
			t.Fatalf("job %s was orphaned: %#v exists=%t", jobID, job, exists)
		}
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

func TestSchedulerCompletesSixtyNodeImportWithFourConcurrentStarts(t *testing.T) {
	desired := controllerConfig()
	desired.Nodes = map[string]model.Node{}
	desired.Devices = map[string]model.Device{}
	imports := importer.New(importer.WithIDSource(func() string { return "preview-sixty" }))
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithImporter(imports), WithControllerJobIDSource(func() string { return "job-import-sixty" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	adapter := &schedulerAdapter{startRelease: release}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{L2TPConcurrency: 4, ProxyConcurrency: 8, ConnectTimeout: time.Second, StopTimeout: time.Second})
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)

	lines := make([]string, 60)
	for index := range lines {
		lines[index] = fmt.Sprintf("vpn-%02d.example|user-%02d|password-%02d", index, index, index)
	}
	previewResponse := controller.Handle(context.Background(), controllerRequest("preview-sixty", "import.preview", fmt.Sprintf(`{"protocol":"l2tp","raw":%q,"expected_revision":3}`, strings.Join(lines, "\n"))))
	assertControllerSuccess(t, previewResponse)
	var preview importer.Preview
	if err := json.Unmarshal(previewResponse.Result, &preview); err != nil {
		t.Fatal(err)
	}
	commitResponse := controller.Handle(context.Background(), controllerRequest("commit-sixty", "import.commit", fmt.Sprintf(`{"preview_id":%q,"preview_hash":%q,"expected_revision":3}`, preview.ID, preview.Hash)))
	assertControllerSuccess(t, commitResponse)
	waitForActiveStarts(t, adapter, 4)
	adapter.mu.Lock()
	maxActive := adapter.maxActive
	adapter.mu.Unlock()
	if maxActive != 4 {
		t.Fatalf("60-node import max concurrent starts = %d", maxActive)
	}
	close(release)
	waitForJobState(t, controller.jobs, "job-import-sixty", JobSucceeded)
	adapter.mu.Lock()
	starts := adapter.startCalls
	adapter.mu.Unlock()
	if starts != 60 {
		t.Fatalf("60-node import starts = %d", starts)
	}
}

func TestSchedulerEndsImportAttemptWhileBackgroundRecoveryKeepsRetrying(t *testing.T) {
	desired := controllerConfig()
	desired.Nodes = map[string]model.Node{}
	desired.Devices = map[string]model.Device{}
	jobIDs := []string{"job-import-attempt", "job-background-recovery"}
	imports := importer.New(importer.WithIDSource(func() string { return "preview-retry" }))
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithImporter(imports), WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	adapter := &schedulerAdapter{startRelease: release}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{ConnectTimeout: time.Second, StopTimeout: time.Second})
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)

	previewResponse := controller.Handle(context.Background(), controllerRequest(
		"preview-retry", "import.preview", `{"protocol":"l2tp","raw":"unreachable.example|user|password","expected_revision":3}`,
	))
	assertControllerSuccess(t, previewResponse)
	var preview importer.Preview
	if err := json.Unmarshal(previewResponse.Result, &preview); err != nil {
		t.Fatal(err)
	}
	commitResponse := controller.Handle(context.Background(), controllerRequest(
		"commit-retry", "import.commit", fmt.Sprintf(`{"preview_id":%q,"preview_hash":%q,"expected_revision":3}`, preview.ID, preview.Hash),
	))
	assertControllerSuccess(t, commitResponse)
	waitForActiveStarts(t, adapter, 1)
	stored, err := controller.desiredStore.Load()
	if err != nil || len(stored.Nodes) != 1 {
		t.Fatalf("imported desired nodes = %d, error = %v", len(stored.Nodes), err)
	}
	for nodeID := range stored.Nodes {
		adapter.failNode = nodeID
	}
	close(release)

	waitForJobState(t, controller.jobs, "job-import-attempt", JobFailed)
	status, exists := onlySchedulerStatus(controller)
	if !exists || status.State != model.StateBackoff {
		t.Fatalf("retryable import runtime = %#v exists=%t, want backoff", status, exists)
	}
	recovery, exists := controller.jobs.Get("job-background-recovery")
	if !exists || recovery.Kind != "system.recover" || recovery.State != JobRunning || len(recovery.Nodes) != 1 {
		t.Fatalf("background recovery job = %#v exists=%t", recovery, exists)
	}
}

func TestSchedulerHealthFailureRevokesAndReconnectsWithoutManualAction(t *testing.T) {
	jobIDs := []string{"job-connect", "job-health-recovery"}
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	var orderMu sync.Mutex
	order := []string{}
	adapter := &schedulerAdapter{onStop: func() {
		orderMu.Lock()
		order = append(order, "stop")
		orderMu.Unlock()
	}}
	route := &schedulerGate{name: "route", order: &order, orderMu: &orderMu}
	dns := &schedulerGate{name: "dns", order: &order, orderMu: &orderMu}
	authorization := &schedulerGate{name: "authorization", order: &order, orderMu: &orderMu}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{
		ConnectTimeout: time.Second, StopTimeout: time.Second, HealthCheckInterval: 5 * time.Millisecond,
	}, route, dns, authorization)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"connect", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`,
	)))
	waitForJobState(t, controller.jobs, "job-connect", JobSucceeded)
	orderMu.Lock()
	order = nil
	orderMu.Unlock()
	adapter.failNextProbes(1)

	waitForJobState(t, controller.jobs, "job-health-recovery", JobSucceeded)
	waitForNodeState(t, controller, "node_a", model.StateOnline)
	adapter.mu.Lock()
	starts, stops := adapter.startCalls, adapter.stopCalls
	adapter.mu.Unlock()
	if starts != 2 || stops != 1 {
		t.Fatalf("automatic health recovery start/stop calls = %d/%d, want 2/1", starts, stops)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	wantPrefix := []string{"close:authorization", "close:dns", "close:route", "stop"}
	if len(gotOrder) < len(wantPrefix) || !reflect.DeepEqual(gotOrder[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("health failure cleanup order = %v, want prefix %v", gotOrder, wantPrefix)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestSchedulerWaitsForAuthoritativeWANAndWakesRecoveryOnReturn(t *testing.T) {
	jobIDs := []string{"job-wan-attempt", "job-wan-recovery"}
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	wan := &schedulerWANSource{}
	adapter := &schedulerAdapter{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{
		ConnectTimeout: time.Second, StopTimeout: time.Second, WANCheckInterval: 5 * time.Millisecond,
	})
	scheduler.SetWANStatusSource(wan)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"wan-connect", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`,
	)))
	waitForJobState(t, controller.jobs, "job-wan-attempt", JobFailed)
	waitForNodeState(t, controller, "node_a", model.StateBackoff)
	adapter.mu.Lock()
	startsWhileDown := adapter.startCalls
	adapter.mu.Unlock()
	if startsWhileDown != 0 {
		t.Fatalf("adapter starts while WAN is down = %d", startsWhileDown)
	}
	wan.set(true, nil)
	waitForJobState(t, controller.jobs, "job-wan-recovery", JobSucceeded)
	waitForNodeState(t, controller, "node_a", model.StateOnline)
	adapter.mu.Lock()
	startsAfterReturn := adapter.startCalls
	adapter.mu.Unlock()
	if startsAfterReturn != 1 {
		t.Fatalf("adapter starts after WAN return = %d, want 1", startsAfterReturn)
	}
}

func TestSchedulerInterfaceEventClosesOwnedSessionBeforeRecreatingIt(t *testing.T) {
	jobIDs := []string{"job-before-ifdown", "job-ifdown-recovery"}
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	gate := &schedulerGate{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{ConnectTimeout: time.Second, StopTimeout: time.Second}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"before-ifdown", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`,
	)))
	waitForJobState(t, controller.jobs, "job-before-ifdown", JobSucceeded)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"ifdown", "system.interface_event", `{"interface":"ppv20001","action":"ifdown"}`,
	)))
	waitForJobState(t, controller.jobs, "job-ifdown-recovery", JobSucceeded)
	adapter.mu.Lock()
	starts, stops := adapter.startCalls, adapter.stopCalls
	adapter.mu.Unlock()
	if starts != 2 || stops != 1 || gate.closeCalls != 1 {
		t.Fatalf("ifdown recovery start/stop/gate-close = %d/%d/%d, want 2/1/1", starts, stops, gate.closeCalls)
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

func TestSchedulerPublishesSpecificL2TPStartFailure(t *testing.T) {
	desired := controllerConfig()
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-l2tp-failure" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{startErr: &model.CodeError{Code: ErrorCodeL2TPNoAddress, Message: "unsafe ppp detail"}}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{ConnectTimeout: time.Second, StopTimeout: time.Second})
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest(
		"l2tp-failure", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":3}`,
	)))
	waitForNodeStateOneOf(t, controller, "node_a", model.StateBackoff, model.StateFailed)
	status, exists := controller.schedulerStatus("node_a")
	if !exists || status.LastError == nil || status.LastError.Code != ErrorCodeL2TPNoAddress ||
		status.LastError.Message != "L2TP did not receive an IPv4 address" {
		t.Fatalf("published L2TP failure = %#v exists=%t", status.LastError, exists)
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

func TestSchedulerStartupReconcilesRestoredOnlineNodeAndRepublishesGates(t *testing.T) {
	desired := controllerConfig()
	nodeB := desired.Nodes["node_b"]
	nodeB.Enabled = false
	desired.Nodes["node_b"] = nodeB
	runtime := &memoryRuntimePersistence{
		exists: true,
		snapshot: RuntimeSnapshot{
			SchemaVersion:  RuntimeSnapshotSchemaVersion,
			ConfigRevision: desired.Revision,
			NodeStatuses: []NodeStatus{{
				NodeID: "node_a", JobID: "old-online", Generation: 7,
				State: model.StateOnline, UpdatedAt: stateTestEpoch,
			}},
		},
	}
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-startup-reconcile" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	gate := &schedulerGate{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)
	if err := controller.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	waitForJobState(t, controller.jobs, "job-startup-reconcile", JobSucceeded)
	if adapter.startCalls != 1 || adapter.probeCalls != 1 || gate.openCalls != 1 {
		t.Fatalf("restart reconciliation calls start/probe/gate = %d/%d/%d", adapter.startCalls, adapter.probeCalls, gate.openCalls)
	}
	status, exists := controller.schedulerStatus("node_a")
	if !exists || status.State != model.StateOnline || status.Generation != 7 {
		t.Fatalf("restart fabricated a new generation: %#v exists=%t", status, exists)
	}
}

func TestSchedulerRefreshesOnlineNodeAfterDeviceBindingChanges(t *testing.T) {
	desired := controllerConfig()
	jobIDs := []string{"job-initial-connect", "job-device-refresh"}
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { id := jobIDs[0]; jobIDs = jobIDs[1:]; return id }),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	gate := &schedulerGate{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("initial-connect", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`)))
	waitForNodeState(t, controller, "node_a", model.StateOnline)
	adapter.mu.Lock()
	startsBefore := adapter.startCalls
	adapter.mu.Unlock()
	opensBefore := gate.openCalls

	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("bind-refresh", "device.bind", `{"device_id":"device_a","node_id":"node_a","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-device-refresh", JobSucceeded)
	adapter.mu.Lock()
	startsAfter := adapter.startCalls
	adapter.mu.Unlock()
	if startsAfter != startsBefore+1 || gate.openCalls != opensBefore+1 {
		t.Fatalf("online binding did not refresh adapter/gates: starts %d->%d gates %d->%d", startsBefore, startsAfter, opensBefore, gate.openCalls)
	}
}

func TestSchedulerRebindRefreshesOldNodeBeforeOpeningNewNode(t *testing.T) {
	desired := controllerConfig()
	runtime := &memoryRuntimePersistence{
		exists: true,
		snapshot: RuntimeSnapshot{
			SchemaVersion: RuntimeSnapshotSchemaVersion, ConfigRevision: desired.Revision,
			NodeStatuses: []NodeStatus{
				{NodeID: "node_a", JobID: "old-a", Generation: 7, State: model.StateOnline, UpdatedAt: stateTestEpoch},
				{NodeID: "node_b", JobID: "old-b", Generation: 8, State: model.StateOnline, UpdatedAt: stateTestEpoch},
			},
		},
	}
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-ordered-rebind" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	oldEntered := make(chan struct{})
	newEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	defer func() {
		select {
		case <-releaseOld:
		default:
			close(releaseOld)
		}
	}()
	gate := &orderedRebindGate{oldEntered: oldEntered, newEntered: newEntered, releaseOld: releaseOld}
	scheduler := NewScheduler(controller, &schedulerAdapter{}, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("ordered-rebind", "device.bind", `{"device_id":"device_a","node_id":"node_b","expected_revision":3}`)))
	select {
	case <-oldEntered:
	case <-time.After(time.Second):
		t.Fatal("old node refresh did not start")
	}
	select {
	case <-newEntered:
		t.Fatal("new node opened before old node authorization was refreshed")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseOld)
	waitForJobState(t, controller.jobs, "job-ordered-rebind", JobSucceeded)
	select {
	case <-newEntered:
	case <-time.After(time.Second):
		t.Fatal("new node did not open after old node refresh")
	}
}

func TestSchedulerBatchRebindRefreshesOldNodeBeforeOpeningTarget(t *testing.T) {
	desired := controllerConfig()
	runtime := &memoryRuntimePersistence{
		exists: true,
		snapshot: RuntimeSnapshot{
			SchemaVersion: RuntimeSnapshotSchemaVersion, ConfigRevision: desired.Revision,
			NodeStatuses: []NodeStatus{
				{NodeID: "node_a", JobID: "old-a", Generation: 7, State: model.StateOnline, UpdatedAt: stateTestEpoch},
				{NodeID: "node_b", JobID: "old-b", Generation: 8, State: model.StateOnline, UpdatedAt: stateTestEpoch},
			},
		},
	}
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-ordered-batch-rebind" }),
		WithDeviceServices(&controllerDeviceSource{devices: []platform.DiscoveredDevice{{
			ID: "device_a", MAC: "00:11:22:33:44:55", IPv4: netip.MustParseAddr("192.168.9.10"), Hostname: "Device A", Confirmed: true,
		}}}, &controllerLeaseManager{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	oldEntered := make(chan struct{})
	newEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	defer func() {
		select {
		case <-releaseOld:
		default:
			close(releaseOld)
		}
	}()
	gate := &orderedRebindGate{oldEntered: oldEntered, newEntered: newEntered, releaseOld: releaseOld}
	scheduler := NewScheduler(controller, &schedulerAdapter{}, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("ordered-batch-rebind", "device.bindings.replace", `{"node_id":"node_b","device_ids":["device_a"],"expected_revision":3}`)))
	select {
	case <-oldEntered:
	case <-time.After(time.Second):
		t.Fatal("old node refresh did not start")
	}
	select {
	case <-newEntered:
		t.Fatal("batch target opened before old node authorization was refreshed")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseOld)
	waitForJobState(t, controller.jobs, "job-ordered-batch-rebind", JobSucceeded)
	select {
	case <-newEntered:
	case <-time.After(time.Second):
		t.Fatal("batch target did not open after old node refresh")
	}
}

func TestSchedulerRebindDoesNotOpenNewNodeWhileOldNodeIsBackingOff(t *testing.T) {
	desired := controllerConfig()
	runtime := &memoryRuntimePersistence{
		exists: true,
		snapshot: RuntimeSnapshot{
			SchemaVersion: RuntimeSnapshotSchemaVersion, ConfigRevision: desired.Revision,
			NodeStatuses: []NodeStatus{
				{NodeID: "node_a", JobID: "old-a", Generation: 7, State: model.StateOnline, UpdatedAt: stateTestEpoch},
				{NodeID: "node_b", JobID: "old-b", Generation: 8, State: model.StateOnline, UpdatedAt: stateTestEpoch},
			},
		},
	}
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-failing-rebind" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	oldEntered := make(chan struct{})
	newEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	gate := &orderedRebindGate{
		oldEntered: oldEntered,
		newEntered: newEntered,
		releaseOld: releaseOld,
		oldErr:     errors.New("old authorization refresh failed"),
	}
	close(releaseOld)
	scheduler := NewScheduler(controller, &schedulerAdapter{}, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("failing-rebind", "device.bind", `{"device_id":"device_a","node_id":"node_b","expected_revision":3}`)))
	select {
	case <-oldEntered:
	case <-time.After(time.Second):
		t.Fatal("old node refresh did not start")
	}
	select {
	case <-newEntered:
		t.Fatal("new node opened while old node refresh was waiting to retry")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSchedulerRebindPermanentOldNodeFailureBlocksFollowingNodeAndTerminatesJob(t *testing.T) {
	desired := controllerConfig()
	runtime := &memoryRuntimePersistence{
		exists: true,
		snapshot: RuntimeSnapshot{
			SchemaVersion: RuntimeSnapshotSchemaVersion, ConfigRevision: desired.Revision,
			NodeStatuses: []NodeStatus{
				{NodeID: "node_a", JobID: "old-a", Generation: 7, State: model.StateOnline, UpdatedAt: stateTestEpoch},
				{NodeID: "node_b", JobID: "old-b", Generation: 8, State: model.StateOnline, UpdatedAt: stateTestEpoch},
			},
		},
	}
	controller, err := NewController(
		&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-permanent-rebind" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	oldEntered := make(chan struct{})
	newEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	close(releaseOld)
	gate := &orderedRebindGate{
		oldEntered: oldEntered,
		newEntered: newEntered,
		releaseOld: releaseOld,
		oldErr:     &model.CodeError{Code: ErrorCodeAuthentication, Message: "credentials rejected"},
	}
	scheduler := NewScheduler(controller, &schedulerAdapter{}, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("permanent-rebind", "device.bind", `{"device_id":"device_a","node_id":"node_b","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-permanent-rebind", JobFailed)
	job, _ := controller.jobs.Get("job-permanent-rebind")
	if len(job.Nodes) != 2 || job.Nodes[0].State != model.StateFailed || job.Nodes[1].State != model.StateFailed || job.Nodes[1].Step != "blocked_by_previous_node" {
		t.Fatalf("permanent rebind outcome = %#v", job)
	}
	select {
	case <-newEntered:
		t.Fatal("new node opened after permanent old-node failure")
	default:
	}
}

func TestSchedulerShutdownClosesGatesAndOwnedSessionWithoutChangingDesiredState(t *testing.T) {
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-shutdown" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &schedulerAdapter{}
	gate := &schedulerGate{}
	scheduler := NewScheduler(controller, adapter, SchedulerConfig{}, gate)
	controller.AttachScheduler(scheduler)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("connect-for-shutdown", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`)))
	waitForNodeState(t, controller, "node_a", model.StateOnline)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gate.closeCalls != 1 || adapter.stopCalls != 1 {
		t.Fatalf("shutdown cleanup calls gate/adapter = %d/%d", gate.closeCalls, adapter.stopCalls)
	}
	status, _ := controller.schedulerStatus("node_a")
	if status.State != model.StateOnline {
		t.Fatalf("shutdown rewrote desired restart observation: %#v", status)
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

func TestSchedulerStopsUsingTheSessionOwnershipGeneration(t *testing.T) {
	jobIDs := []string{"job-connect-generation", "job-stop-generation"}
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { id := jobIDs[0]; jobIDs = jobIDs[1:]; return id }),
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
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("connect-before-stop", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":3}`)))
	waitForNodeState(t, controller, "node_a", model.StateOnline)
	assertControllerSuccess(t, controller.Handle(context.Background(), controllerRequest("stop-generation", "node.action", `{"node_id":"node_a","action":"stop","expected_revision":3}`)))
	waitForJobState(t, controller.jobs, "job-stop-generation", JobSucceeded)
	adapter.mu.Lock()
	mismatch := adapter.stopGenerationMismatch
	adapter.mu.Unlock()
	if mismatch {
		t.Fatal("stop used the post-transition generation instead of the owned session generation")
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
	mu            sync.Mutex
	cfg           model.DesiredConfig
	replaceCount  int
	failReplaceAt int
}

func (store *memoryDesiredStore) Load() (model.DesiredConfig, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneControllerConfig(store.cfg), nil
}

func (store *memoryDesiredStore) Replace(_ context.Context, expected uint64, next model.DesiredConfig) (model.DesiredConfig, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.replaceCount++
	if store.failReplaceAt > 0 && store.replaceCount == store.failReplaceAt {
		return model.DesiredConfig{}, errors.New("desired persistence failed")
	}
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

func (store *memoryDesiredStore) EnsureDurable(context.Context) error { return nil }

type memoryRuntimePersistence struct {
	mu              sync.Mutex
	snapshot        RuntimeSnapshot
	exists          bool
	saveCount       int
	failSaveAt      int
	failSaveFrom    int
	failSaveThrough int
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
	store.saveCount++
	if store.failSaveAt > 0 && store.saveCount == store.failSaveAt {
		return errors.New("runtime persistence failed")
	}
	if store.failSaveFrom > 0 && store.saveCount >= store.failSaveFrom && store.saveCount <= store.failSaveThrough {
		return errors.New("runtime persistence failed")
	}
	store.snapshot, store.exists = normalized, true
	return nil
}

type schedulerAdapter struct {
	mu                        sync.Mutex
	startRelease              <-chan struct{}
	blockUntilContext         bool
	failNode                  string
	startErr                  error
	runtimeSaves              func() int
	onStop                    func()
	active                    int
	maxActive                 int
	startCalls                int
	probeCalls                int
	probeFailures             int
	stopCalls                 int
	stopGenerationMismatch    bool
	requiredStopGeneration    uint64
	rejectWrongStopGeneration bool
	stopGenerations           []uint64
	saveCountAtStart          int
}

type schedulerTrafficReader struct {
	mu       sync.Mutex
	counters platform.InterfaceCounters
	err      error
}

type schedulerWANSource struct {
	mu        sync.Mutex
	available bool
	err       error
}

func (source *schedulerWANSource) set(available bool, err error) {
	source.mu.Lock()
	source.available = available
	source.err = err
	source.mu.Unlock()
}

func (source *schedulerWANSource) Available(context.Context) (bool, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.available, source.err
}

func (reader *schedulerTrafficReader) set(counters platform.InterfaceCounters, err error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.counters = counters
	reader.err = err
}

func (reader *schedulerTrafficReader) ReadInterfaceCounters(string) (platform.InterfaceCounters, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.counters, reader.err
}

func waitForTraffic(t *testing.T, scheduler *Scheduler, nodeID string, ready func(TrafficSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot := scheduler.Traffic(nodeID); ready(snapshot) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("traffic for %s did not reach expected state: %#v", nodeID, scheduler.Traffic(nodeID))
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
	if adapter.startErr != nil {
		return platform.Session{NodeID: request.Node.ID, Generation: request.Generation, Protocol: request.Node.Protocol}, adapter.startErr
	}
	return platform.Session{NodeID: request.Node.ID, Generation: request.Generation, Protocol: request.Node.Protocol, Interface: "l2tp-test", OwnershipDigest: "owned"}, nil
}

func (adapter *schedulerAdapter) Probe(_ context.Context, _ platform.NodeRequest, _ platform.Session) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.probeCalls++
	if adapter.probeFailures > 0 {
		adapter.probeFailures--
		return errors.New("isolated health probe failure")
	}
	return nil
}

func (adapter *schedulerAdapter) failNextProbes(count int) {
	adapter.mu.Lock()
	adapter.probeFailures = count
	adapter.mu.Unlock()
}

func (adapter *schedulerAdapter) Stop(_ context.Context, request platform.NodeRequest, session platform.Session) error {
	adapter.mu.Lock()
	adapter.stopCalls++
	adapter.stopGenerations = append(adapter.stopGenerations, request.Generation)
	if session.Generation != 0 && request.Generation != session.Generation {
		adapter.stopGenerationMismatch = true
	}
	reject := adapter.rejectWrongStopGeneration && request.Generation != adapter.requiredStopGeneration
	adapter.mu.Unlock()
	if adapter.onStop != nil {
		adapter.onStop()
	}
	if reject {
		return errors.New("owned session generation mismatch")
	}
	return nil
}

type schedulerGate struct {
	name          string
	openRelease   <-chan struct{}
	openErr       error
	closeErr      error
	closeFailures int
	order         *[]string
	orderMu       *sync.Mutex
	openCalls     int
	closeCalls    int
}

type orderedRebindGate struct {
	oldEntered chan struct{}
	newEntered chan struct{}
	releaseOld <-chan struct{}
	oldErr     error
	oldOnce    sync.Once
	newOnce    sync.Once
}

func (gate *orderedRebindGate) Open(ctx context.Context, request platform.NodeRequest, _ platform.Session) error {
	switch request.Node.ID {
	case "node_a":
		gate.oldOnce.Do(func() { close(gate.oldEntered) })
		select {
		case <-gate.releaseOld:
		case <-ctx.Done():
			return ctx.Err()
		}
		return gate.oldErr
	case "node_b":
		gate.newOnce.Do(func() { close(gate.newEntered) })
	}
	return nil
}

func (gate *orderedRebindGate) Close(context.Context, platform.NodeRequest, platform.Session) error {
	return nil
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
	if gate.closeFailures > 0 {
		gate.closeFailures--
		return errors.New("one-shot close failure")
	}
	return gate.closeErr
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

func onlySchedulerStatus(controller *Controller) (NodeStatus, bool) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	for _, status := range controller.statuses {
		return cloneNodeStatus(status), true
	}
	return NodeStatus{}, false
}
