package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

const schedulerQueueCapacity = MaxRetainedJobs * 2

type SchedulerConfig struct {
	L2TPConcurrency       int
	ProxyConcurrency      int
	ConnectTimeout        time.Duration
	StopTimeout           time.Duration
	TrafficSampleInterval time.Duration
	HealthCheckInterval   time.Duration
	WANCheckInterval      time.Duration
}

type scheduledNode struct {
	jobID  string
	nodeID string
}

// Scheduler is the sole owner of protocol and dataplane side effects. Control
// requests only create durable jobs; Run reconciles those jobs independently.
type Scheduler struct {
	controller    *Controller
	adapter       platform.NodeAdapter
	trafficReader platform.InterfaceTrafficReader
	traffic       *trafficTracker
	gates         []platform.SessionGate
	config        SchedulerConfig
	queue         chan Job

	mu        sync.Mutex
	submitted map[string]struct{}
	nodeLocks map[string]*sync.Mutex
	sessions  map[string]platform.Session
	l2tp      chan struct{}
	proxy     chan struct{}
	workers   sync.WaitGroup
	health    chan struct{}
	healthRun chan struct{}
	wanSource platform.WANStatusSource
	wanKnown  bool
	wanUp     bool
}

func NewScheduler(controller *Controller, adapter platform.NodeAdapter, config SchedulerConfig, gates ...platform.SessionGate) *Scheduler {
	return NewSchedulerWithTraffic(controller, adapter, nil, config, gates...)
}

func NewSchedulerWithTraffic(controller *Controller, adapter platform.NodeAdapter, trafficReader platform.InterfaceTrafficReader, config SchedulerConfig, gates ...platform.SessionGate) *Scheduler {
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
	if config.TrafficSampleInterval <= 0 {
		config.TrafficSampleInterval = time.Second
	}
	if config.HealthCheckInterval <= 0 {
		config.HealthCheckInterval = 10 * time.Second
	}
	if config.WANCheckInterval <= 0 {
		config.WANCheckInterval = 5 * time.Second
	}
	return &Scheduler{
		controller:    controller,
		adapter:       adapter,
		trafficReader: trafficReader,
		traffic:       newTrafficTracker(),
		gates:         append([]platform.SessionGate(nil), gates...),
		config:        config,
		queue:         make(chan Job, schedulerQueueCapacity),
		submitted:     make(map[string]struct{}),
		nodeLocks:     make(map[string]*sync.Mutex),
		sessions:      make(map[string]platform.Session),
		l2tp:          make(chan struct{}, config.L2TPConcurrency),
		proxy:         make(chan struct{}, config.ProxyConcurrency),
		health:        make(chan struct{}, config.L2TPConcurrency),
		healthRun:     make(chan struct{}, 1),
		wanKnown:      true,
		wanUp:         true,
	}
}

