package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

const schedulerQueueCapacity = MaxRetainedJobs * 2

type SchedulerConfig struct {
	L2TPConcurrency  int
	ProxyConcurrency int
	ConnectTimeout   time.Duration
	StopTimeout      time.Duration
}

type scheduledNode struct {
	jobID  string
	nodeID string
}

// Scheduler is the sole owner of protocol and dataplane side effects. Control
// requests only create durable jobs; Run reconciles those jobs independently.
type Scheduler struct {
	controller *Controller
	adapter    platform.NodeAdapter
	gates      []platform.SessionGate
	config     SchedulerConfig
	queue      chan Job

	mu        sync.Mutex
	submitted map[string]struct{}
	nodeLocks map[string]*sync.Mutex
	sessions  map[string]platform.Session
	l2tp      chan struct{}
	proxy     chan struct{}
}

func NewScheduler(controller *Controller, adapter platform.NodeAdapter, config SchedulerConfig, gates ...platform.SessionGate) *Scheduler {
	if config.L2TPConcurrency < 1 {
		config.L2TPConcurrency = 4
	}
	if config.ProxyConcurrency < 1 {
		config.ProxyConcurrency = 8
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 30 * time.Second
	}
	if config.StopTimeout <= 0 {
		config.StopTimeout = 10 * time.Second
	}
	return &Scheduler{
		controller: controller,
		adapter:    adapter,
		gates:      append([]platform.SessionGate(nil), gates...),
		config:     config,
		queue:      make(chan Job, schedulerQueueCapacity),
		submitted:  make(map[string]struct{}),
		nodeLocks:  make(map[string]*sync.Mutex),
		sessions:   make(map[string]platform.Session),
		l2tp:       make(chan struct{}, config.L2TPConcurrency),
		proxy:      make(chan struct{}, config.ProxyConcurrency),
	}
}

// Submit is intentionally memory-only and non-blocking at the side-effect
// boundary. The job has already been persisted and Run also recovers it.
func (scheduler *Scheduler) Submit(job Job) {
	if scheduler == nil || scheduler.controller == nil || scheduler.adapter == nil || isTerminalJob(job.State) {
		return
	}
	scheduler.mu.Lock()
	if _, exists := scheduler.submitted[job.ID]; exists {
		scheduler.mu.Unlock()
		return
	}
	scheduler.submitted[job.ID] = struct{}{}
	scheduler.mu.Unlock()
	scheduler.queue <- cloneJob(job)
}

func (scheduler *Scheduler) Run(ctx context.Context) error {
	if scheduler == nil || scheduler.controller == nil || scheduler.adapter == nil {
		return errors.New("scheduler dependencies are missing")
	}
	for _, job := range scheduler.controller.jobs.List() {
		if job.State == JobQueued || job.State == JobRunning {
			scheduler.Submit(job)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case job := <-scheduler.queue:
			scheduler.mu.Lock()
			delete(scheduler.submitted, job.ID)
			scheduler.mu.Unlock()
			for _, progress := range job.Nodes {
				work := scheduledNode{jobID: job.ID, nodeID: progress.NodeID}
				go scheduler.runNode(ctx, work)
			}
		}
	}
}

func (scheduler *Scheduler) runNode(ctx context.Context, work scheduledNode) {
	lock := scheduler.nodeLock(work.nodeID)
	lock.Lock()
	defer lock.Unlock()

	job, node, exists := scheduler.controller.schedulerWork(work.jobID, work.nodeID)
	if !exists || isTerminalJob(job.State) {
		return
	}
	if job.Kind == "device.unbind" {
		_ = scheduler.controller.schedulerCompleteNode(ctx, work.jobID, work.nodeID, "unbound")
		return
	}
	if job.Kind == "node.stop" {
		status, exists := scheduler.controller.schedulerStatus(work.nodeID)
		scheduler.stopNode(ctx, work, node, status, exists, false)
		return
	}

	status, exists, err := scheduler.controller.schedulerEnsureKnown(ctx, work.jobID, work.nodeID)
	if err != nil {
		return
	}
	if exists && status.State == model.StateOnline && job.Kind != "node.reconnect" {
		_ = scheduler.controller.schedulerRecordStatus(ctx, work.jobID, status, "online")
		return
	}
	if exists && status.State != model.StateQueued && status.State != model.StateDisabled && status.State != model.StateFailed && status.State != model.StateBackoff {
		status = scheduler.prepareReconnect(ctx, work, node, status)
		if status.State != model.StateQueued {
			return
		}
	} else if exists && status.State == model.StateDisabled {
		if status.Generation == ^uint64(0) {
			return
		}
		status, err = scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation+1, EventEnable, nil, "queued")
		if err != nil {
			return
		}
	} else if exists && (status.State == model.StateFailed || status.State == model.StateBackoff) {
		status, err = scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, EventManualReconnect, nil, "queued")
		if err != nil {
			return
		}
	}
	scheduler.startNode(ctx, work, node, status)
}