func (scheduler *Scheduler) SetWANStatusSource(source platform.WANStatusSource) {
	if scheduler == nil {
		return
	}
	scheduler.mu.Lock()
	scheduler.wanSource = source
	if source != nil {
		scheduler.wanKnown = false
		scheduler.wanUp = false
	} else {
		scheduler.wanKnown = true
		scheduler.wanUp = true
	}
	scheduler.mu.Unlock()
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
	scheduler.mu.Lock()
	wanSource := scheduler.wanSource
	scheduler.mu.Unlock()
	if wanSource != nil {
		scheduler.refreshWAN(ctx, wanSource)
	}
	for _, job := range scheduler.controller.jobs.List() {
		if job.State == JobQueued || job.State == JobRunning {
			scheduler.Submit(job)
		}
	}
	var trafficTicker *time.Ticker
	var trafficTicks <-chan time.Time
	if scheduler.trafficReader != nil {
		trafficTicker = time.NewTicker(scheduler.config.TrafficSampleInterval)
		trafficTicks = trafficTicker.C
		defer trafficTicker.Stop()
	}
	healthTicker := time.NewTicker(scheduler.config.HealthCheckInterval)
	defer healthTicker.Stop()
	var wanTicker *time.Ticker
	var wanTicks <-chan time.Time
	if wanSource != nil {
		wanTicker = time.NewTicker(scheduler.config.WANCheckInterval)
		wanTicks = wanTicker.C
		defer wanTicker.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			scheduler.workers.Wait()
			return nil
		case sampledAt := <-trafficTicks:
			scheduler.sampleTraffic(sampledAt)
		case <-healthTicker.C:
			scheduler.checkSessions(ctx)
		case <-wanTicks:
			scheduler.refreshWAN(ctx, wanSource)
		case job := <-scheduler.queue:
			scheduler.mu.Lock()
			delete(scheduler.submitted, job.ID)
			scheduler.mu.Unlock()
			if (job.Kind == "device.bind" || job.Kind == "device.bindings.replace") && len(job.Nodes) > 1 {
				scheduler.workers.Add(1)
				go func(job Job) {
					defer scheduler.workers.Done()
					for _, progress := range job.Nodes {
						scheduler.runNode(ctx, scheduledNode{jobID: job.ID, nodeID: progress.NodeID})
						if ctx.Err() != nil {
							return
						}
						if !scheduler.waitForOrderedNode(ctx, job.ID, progress.NodeID) {
							_ = scheduler.controller.schedulerFailFollowingNodes(ctx, job.ID, progress.NodeID)
							return
						}
					}
				}(cloneJob(job))
				continue
			}
			for _, progress := range job.Nodes {
				work := scheduledNode{jobID: job.ID, nodeID: progress.NodeID}
				scheduler.workers.Add(1)
				go func(work scheduledNode) {
					defer scheduler.workers.Done()
					scheduler.runNode(ctx, work)
				}(work)
			}
		}
	}
}

// Shutdown removes only side effects owned by sessions established by this
// process. Runtime status remains observationally online so a later daemon can
// reconcile and recreate the same desired node.
func (scheduler *Scheduler) Shutdown(ctx context.Context) error {
	if scheduler == nil || scheduler.controller == nil || scheduler.adapter == nil {
		return errors.New("scheduler dependencies are missing")
	}
	scheduler.mu.Lock()
	nodeIDs := make([]string, 0, len(scheduler.sessions))
	sessions := make(map[string]platform.Session, len(scheduler.sessions))
	for nodeID, session := range scheduler.sessions {
		nodeIDs = append(nodeIDs, nodeID)
		sessions[nodeID] = session
	}
	scheduler.sessions = make(map[string]platform.Session)
	scheduler.mu.Unlock()
	for nodeID, session := range sessions {
		scheduler.traffic.End(nodeID, session.Generation)
	}
	sort.Strings(nodeIDs)
	failed := false
	for _, nodeID := range nodeIDs {
		node, exists := scheduler.controller.schedulerNode(nodeID)
		if !exists {
			failed = true
			continue
		}
		session := sessions[nodeID]
		request := platform.NodeRequest{Node: node, JobID: "system-shutdown", Generation: session.Generation}
		for index := len(scheduler.gates) - 1; index >= 0; index-- {
			if err := scheduler.gates[index].Close(ctx, request, session); err != nil {
				failed = true
			}
		}
		if err := scheduler.adapter.Stop(ctx, request, session); err != nil {
			failed = true
		}
	}
	if failed {
		return errors.New("scheduler shutdown cleanup failed")
	}
	return nil
}

func (scheduler *Scheduler) runNode(ctx context.Context, work scheduledNode) {
	lock := scheduler.nodeLock(work.nodeID)
	lock.Lock()
	defer lock.Unlock()

	job, node, exists := scheduler.controller.schedulerWork(work.jobID, work.nodeID)
	if job.ID == "" || isTerminalJob(job.State) {
		return
	}
	if !exists {
		scheduler.completeNodeDurably(ctx, work, "desired_removed")
		return
	}
	if node.DeletePending {
		scheduler.deleteNode(ctx, work, node)
		return
	}
	if job.Kind == "node.stop" || (job.Kind == "node.save" && !node.Enabled) {
		status, exists := scheduler.controller.schedulerStatus(work.nodeID)
		scheduler.stopNode(ctx, work, node, status, exists, false)
		return
	}

	status, exists, err := scheduler.controller.schedulerEnsureKnown(ctx, work.jobID, work.nodeID)
	if err != nil {
		return
	}
	if exists && status.State == model.StateOnline && job.Kind != "node.reconnect" && job.Kind != "node.save" {
		if job.Kind == "device.bind" || job.Kind == "device.bindings.replace" {
			status = scheduler.prepareReconnect(ctx, work, node, status)
			if status.State == model.StateQueued {
				scheduler.startNode(ctx, work, node, status)
			}
		} else if scheduler.hasSession(work.nodeID) {
			status = scheduler.prepareReconnect(ctx, work, node, status)
			if status.State == model.StateQueued {
				scheduler.startNode(ctx, work, node, status)
			}
		} else {
			scheduler.refreshNode(ctx, work, node, status)
		}
		return
	}
	if job.Kind == "device.unbind" {
		_ = scheduler.controller.schedulerCompleteNode(ctx, work.jobID, work.nodeID, "unbound")
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

func (scheduler *Scheduler) deleteNode(ctx context.Context, work scheduledNode, node model.Node) {
	status, exists := scheduler.controller.schedulerStatus(work.nodeID)
	if !exists || status.State == model.StateDisabled {
		finalized, err := scheduler.controller.schedulerFinalizeNodeDelete(ctx, work.nodeID)
		if err != nil {
			_ = scheduler.controller.schedulerFailNode(ctx, work.jobID, work.nodeID, "delete_finalize_failed")
			return
		}
		if completed := scheduler.completeNodeDurably(ctx, work, "deleted"); completed && finalized {
			_ = scheduler.controller.schedulerForgetDeletedNode(ctx, work.nodeID)
		}
		return
	}
	updated, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, EventStop, nil, "stopping")
	if err != nil {
		return
	}
	if !scheduler.closeOwnedSession(work, node, status.Generation) {
		_, _ = scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, updated.Generation, EventFailure, &model.CodeError{Code: ErrorCodeStopTimeout}, "cleanup_failed")
		return
	}
	finalized, err := scheduler.controller.schedulerFinalizeNodeDelete(ctx, work.nodeID)
	if err != nil {
		_ = scheduler.controller.schedulerFailNode(ctx, work.jobID, work.nodeID, "delete_finalize_failed")
		return
	}
	if _, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, updated.Generation, EventStopped, nil, "deleted"); err != nil {
		if completed := scheduler.completeNodeDurably(ctx, work, "desired_removed"); completed && finalized {
			_ = scheduler.controller.schedulerForgetDeletedNode(ctx, work.nodeID)
		}
		return
	}
	if finalized {
		_ = scheduler.controller.schedulerForgetDeletedNode(ctx, work.nodeID)
	}
}

func (scheduler *Scheduler) completeNodeDurably(ctx context.Context, work scheduledNode, step string) bool {
	const retryDelay = 25 * time.Millisecond

	for {
		if err := scheduler.controller.schedulerCompleteNode(ctx, work.jobID, work.nodeID, step); err == nil {
			return true
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return false
		case <-timer.C:
		}
	}
}