func (scheduler *Scheduler) prepareReconnect(ctx context.Context, work scheduledNode, node model.Node, status NodeStatus) NodeStatus {
	updated, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, EventManualReconnect, nil, "stopping")
	if err != nil {
		return status
	}
	if updated.State == model.StateQueued {
		return updated
	}
	return scheduler.cleanupBarrier(ctx, work, node, updated)
}

func (scheduler *Scheduler) stopNode(ctx context.Context, work scheduledNode, node model.Node, status NodeStatus, exists, reconnect bool) {
	if !exists || status.State == model.StateDisabled {
		_ = scheduler.controller.schedulerCompleteNode(ctx, work.jobID, work.nodeID, "stopped")
		return
	}
	kind := EventStop
	step := "stopping"
	if reconnect {
		kind = EventManualReconnect
	}
	updated, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, kind, nil, step)
	if err != nil {
		return
	}
	_ = scheduler.cleanupBarrier(ctx, work, node, updated)
}

func (scheduler *Scheduler) cleanupBarrier(ctx context.Context, work scheduledNode, node model.Node, status NodeStatus) NodeStatus {
	request := platform.NodeRequest{Node: node, JobID: work.jobID, Generation: status.Generation}
	session := scheduler.takeSession(work.nodeID)
	stopCtx, cancel := context.WithTimeout(context.Background(), scheduler.config.StopTimeout)
	defer cancel()
	for index := len(scheduler.gates) - 1; index >= 0; index-- {
		_ = scheduler.gates[index].Close(stopCtx, request, session)
	}
	if err := scheduler.adapter.Stop(stopCtx, request, session); err != nil {
		failed, _ := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, EventFailure, &model.CodeError{Code: ErrorCodeStopTimeout}, "cleanup_failed")
		return failed
	}
	complete := EventStopped
	if status.State == model.StateRecovering {
		if status.CleanupPending {
			complete = EventCleanupComplete
		} else {
			complete = EventRecovered
		}
	}
	updated, _ := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, complete, nil, "cleanup_complete")
	return updated
}

func (scheduler *Scheduler) startNode(ctx context.Context, work scheduledNode, node model.Node, status NodeStatus) {
	semaphore := scheduler.semaphore(node.Protocol)
	select {
	case semaphore <- struct{}{}:
		defer func() { <-semaphore }()
	case <-ctx.Done():
		return
	}

	starting, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, EventStart, nil, "starting")
	if err != nil {
		return
	}
	request := platform.NodeRequest{Node: node, JobID: work.jobID, Generation: starting.Generation}
	connectCtx, cancel := context.WithTimeout(ctx, scheduler.config.ConnectTimeout)
	defer cancel()
	session, err := scheduler.adapter.Start(connectCtx, request)
	if err != nil {
		scheduler.cleanupFailedStart(request, session, nil)
		scheduler.failStart(ctx, work, starting, err, ErrorCodeDataplaneFailed)
		return
	}
	validating, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, starting.Generation, EventStarted, nil, "probing")
	if err != nil {
		scheduler.cleanupFailedStart(request, session, nil)
		return
	}
	if err := scheduler.adapter.Probe(connectCtx, request, session); err != nil {
		scheduler.cleanupFailedStart(request, session, nil)
		scheduler.failStart(ctx, work, validating, err, ErrorCodeProbeFailed)
		return
	}
	opened := make([]platform.SessionGate, 0, len(scheduler.gates))
	for _, gate := range scheduler.gates {
		if err := gate.Open(connectCtx, request, session); err != nil {
			scheduler.cleanupFailedStart(request, session, opened)
			scheduler.failStart(ctx, work, validating, err, ErrorCodeDataplaneFailed)
			return
		}
		opened = append(opened, gate)
	}
	online, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, validating.Generation, EventValidated, nil, "online")
	if err != nil {
		scheduler.cleanupFailedStart(request, session, opened)
		return
	}
	scheduler.putSession(work.nodeID, session)
	_ = online
}