func (scheduler *Scheduler) waitForOrderedNode(ctx context.Context, jobID, nodeID string) bool {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, exists := scheduler.controller.jobs.Get(jobID)
		if !exists {
			return false
		}
		for _, progress := range job.Nodes {
			if progress.NodeID != nodeID {
				continue
			}
			switch nodeProgressBucket(progress) {
			case jobBucketSucceeded:
				return true
			case jobBucketFailed, jobBucketCancelled:
				return false
			}
			if progress.Step == "cleanup_failed" {
				return false
			}
			break
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (scheduler *Scheduler) refreshNode(ctx context.Context, work scheduledNode, node model.Node, status NodeStatus) {
	request := platform.NodeRequest{Node: node, JobID: work.jobID, Generation: status.Generation}
	refreshCtx, cancel := context.WithTimeout(ctx, scheduler.config.ConnectTimeout)
	defer cancel()
	if !scheduler.wanUsable() {
		scheduler.failStart(ctx, work, status, &model.CodeError{Code: ErrorCodeWANDown}, ErrorCodeWANDown)
		return
	}
	session, err := scheduler.adapter.Start(refreshCtx, request)
	if err != nil {
		scheduler.cleanupFailedStart(request, session, nil)
		scheduler.failStart(ctx, work, status, err, ErrorCodeDataplaneFailed)
		return
	}
	if err := scheduler.adapter.Probe(refreshCtx, request, session); err != nil {
		scheduler.cleanupFailedStart(request, session, nil)
		scheduler.failStart(ctx, work, status, err, ErrorCodeProbeFailed)
		return
	}
	opened := make([]platform.SessionGate, 0, len(scheduler.gates))
	for _, gate := range scheduler.gates {
		if err := gate.Open(refreshCtx, request, session); err != nil {
			scheduler.cleanupFailedStart(request, session, opened)
			scheduler.failStart(ctx, work, status, err, ErrorCodeDataplaneFailed)
			return
		}
		opened = append(opened, gate)
	}
	scheduler.putSession(work.nodeID, session)
	_ = scheduler.controller.schedulerRecordStatus(ctx, work.jobID, status, "online")
}

func (scheduler *Scheduler) prepareReconnect(ctx context.Context, work scheduledNode, node model.Node, status NodeStatus) NodeStatus {
	updated, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, EventManualReconnect, nil, "stopping")
	if err != nil {
		return status
	}
	if updated.State == model.StateQueued {
		return updated
	}
	return scheduler.cleanupBarrier(ctx, work, node, updated, status.Generation)
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
	_ = scheduler.cleanupBarrier(ctx, work, node, updated, status.Generation)
}

func (scheduler *Scheduler) cleanupBarrier(ctx context.Context, work scheduledNode, node model.Node, status NodeStatus, ownershipGeneration uint64) NodeStatus {
	if !scheduler.closeOwnedSession(work, node, ownershipGeneration) {
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

func (scheduler *Scheduler) closeOwnedSession(work scheduledNode, node model.Node, ownershipGeneration uint64) bool {
	request := platform.NodeRequest{Node: node, JobID: work.jobID, Generation: ownershipGeneration}
	session := scheduler.takeSession(work.nodeID)
	stopCtx, cancel := context.WithTimeout(context.Background(), scheduler.config.StopTimeout)
	defer cancel()
	succeeded := true
	for index := len(scheduler.gates) - 1; index >= 0; index-- {
		if err := scheduler.gates[index].Close(stopCtx, request, session); err != nil {
			succeeded = false
		}
	}
	if err := scheduler.adapter.Stop(stopCtx, request, session); err != nil {
		succeeded = false
	}
	return succeeded
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
	if !scheduler.wanUsable() {
		scheduler.failStart(ctx, work, starting, &model.CodeError{Code: ErrorCodeWANDown}, ErrorCodeWANDown)
		return
	}
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
	var coded *model.CodeError
	if errors.As(cause, &coded) && coded.Code != "" {
		code = coded.Code
	}
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		kind = EventTimeout
		code = ErrorCodeConnectTimeout
	}
	failed, err := scheduler.controller.schedulerApply(ctx, work.jobID, work.nodeID, status.Generation, kind, &model.CodeError{Code: code}, "failed")
	if err == nil && failed.State == model.StateBackoff {
		retryWork, handoffErr := scheduler.controller.schedulerHandoffRetry(ctx, work, failed)
		if handoffErr != nil {
			retryWork = work
		}
		if failed.LastError == nil || failed.LastError.Code != ErrorCodeWANDown || !failed.RetryAt.IsZero() {
			scheduler.scheduleRetry(ctx, retryWork, failed)
		}
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
	scheduler.sessions[nodeID] = session
	scheduler.mu.Unlock()
	scheduler.traffic.Begin(nodeID, session.Generation, session.Interface)
}

func (scheduler *Scheduler) takeSession(nodeID string) platform.Session {
	scheduler.mu.Lock()
	session := scheduler.sessions[nodeID]
	delete(scheduler.sessions, nodeID)
	scheduler.mu.Unlock()
	scheduler.traffic.End(nodeID, session.Generation)
	return session
}

func (scheduler *Scheduler) currentSession(nodeID string, expected platform.Session) bool {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	current, exists := scheduler.sessions[nodeID]
	return exists && current == expected
}

func (scheduler *Scheduler) hasSession(nodeID string) bool {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	_, exists := scheduler.sessions[nodeID]
	return exists
}

func (scheduler *Scheduler) wanUsable() bool {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.wanKnown && scheduler.wanUp
}

func (scheduler *Scheduler) refreshWAN(ctx context.Context, source platform.WANStatusSource) {
	if source == nil || ctx.Err() != nil {
		return
	}
	probeTimeout := scheduler.config.WANCheckInterval
	if probeTimeout > 5*time.Second {
		probeTimeout = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	available, err := source.Available(probeCtx)
	cancel()
	if err != nil {
		available = false
	}
	scheduler.mu.Lock()
	wasUsable := scheduler.wanKnown && scheduler.wanUp
	scheduler.wanKnown = true
	scheduler.wanUp = available
	scheduler.mu.Unlock()
	if available && !wasUsable {
		scheduler.wakeWANRecoveries(ctx)
	}
}

type wanRecoveryTarget struct {
	work   scheduledNode
	status NodeStatus
	node   model.Node
}

func (scheduler *Scheduler) wakeWANRecoveries(ctx context.Context) {
	for _, target := range scheduler.controller.schedulerWANRecoveries() {
		scheduler.workers.Add(1)
		go func(target wanRecoveryTarget) {
			defer scheduler.workers.Done()
			lock := scheduler.nodeLock(target.work.nodeID)
			lock.Lock()
			defer lock.Unlock()
			current, exists := scheduler.controller.schedulerStatus(target.work.nodeID)
			if !exists || current.Generation != target.status.Generation || current.State != model.StateBackoff ||
				current.LastError == nil || current.LastError.Code != ErrorCodeWANDown || !scheduler.wanUsable() {
				return
			}
			queued, err := scheduler.controller.schedulerApply(ctx, target.work.jobID, target.work.nodeID,
				current.Generation, EventWANAvailable, nil, "queued")
			if err != nil {
				return
			}
			scheduler.startNode(ctx, target.work, target.node, queued)
		}(target)
	}
}

func (scheduler *Scheduler) checkSessions(ctx context.Context) {
	select {
	case scheduler.healthRun <- struct{}{}:
	default:
		return
	}
	scheduler.workers.Add(1)
	go scheduler.runHealthSweep(ctx)
}

func (scheduler *Scheduler) runHealthSweep(ctx context.Context) {
	defer scheduler.workers.Done()
	defer func() { <-scheduler.healthRun }()
	scheduler.mu.Lock()
	sessions := make(map[string]platform.Session, len(scheduler.sessions))
	nodeIDs := make([]string, 0, len(scheduler.sessions))
	for nodeID, session := range scheduler.sessions {
		sessions[nodeID] = session
		nodeIDs = append(nodeIDs, nodeID)
	}
	scheduler.mu.Unlock()
	sort.Strings(nodeIDs)
	var checks sync.WaitGroup
	for _, nodeID := range nodeIDs {
		session := sessions[nodeID]
		select {
		case scheduler.health <- struct{}{}:
		case <-ctx.Done():
			checks.Wait()
			return
		}
		lock := scheduler.nodeLock(nodeID)
		if !lock.TryLock() {
			<-scheduler.health
			continue
		}
		checks.Add(1)
		go func(nodeID string, session platform.Session, lock *sync.Mutex) {
			defer checks.Done()
			defer func() { <-scheduler.health }()
			defer lock.Unlock()
			scheduler.checkSession(ctx, nodeID, session)
		}(nodeID, session, lock)
	}
	checks.Wait()
}

func (scheduler *Scheduler) checkSession(ctx context.Context, nodeID string, session platform.Session) {
	if ctx.Err() != nil || !scheduler.wanUsable() || !scheduler.currentSession(nodeID, session) {
		return
	}
	node, exists := scheduler.controller.schedulerNode(nodeID)
	status, statusExists := scheduler.controller.schedulerStatus(nodeID)
	if !exists || !statusExists || !node.Enabled || status.State != model.StateOnline || status.Generation != session.Generation {
		return
	}
	request := platform.NodeRequest{Node: node, JobID: status.JobID, Generation: session.Generation}
	probeCtx, cancel := context.WithTimeout(ctx, scheduler.config.ConnectTimeout)
	err := scheduler.adapter.Probe(probeCtx, request, session)
	if err == nil {
		for _, gate := range scheduler.gates {
			verifier, ok := gate.(platform.SessionGateVerifier)
			if !ok {
				continue
			}
			if err = verifier.Verify(probeCtx, request, session); err != nil {
				break
			}
		}
	}
	cancel()
	if err == nil {
		if status.Attempts > 0 {
			_, _ = scheduler.controller.schedulerMarkStable(ctx, status)
		}
		return
	}
	if ctx.Err() != nil || !scheduler.currentSession(nodeID, session) {
		return
	}
	work, recovering, err := scheduler.controller.schedulerBeginRecovery(ctx, nodeID, &model.CodeError{Code: ErrorCodeProbeFailed})
	if err != nil {
		return
	}
	if !scheduler.closeOwnedSession(work, node, session.Generation) {
		_, _ = scheduler.controller.schedulerApply(ctx, work.jobID, nodeID, recovering.Generation, EventFailure,
			&model.CodeError{Code: ErrorCodeStopTimeout}, "cleanup_failed")
		return
	}
	queued, err := scheduler.controller.schedulerApply(ctx, work.jobID, nodeID, recovering.Generation, EventRecovered, nil, "queued")
	if err != nil {
		return
	}
	scheduler.startNode(ctx, work, node, queued)
}

func (scheduler *Scheduler) Traffic(nodeID string) TrafficSnapshot {
	if scheduler == nil {
		return TrafficSnapshot{}
	}
	return scheduler.traffic.Snapshot(nodeID)
}

func (scheduler *Scheduler) sampleTraffic(sampledAt time.Time) {
	scheduler.mu.Lock()
	sessions := make(map[string]platform.Session, len(scheduler.sessions))
	for nodeID, session := range scheduler.sessions {
		sessions[nodeID] = session
	}
	scheduler.mu.Unlock()
	for nodeID, session := range sessions {
		if session.Interface == "" {
			scheduler.traffic.Unavailable(nodeID, session.Generation, sampledAt)
			continue
		}
		counters, err := scheduler.trafficReader.ReadInterfaceCounters(session.Interface)
		if err != nil {
			scheduler.traffic.Unavailable(nodeID, session.Generation, sampledAt)
			continue
		}
		scheduler.traffic.Sample(nodeID, session.Generation, counters, sampledAt)
	}
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

func (controller *Controller) schedulerNode(nodeID string) (model.Node, bool) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	node, exists := controller.desired.Nodes[nodeID]
	return node, exists
}

func (controller *Controller) schedulerBeginRecovery(ctx context.Context, nodeID string, failure *model.CodeError) (scheduledNode, NodeStatus, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	node, exists := controller.desired.Nodes[nodeID]
	status, statusExists := controller.machine.Status(nodeID)
	if !exists || !statusExists || !controller.desired.Global.Enabled || !node.Enabled || status.State != model.StateOnline {
		return scheduledNode{}, NodeStatus{}, errors.New("scheduler recovery target is unavailable")
	}
	for _, active := range controller.jobs.List() {
		if isTerminalJob(active.State) {
			continue
		}
		for _, progress := range active.Nodes {
			if progress.NodeID == nodeID {
				return scheduledNode{}, NodeStatus{}, errors.New("scheduler recovery is already queued")
			}
		}
	}
	jobID := controller.uniqueRecoveryJobIDLocked(status.JobID, nodeID, status.Generation)
	job := newControllerJob(jobID, "system.recover", controller.now(), controller.desired.Revision, []string{nodeID})
	beforeJobs := controller.jobs.Snapshot()
	if err := controller.jobs.Put(job); err != nil {
		return scheduledNode{}, NodeStatus{}, err
	}
	recovering, err := controller.schedulerApplyLocked(ctx, Event{
		NodeID: nodeID, JobID: jobID, Generation: status.Generation, Kind: EventRecover, Err: failure, At: controller.now(),
	}, "recovering")
	if err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		return scheduledNode{}, NodeStatus{}, err
	}
	return scheduledNode{jobID: jobID, nodeID: nodeID}, recovering, nil
}

func (controller *Controller) schedulerWANRecoveries() []wanRecoveryTarget {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	targets := make([]wanRecoveryTarget, 0)
	for nodeID, status := range controller.statuses {
		if status.State != model.StateBackoff || status.LastError == nil || status.LastError.Code != ErrorCodeWANDown {
			continue
		}
		node, exists := controller.desired.Nodes[nodeID]
		if !exists || !controller.desired.Global.Enabled || !node.Enabled {
			continue
		}
		var work scheduledNode
		for _, job := range controller.jobs.List() {
			if isTerminalJob(job.State) {
				continue
			}
			for _, progress := range job.Nodes {
				if progress.NodeID == nodeID {
					work = scheduledNode{jobID: job.ID, nodeID: nodeID}
					break
				}
			}
			if work.jobID != "" {
				break
			}
		}
		if work.jobID != "" {
			targets = append(targets, wanRecoveryTarget{work: work, status: cloneNodeStatus(status), node: node})
		}
	}
	return targets
}

func (controller *Controller) schedulerHandoffRetry(ctx context.Context, work scheduledNode, status NodeStatus) (scheduledNode, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	job, exists := controller.jobs.Get(work.jobID)
	if !exists || isTerminalJob(job.State) {
		return scheduledNode{}, errors.New("scheduler retry job is unavailable")
	}
	if job.Kind == "system.recover" {
		return work, nil
	}
	beforeJobs := controller.jobs.Snapshot()
	attemptOutcome := cloneNodeStatus(status)
	attemptOutcome.State = model.StateFailed
	if err := controller.updateJobForStatus(work.jobID, attemptOutcome, "retry_scheduled"); err != nil {
		return scheduledNode{}, err
	}
	recoveryID := controller.uniqueRecoveryJobIDLocked(work.jobID, work.nodeID, status.Generation)
	recovery := newControllerJob(recoveryID, "system.recover", controller.now(), controller.desired.Revision, []string{work.nodeID})
	if err := controller.jobs.Put(recovery); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		return scheduledNode{}, err
	}
	if err := controller.updateJobForStatus(recoveryID, status, "backoff"); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		return scheduledNode{}, err
	}
	if _, err := controller.jobs.AppendEvent(NodeEvent{
		JobID: recoveryID, NodeID: work.nodeID, Generation: status.Generation, State: status.State,
		Attempt: status.Attempts, At: controller.now(), Error: status.LastError,
	}); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		return scheduledNode{}, err
	}
	if err := controller.persistLocked(ctx); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		return scheduledNode{}, err
	}
	return scheduledNode{jobID: recoveryID, nodeID: work.nodeID}, nil
}

func (controller *Controller) schedulerMarkStable(ctx context.Context, status NodeStatus) (NodeStatus, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	beforeMachine, machineExists := controller.machine.Status(status.NodeID)
	beforeStatus, statusExists := controller.statuses[status.NodeID]
	beforeJobs := controller.jobs.Snapshot()
	stable, err := controller.machine.Apply(Event{
		NodeID: status.NodeID, JobID: status.JobID, Generation: status.Generation, Kind: EventStableOnline, At: controller.now(),
	})
	if err != nil {
		return NodeStatus{}, err
	}
	controller.statuses[status.NodeID] = cloneNodeStatus(stable)
	if _, err := controller.jobs.AppendEvent(NodeEvent{
		JobID: status.JobID, NodeID: status.NodeID, Generation: stable.Generation, State: stable.State,
		Attempt: stable.Attempts, At: controller.now(), Error: stable.LastError,
	}); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		controller.machine.restoreNode(status.NodeID, beforeMachine, machineExists)
		if statusExists {
			controller.statuses[status.NodeID] = beforeStatus
		} else {
			delete(controller.statuses, status.NodeID)
		}
		return NodeStatus{}, err
	}
	if err := controller.persistLocked(ctx); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		controller.machine.restoreNode(status.NodeID, beforeMachine, machineExists)
		if statusExists {
			controller.statuses[status.NodeID] = beforeStatus
		} else {
			delete(controller.statuses, status.NodeID)
		}
		return NodeStatus{}, err
	}
	return stable, nil
}