func (scheduler *Scheduler) cleanupFailedStart(request platform.NodeRequest, session platform.Session, opened []platform.SessionGate) {
	stopCtx, cancel := context.WithTimeout(context.Background(), scheduler.config.StopTimeout)
	defer cancel()
	for index := len(opened) - 1; index >= 0; index-- {
		_ = opened[index].Close(stopCtx, request, session)
	}
	_ = scheduler.adapter.Stop(stopCtx, request, session)
}

func (scheduler *Scheduler) failStart(ctx context.Context, work scheduledNode, status NodeStatus, cause error, code string) {
	kind := EventFailure
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		kind = EventTimeout
		code = ErrorCodeConnectTimeout
	}
	failed, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, kind, &model.CodeError{Code: code}, "failed")
	if err == nil && failed.State == model.StateBackoff {
		scheduler.scheduleRetry(ctx, work, failed)
	}
}

func (scheduler *Scheduler) scheduleRetry(ctx context.Context, work scheduledNode, status NodeStatus) {
	delay := time.Until(status.RetryAt)
	if delay < 0 {
		delay = 0
	}
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		lock := scheduler.nodeLock(work.nodeID)
		lock.Lock()
		defer lock.Unlock()
		queued, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, EventRetryDue, nil, "queued")
		if err != nil {
			return
		}
		_, node, exists := scheduler.controller.schedulerWork(work.jobID, work.nodeID)
		if exists {
			scheduler.startNode(ctx, work, node, queued)
		}
	}()
}

func (scheduler *Scheduler) semaphore(protocol model.Protocol) chan struct{} {
	if protocol == model.ProtocolL2TP {
		return scheduler.l2tp
	}
	return scheduler.proxy
}

func (scheduler *Scheduler) nodeLock(nodeID string) *sync.Mutex {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	lock := scheduler.nodeLocks[nodeID]
	if lock == nil {
		lock = &sync.Mutex{}
		scheduler.nodeLocks[nodeID] = lock
	}
	return lock
}

func (scheduler *Scheduler) putSession(nodeID string, session platform.Session) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	scheduler.sessions[nodeID] = session
}

func (scheduler *Scheduler) takeSession(nodeID string) platform.Session {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	session := scheduler.sessions[nodeID]
	delete(scheduler.sessions, nodeID)
	return session
}

func (controller *Controller) schedulerWork(jobID, nodeID string) (Job, model.Node, bool) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	job, exists := controller.jobs.Get(jobID)
	if !exists {
		return Job{}, model.Node{}, false
	}
	node, exists := controller.desired.Nodes[nodeID]
	return job, node, exists
}

func (controller *Controller) schedulerStatus(nodeID string) (NodeStatus, bool) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	status, exists := controller.statuses[nodeID]
	return cloneNodeStatus(status), exists
}

func (controller *Controller) schedulerEnsureKnown(ctx context.Context, jobID, nodeID string) (NodeStatus, bool, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if status, exists := controller.machine.Status(nodeID); exists {
		return status, true, nil
	}
	generation := uint64(1)
	if retained, exists := controller.statuses[nodeID]; exists && retained.Generation < ^uint64(0) {
		generation = retained.Generation + 1
	}
	status, err := controller.schedulerApplyLocked(ctx, Event{NodeID: nodeID, JobID: jobID, Generation: generation, Kind: EventEnable, At: controller.now()}, "queued")
	return status, false, err
}