func (controller *Controller) uniqueRecoveryJobIDLocked(previousJobID, nodeID string, generation uint64) string {
	candidate := controller.newJobID()
	if candidate != "" {
		if _, exists := controller.jobs.Get(candidate); !exists {
			return candidate
		}
	}
	base := fmt.Sprintf("%s-recovery-%s-%d", previousJobID, nodeID, generation)
	for suffix := 0; ; suffix++ {
		candidate = base
		if suffix > 0 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		if _, exists := controller.jobs.Get(candidate); !exists {
			return candidate
		}
	}
}

func (controller *Controller) schedulerFinalizeNodeDelete(ctx context.Context, nodeID string) (bool, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	current, err := controller.desiredStore.Load()
	if err != nil {
		return false, errors.New("delete finalization configuration load failed")
	}
	node, exists := current.Nodes[nodeID]
	if !exists {
		return true, nil
	}
	if !node.DeletePending {
		return false, errors.New("delete finalization tombstone is missing")
	}
	next := cloneControllerConfig(current)
	delete(next.Nodes, nodeID)
	if err := model.Validate(next); err != nil {
		return false, errors.New("delete finalization configuration is invalid")
	}
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		observed, observeErr := controller.desiredStore.Load()
		if observeErr != nil || !controllerConfigMatchesStoredMutation(current, next, observed) {
			return false, errors.New("delete finalization persistence failed")
		}
		durableCtx, cancel := context.WithTimeout(context.Background(), controller.leaseRollbackTimeout)
		durableErr := controller.desiredStore.EnsureDurable(durableCtx)
		cancel()
		if durableErr != nil {
			return false, errors.New("delete finalization durability failed")
		}
		stored = observed
	}
	controller.desired = cloneControllerConfig(stored)
	return true, nil
}

func (controller *Controller) schedulerFailNode(ctx context.Context, jobID, nodeID, step string) error {
	return controller.schedulerRecordStatus(ctx, jobID, NodeStatus{
		NodeID: nodeID, State: model.StateFailed,
		LastError: publicErrorFromCode(&model.CodeError{Code: ErrorCodeInternal}),
	}, step)
}

func (controller *Controller) schedulerForgetDeletedNode(ctx context.Context, nodeID string) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	delete(controller.statuses, nodeID)
	controller.machine.restoreNode(nodeID, NodeStatus{}, false)
	if err := controller.persistLocked(ctx); err != nil {
		return errors.New("delete status cleanup persistence failed")
	}
	return nil
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

func (controller *Controller) schedulerFailFollowingNodes(ctx context.Context, jobID, completedNodeID string) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	job, exists := controller.jobs.Get(jobID)
	if !exists || isTerminalJob(job.State) {
		return nil
	}
	before := controller.jobs.Snapshot()
	found := false
	for _, progress := range job.Nodes {
		if !found {
			found = progress.NodeID == completedNodeID
			continue
		}
		bucket := nodeProgressBucket(progress)
		if bucket == jobBucketSucceeded || bucket == jobBucketFailed || bucket == jobBucketCancelled {
			continue
		}
		status := NodeStatus{
			NodeID:    progress.NodeID,
			State:     model.StateFailed,
			Attempts:  progress.Attempt,
			LastError: &PublicError{Code: ErrorCodeDataplaneFailed, Message: "blocked by an earlier node operation"},
		}
		if err := controller.updateJobForStatus(jobID, status, "blocked_by_previous_node"); err != nil {
			_ = controller.jobs.Restore(before)
			return err
		}
	}
	if !found {
		_ = controller.jobs.Restore(before)
		return errors.New("scheduler ordered job node is missing")
	}
	if err := controller.persistLocked(ctx); err != nil {
		_ = controller.jobs.Restore(before)
		return err
	}
	return nil
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