func (controller *Controller) schedulerApply(ctx context.Context, jobID, nodeID string, generation uint64, kind EventKind, failure *model.CodeError, step string) (NodeStatus, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.schedulerApplyLocked(ctx, Event{NodeID: nodeID, JobID: jobID, Generation: generation, Kind: kind, Err: failure, At: controller.now()}, step)
}

func (controller *Controller) schedulerApplyLocked(ctx context.Context, event Event, step string) (NodeStatus, error) {
	beforeJobs := controller.jobs.Snapshot()
	beforeMachine, machineExists := controller.machine.Status(event.NodeID)
	beforeStatus, statusExists := controller.statuses[event.NodeID]
	status, err := controller.machine.Apply(event)
	if err != nil {
		return NodeStatus{}, err
	}
	controller.statuses[event.NodeID] = cloneNodeStatus(status)
	if err := controller.updateJobForStatus(event.JobID, status, step); err != nil {
		controller.machine.restoreNode(event.NodeID, beforeMachine, machineExists)
		if statusExists {
			controller.statuses[event.NodeID] = beforeStatus
		} else {
			delete(controller.statuses, event.NodeID)
		}
		return NodeStatus{}, err
	}
	if _, err := controller.jobs.AppendEvent(NodeEvent{JobID: event.JobID, NodeID: event.NodeID, Generation: status.Generation, State: status.State, Attempt: status.Attempts, At: event.At, Error: status.LastError}); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		controller.machine.restoreNode(event.NodeID, beforeMachine, machineExists)
		if statusExists {
			controller.statuses[event.NodeID] = beforeStatus
		} else {
			delete(controller.statuses, event.NodeID)
		}
		return NodeStatus{}, err
	}
	if err := controller.persistLocked(ctx); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		controller.machine.restoreNode(event.NodeID, beforeMachine, machineExists)
		if statusExists {
			controller.statuses[event.NodeID] = beforeStatus
		} else {
			delete(controller.statuses, event.NodeID)
		}
		return NodeStatus{}, err
	}
	return status, nil
}

func (controller *Controller) schedulerRecordStatus(ctx context.Context, jobID string, status NodeStatus, step string) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	before := controller.jobs.Snapshot()
	if err := controller.updateJobForStatus(jobID, status, step); err != nil {
		return err
	}
	if err := controller.persistLocked(ctx); err != nil {
		_ = controller.jobs.Restore(before)
		return err
	}
	return nil
}

func (controller *Controller) schedulerCompleteNode(ctx context.Context, jobID, nodeID, step string) error {
	return controller.schedulerRecordStatus(ctx, jobID, NodeStatus{NodeID: nodeID, State: model.StateDisabled}, step)
}

func (controller *Controller) updateJobForStatus(jobID string, status NodeStatus, step string) error {
	job, exists := controller.jobs.Get(jobID)
	if !exists {
		return fmt.Errorf("scheduler job is missing")
	}
	found := false
	for index := range job.Nodes {
		if job.Nodes[index].NodeID != status.NodeID {
			continue
		}
		job.Nodes[index].Step = step
		job.Nodes[index].State = status.State
		job.Nodes[index].Attempt = status.Attempts
		job.Nodes[index].Error = clonePublicError(status.LastError)
		found = true
		break
	}
	if !found {
		return fmt.Errorf("scheduler job node is missing")
	}
	job.Queued, job.Running, job.Succeeded, job.Failed, job.CancelledNodes = 0, 0, 0, 0, 0
	for _, progress := range job.Nodes {
		switch nodeProgressBucket(progress) {
		case jobBucketQueued:
			job.Queued++
		case jobBucketRunning:
			job.Running++
		case jobBucketSucceeded:
			job.Succeeded++
		case jobBucketFailed:
			job.Failed++
		case jobBucketCancelled:
			job.CancelledNodes++
		}
	}
	switch {
	case job.Succeeded == job.Total:
		job.State = JobSucceeded
	case job.Queued+job.Running > 0:
		job.State = JobRunning
	case job.Failed > 0:
		job.State = JobFailed
	default:
		return fmt.Errorf("scheduler job has no valid outcome")
	}
	return controller.jobs.Put(job)
}
