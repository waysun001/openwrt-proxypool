package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/diagnostics"
	"proxypoold/internal/importer"
	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

type desiredConfigStore interface {
	Load() (model.DesiredConfig, error)
	Replace(context.Context, uint64, model.DesiredConfig) (model.DesiredConfig, error)
	EnsureDurable(context.Context) error
}

type runtimePersistence interface {
	Load() (RuntimeSnapshot, error)
	Save(context.Context, RuntimeSnapshot) error
}

type schedulerSubmitter interface {
	Submit(Job)
}

type diagnosticsService interface {
	Create() (diagnostics.DiagnosticStatus, error)
	Get(string) (diagnostics.DiagnosticStatus, bool)
	Claim(string) (diagnostics.ArtifactClaim, error)
	Release(string) error
}

type ControllerOption func(*Controller)

const defaultControllerLeaseRollbackTimeout = 10 * time.Second

func WithControllerClock(clock func() time.Time) ControllerOption {
	return func(controller *Controller) {
		if clock != nil {
			controller.now = clock
		}
	}
}

func WithControllerJobIDSource(source func() string) ControllerOption {
	return func(controller *Controller) {
		if source != nil {
			controller.newJobID = source
		}
	}
}

func WithControllerNodeIDSource(source func() string) ControllerOption {
	return func(controller *Controller) {
		if source != nil {
			controller.newNodeID = source
		}
	}
}

func WithDeviceServices(source platform.DeviceSource, leases platform.LeaseManager) ControllerOption {
	return func(controller *Controller) {
		controller.deviceSource = source
		controller.leaseManager = leases
	}
}

func WithControllerLeaseRollbackTimeout(timeout time.Duration) ControllerOption {
	return func(controller *Controller) {
		if timeout > 0 {
			controller.leaseRollbackTimeout = timeout
		}
	}
}

func WithImporter(manager *importer.Manager) ControllerOption {
	return func(controller *Controller) {
		if manager != nil {
			controller.importer = manager
		}
	}
}

func WithDiagnostics(service diagnosticsService) ControllerOption {
	return func(controller *Controller) { controller.diagnostics = service }
}

// Controller is the formal V2 single writer. Platform work is deliberately
// only queued here; Scheduler becomes the sole side-effect owner in Task 4.
type Controller struct {
	mu sync.Mutex

	desiredStore desiredConfigStore
	runtimeStore runtimePersistence
	machine      *Machine
	jobs         *JobStore
	deviceSource platform.DeviceSource
	leaseManager platform.LeaseManager
	scheduler    schedulerSubmitter
	importer     *importer.Manager
	diagnostics  diagnosticsService

	desired              model.DesiredConfig
	statuses             map[string]NodeStatus
	idempotency          map[string]IdempotencyRecord
	idempotencyOrder     []string
	now                  func() time.Time
	newJobID             func() string
	newNodeID            func() string
	leaseRollbackTimeout time.Duration
}

func NewController(desiredStore desiredConfigStore, runtimeStore runtimePersistence, machine *Machine, jobs *JobStore, options ...ControllerOption) (*Controller, error) {
	if desiredStore == nil || runtimeStore == nil {
		return nil, errors.New("live controller dependencies are missing")
	}
	if machine == nil {
		machine = NewMachine(nil)
	}
	if jobs == nil {
		jobs = NewJobStore()
	}
	controller := &Controller{
		desiredStore:         desiredStore,
		runtimeStore:         runtimeStore,
		machine:              machine,
		jobs:                 jobs,
		statuses:             make(map[string]NodeStatus),
		idempotency:          make(map[string]IdempotencyRecord),
		now:                  time.Now,
		newJobID:             randomReconciliationID,
		newNodeID:            randomControllerNodeID,
		leaseRollbackTimeout: defaultControllerLeaseRollbackTimeout,
		importer:             importer.New(),
	}
	for _, option := range options {
		if option != nil {
			option(controller)
		}
	}

	desired, err := desiredStore.Load()
	if err != nil {
		return nil, errors.New("live desired configuration load failed")
	}
	controller.desired = cloneControllerConfig(desired)
	snapshot, err := runtimeStore.Load()
	if errors.Is(err, ErrRuntimeSnapshotNotFound) {
		snapshot = RuntimeSnapshot{SchemaVersion: RuntimeSnapshotSchemaVersion, ConfigRevision: desired.Revision, Jobs: jobs.Snapshot()}
		if err := runtimeStore.Save(context.Background(), snapshot); err != nil {
			return nil, errors.New("live runtime initialization failed")
		}
	} else if err != nil {
		return nil, errors.New("live runtime load failed")
	}
	if snapshot.ConfigRevision > desired.Revision {
		// Desired state is authoritative. A prior ambiguous desired-directory
		// sync may leave observational runtime metadata one revision ahead after
		// a crash. Discard it and reconcile from the durable desired revision.
		snapshot = RuntimeSnapshot{
			SchemaVersion:  RuntimeSnapshotSchemaVersion,
			ConfigRevision: desired.Revision,
			Jobs:           NewJobStore().Snapshot(),
		}
		if err := runtimeStore.Save(context.Background(), snapshot); err != nil {
			return nil, errors.New("live runtime rollback reconciliation failed")
		}
	}
	if err := jobs.Restore(snapshot.Jobs); err != nil {
		return nil, errors.New("live runtime jobs are invalid")
	}
	for _, status := range snapshot.NodeStatuses {
		controller.statuses[status.NodeID] = cloneNodeStatus(status)
		controller.machine.restoreNode(status.NodeID, status, true)
	}
	for _, record := range snapshot.Idempotency {
		controller.idempotency[record.RequestID] = cloneIdempotencyRecord(record)
		controller.idempotencyOrder = append(controller.idempotencyOrder, record.RequestID)
	}
	if snapshot.ConfigRevision < desired.Revision {
		// A desired write may have reached disk immediately before a crash or
		// runtime persistence failure. Only a delete tombstone may retain its
		// prior generation: the adapter needs that ownership evidence to remove
		// the old session. Every other observational state is discarded.
		for _, job := range controller.jobs.List() {
			if isTerminalJob(job.State) {
				continue
			}
			for _, progress := range job.Nodes {
				if _, exists := desired.Nodes[progress.NodeID]; exists {
					continue
				}
				if err := controller.updateJobForStatus(job.ID, NodeStatus{NodeID: progress.NodeID, State: model.StateDisabled}, "desired_removed"); err != nil {
					return nil, errors.New("live runtime finalized-delete reconciliation failed")
				}
			}
		}
		for nodeID := range controller.statuses {
			node, exists := desired.Nodes[nodeID]
			if exists && node.DeletePending {
				continue
			}
			delete(controller.statuses, nodeID)
			controller.machine.restoreNode(nodeID, NodeStatus{}, false)
		}
		if err := controller.persistLocked(context.Background()); err != nil {
			return nil, errors.New("live runtime reconciliation persistence failed")
		}
	}
	return controller, nil
}

// AttachScheduler connects the durable control plane to its sole side-effect
// owner. Queued work remains durable even when the scheduler is not running.
func (controller *Controller) AttachScheduler(scheduler schedulerSubmitter) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.scheduler = scheduler
}

// ReconcileStartup durably queues every enabled node that is not already
// covered by unfinished work. Restored "online" state is only an observation;
// the scheduler must re-probe the owned session and republish its expiring
// dataplane leases before clients can use it again.
func (controller *Controller) ReconcileStartup(ctx context.Context) error {
	if controller == nil || contextDone(ctx) != nil {
		return errors.New("startup reconciliation is unavailable")
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	covered := make(map[string]struct{})
	for _, job := range controller.jobs.List() {
		if isTerminalJob(job.State) {
			continue
		}
		for _, progress := range job.Nodes {
			covered[progress.NodeID] = struct{}{}
		}
	}
	nodeIDs := make([]string, 0, len(controller.desired.Nodes))
	for nodeID, node := range controller.desired.Nodes {
		if !node.DeletePending && (!controller.desired.Global.Enabled || !node.Enabled) {
			continue
		}
		if _, exists := covered[nodeID]; !exists {
			nodeIDs = append(nodeIDs, nodeID)
		}
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	sort.Strings(nodeIDs)
	job := newControllerJob(controller.newJobID(), "system.recover", controller.now(), controller.desired.Revision, nodeIDs)
	beforeJobs := controller.jobs.Snapshot()
	if err := controller.jobs.Put(job); err != nil {
		return err
	}
	if err := controller.persistLocked(ctx); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		return err
	}
	if controller.scheduler != nil {
		controller.scheduler.Submit(job)
	}
	return nil
}

// LearnPendingBindings converts legacy IP-only bindings after DHCP confirms
// the corresponding MAC. Pending entries never authorize traffic, and the
// whole discovered batch is published in one desired-config replacement.
func (controller *Controller) LearnPendingBindings(ctx context.Context) error {
	if controller == nil || controller.deviceSource == nil || contextDone(ctx) != nil {
		return errors.New("pending binding learning is unavailable")
	}
	discovered, err := controller.deviceSource.List(ctx)
	if err != nil {
		return errors.New("pending binding discovery failed")
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	current, err := controller.desiredStore.Load()
	if err != nil {
		return errors.New("pending binding configuration load failed")
	}
	if len(current.PendingBindings) == 0 {
		return nil
	}
	next := cloneControllerConfig(current)
	byAddress := make(map[string][]platform.DiscoveredDevice)
	for _, device := range discovered {
		if device.IPv4.Is4() && device.Confirmed {
			key := device.IPv4.Unmap().String()
			byAddress[key] = append(byAddress[key], device)
		}
	}
	usedIDs := make(map[string]struct{}, len(next.Devices))
	usedMACs := make(map[string]struct{}, len(next.Devices))
	usedAddresses := make(map[string]struct{}, len(next.Devices))
	for id, device := range next.Devices {
		usedIDs[id] = struct{}{}
		usedMACs[strings.ToLower(device.MAC)] = struct{}{}
		usedAddresses[device.FixedIPv4.String()] = struct{}{}
	}
	pendingIDs := make([]string, 0, len(next.PendingBindings))
	for id := range next.PendingBindings {
		pendingIDs = append(pendingIDs, id)
	}
	sort.Strings(pendingIDs)
	applied := make([]model.Device, 0)
	nodeSet := make(map[string]struct{})
	changed := false
	for _, pendingID := range pendingIDs {
		pending := next.PendingBindings[pendingID]
		matches := byAddress[pending.LegacyIPv4.String()]
		if len(matches) == 0 {
			_, addressConflict := usedAddresses[pending.LegacyIPv4.String()]
			wantError := ""
			if addressConflict {
				wantError = "duplicate"
			}
			if pending.ErrorCode != wantError {
				pending.ErrorCode = wantError
				next.PendingBindings[pendingID] = pending
				changed = true
			}
			continue
		}
		if len(matches) != 1 {
			if pending.ErrorCode != "duplicate" {
				pending.ErrorCode = "duplicate"
				next.PendingBindings[pendingID] = pending
				changed = true
			}
			continue
		}
		candidate, ok := pendingLearnedDevice(matches[0], pending)
		_, idConflict := usedIDs[candidate.ID]
		_, macConflict := usedMACs[candidate.MAC]
		_, addressConflict := usedAddresses[candidate.FixedIPv4.String()]
		if !ok || idConflict || macConflict || addressConflict {
			if pending.ErrorCode != "duplicate" {
				pending.ErrorCode = "duplicate"
				next.PendingBindings[pendingID] = pending
				changed = true
			}
			continue
		}
		next.Devices[candidate.ID] = candidate
		delete(next.PendingBindings, pendingID)
		usedIDs[candidate.ID] = struct{}{}
		usedMACs[candidate.MAC] = struct{}{}
		usedAddresses[candidate.FixedIPv4.String()] = struct{}{}
		applied = append(applied, candidate)
		nodeSet[candidate.NodeID] = struct{}{}
		changed = true
	}
	if !changed {
		return nil
	}
	if err := model.Validate(next); err != nil {
		return errors.New("pending binding candidate is invalid")
	}
	if len(applied) > 0 && controller.leaseManager == nil {
		return errors.New("pending binding lease manager is unavailable")
	}
	for _, device := range applied {
		if err := controller.leaseManager.Apply(ctx, device, current.Revision+1); err != nil {
			controller.rollbackPendingLeases(applied[:lenAppliedBefore(applied, device)], current.Revision+1)
			return errors.New("pending binding lease apply failed")
		}
	}
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		controller.rollbackPendingLeases(applied, current.Revision+1)
		return errors.New("pending binding configuration persistence failed")
	}
	controller.desired = cloneControllerConfig(stored)
	if len(nodeSet) == 0 {
		if err := controller.persistLocked(ctx); err != nil {
			return errors.New("pending binding runtime persistence failed")
		}
		return nil
	}
	nodeIDs := make([]string, 0, len(nodeSet))
	for nodeID := range nodeSet {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	job := newControllerJob(controller.newJobID(), "pending.learn", controller.now(), stored.Revision, nodeIDs)
	if err := controller.jobs.Put(job); err != nil {
		return errors.New("pending binding job creation failed")
	}
	if err := controller.persistLocked(ctx); err != nil {
		if controller.scheduler != nil && !isTerminalJob(job.State) {
			controller.scheduler.Submit(job)
		}
		return errors.New("pending binding runtime persistence failed")
	}
	if controller.scheduler != nil && !isTerminalJob(job.State) {
		controller.scheduler.Submit(job)
	}
	return nil
}

func pendingLearnedDevice(discovered platform.DiscoveredDevice, pending model.PendingBinding) (model.Device, bool) {
	parsed, err := net.ParseMAC(discovered.MAC)
	if err != nil || len(parsed) != 6 || !discovered.IPv4.Is4() || discovered.IPv4.Unmap() != pending.LegacyIPv4.Unmap() {
		return model.Device{}, false
	}
	mac := strings.ToLower(parsed.String())
	id := "device_" + strings.ReplaceAll(mac, ":", "")
	if discovered.ID != id {
		return model.Device{}, false
	}
	return model.Device{
		ID: id, MAC: mac, Hostname: discovered.Hostname, FixedIPv4: pending.LegacyIPv4.Unmap(),
		NodeID: pending.NodeID, Enabled: true,
	}, true
}

func lenAppliedBefore(devices []model.Device, target model.Device) int {
	for index := range devices {
		if devices[index].ID == target.ID {
			return index
		}
	}
	return len(devices)
}

func (controller *Controller) rollbackPendingLeases(devices []model.Device, revision uint64) {
	if controller.leaseManager == nil {
		return
	}
	rollbackCtx, cancel := context.WithTimeout(context.Background(), controller.leaseRollbackTimeout)
	defer cancel()
	for index := len(devices) - 1; index >= 0; index-- {
		_ = controller.leaseManager.Remove(rollbackCtx, devices[index], revision)
	}
}

func (controller *Controller) Handle(ctx context.Context, request api.Request) api.Response {
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	switch request.Method {
	case "status.get":
		return controller.handleStatus(request)
	case "device.list":
		return controller.handleDeviceList(request)
	case "device.bind":
		return controller.handleDeviceBind(ctx, request)
	case "device.unbind":
		return controller.handleDeviceUnbind(ctx, request)
	case "device.bindings.replace":
		return controller.handleDeviceBindingsReplace(ctx, request)
	case "node.save":
		return controller.handleNodeSave(ctx, request)
	case "node.delete":
		return controller.handleNodeDelete(ctx, request)
	case "node.action":
		return controller.handleNodeAction(ctx, request)
	case "import.preview":
		return controller.handleImportPreview(ctx, request)
	case "import.commit":
		return controller.handleImportCommit(ctx, request)
	case "job.get":
		return controller.handleJobGet(request)
	case "job.list":
		return controller.handleJobList(request)
	case "system.events":
		return controller.handleEvents(request)
	case "system.interface_event":
		return controller.handleInterfaceEvent(ctx, request)
	case "diagnostics.create":
		return controller.handleDiagnosticsCreate(request)
	case "diagnostics.get":
		return controller.handleDiagnosticsGet(request)
	case "diagnostics.claim":
		return controller.handleDiagnosticsClaim(request)
	case "diagnostics.release":
		return controller.handleDiagnosticsRelease(request)
	default:
		return api.Response{Version: api.ProtocolVersion, ID: request.ID, Error: &api.Error{Code: "unknown_method", Message: "unknown control method"}}
	}
}

type emptyParams struct{}

type bindParams struct {
	DeviceID         string  `json:"device_id"`
	NodeID           string  `json:"node_id"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

type unbindParams struct {
	DeviceID         string  `json:"device_id"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

type replaceDeviceBindingsParams struct {
	NodeID           string   `json:"node_id"`
	DeviceIDs        []string `json:"device_ids"`
	ExpectedRevision *uint64  `json:"expected_revision"`
}

type nodeActionParams struct {
	NodeID           string  `json:"node_id"`
	Action           string  `json:"action"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

type nodeSaveParams struct {
	NodeID           string         `json:"node_id"`
	Name             string         `json:"name"`
	Protocol         model.Protocol `json:"protocol"`
	Enabled          bool           `json:"enabled"`
	Server           string         `json:"server"`
	Port             uint16         `json:"port"`
	Username         string         `json:"username"`
	Password         string         `json:"password"`
	ExpiresAt        string         `json:"expires_at"`
	ExpectedRevision *uint64        `json:"expected_revision"`
}

type nodeDeleteParams struct {
	NodeID           string  `json:"node_id"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

type jobGetParams struct {
	JobID string `json:"job_id"`
}

type eventsParams struct {
	AfterSequence uint64 `json:"after_sequence"`
	Limit         int    `json:"limit"`
}

type interfaceEventParams struct {
	Interface string `json:"interface"`
	Action    string `json:"action"`
}

type importPreviewParams struct {
	Protocol         model.Protocol `json:"protocol"`
	Raw              string         `json:"raw"`
	ExpectedRevision *uint64        `json:"expected_revision"`
}

type importCommitParams struct {
	PreviewID        string  `json:"preview_id"`
	PreviewHash      string  `json:"preview_hash"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

type diagnosticJobParams struct {
	JobID string `json:"job_id"`
}
type diagnosticArtifactParams struct {
	ArtifactID string `json:"artifact_id"`
}

var (
	diagnosticJobParamPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	diagnosticArtifactParamPattern = regexp.MustCompile(`^diag-[a-f0-9]{16,64}$`)
)

type diagnosticClaimResult struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
}

type mutationResult struct {
	JobID          string `json:"job_id"`
	ConfigRevision uint64 `json:"config_revision"`
}

type interfaceEventResult struct {
	JobID          string `json:"job_id,omitempty"`
	ConfigRevision uint64 `json:"config_revision"`
	Ignored        bool   `json:"ignored,omitempty"`
}

func (controller *Controller) handleStatus(request api.Request) api.Response {
	var params emptyParams
	if decodeControllerParams(request.Params, &params) != nil {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	desired, runtimeSummary := summarizeDesired(controller.desired)
	for index := range runtimeSummary.Nodes {
		if status, exists := controller.statuses[runtimeSummary.Nodes[index].NodeID]; exists {
			runtimeSummary.Nodes[index].JobID = status.JobID
			runtimeSummary.Nodes[index].State = status.State
			runtimeSummary.Nodes[index].Attempts = status.Attempts
			runtimeSummary.Nodes[index].LastError = clonePublicError(status.LastError)
			if !status.RetryAt.IsZero() {
				retryAt := status.RetryAt
				runtimeSummary.Nodes[index].RetryAt = &retryAt
			}
		}
	}
	status := ShadowStatus{
		Mode:    "v2_live",
		Config:  ConfigSummary{State: ConfigStateReady, SchemaVersion: controller.desired.SchemaVersion, Revision: controller.desired.Revision},
		Desired: desired,
		Runtime: runtimeSummary,
	}
	jobs := controller.jobs.List()
	if len(jobs) > 0 {
		status.Reconciliation = summarizeReconciliation(jobs[len(jobs)-1])
	}
	return controllerResult(request.ID, status)
}

func (controller *Controller) handleDeviceList(request api.Request) api.Response {
	var params emptyParams
	if decodeControllerParams(request.Params, &params) != nil {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	var discovered []platform.DiscoveredDevice
	var err error
	if controller.deviceSource != nil {
		discovered, err = controller.deviceSource.List(context.Background())
		if err != nil {
			return controllerError(request.ID, ErrorCodeInternal)
		}
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	devices := mergeDeviceList(controller.desired, discovered)
	return controllerResult(request.ID, struct {
		ConfigRevision uint64            `json:"config_revision"`
		Devices        []DeviceListEntry `json:"devices"`
	}{controller.desired.Revision, devices})
}

func (controller *Controller) handleDeviceBind(ctx context.Context, request api.Request) api.Response {
	var params bindParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" || params.DeviceID == "" || params.NodeID == "" || params.ExpectedRevision == nil || *params.ExpectedRevision == 0 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	digest := controllerDigest(request.Method, params)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if response, handled := controller.replayLocked(request, digest); handled {
		return response
	}
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	current, err := controller.desiredStore.Load()
	if err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if current.Revision != *params.ExpectedRevision {
		return controllerError(request.ID, ErrorCodeRevisionConflict)
	}
	targetNode, exists := current.Nodes[params.NodeID]
	if !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	if !targetNode.Enabled || !current.Global.Enabled {
		return controllerError(request.ID, ErrorCodeInvalidConfig)
	}
	device, configured := current.Devices[params.DeviceID]
	oldNodeID := device.NodeID
	if controller.deviceSource != nil {
		discovered, err := controller.findDiscoveredDeviceLocked(ctx, params.DeviceID)
		if err != nil {
			return controllerModelError(request.ID, err)
		}
		if configured && device.MAC != discovered.MAC {
			return controllerError(request.ID, ErrorCodeInvalidConfig)
		}
		if !configured {
			device = model.Device{
				ID: discovered.ID, MAC: discovered.MAC, Hostname: discovered.Hostname,
				FixedIPv4: discovered.IPv4,
			}
		}
	} else if !configured {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	next := cloneControllerConfig(current)
	device.NodeID = params.NodeID
	device.Enabled = true
	next.Devices[params.DeviceID] = device
	if controller.leaseManager != nil {
		if err := controller.leaseManager.Apply(ctx, device, current.Revision+1); err != nil {
			return controllerError(request.ID, ErrorCodeInternal)
		}
	}
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		if controller.leaseManager != nil && !configured {
			_ = controller.leaseManager.Remove(context.Background(), device, current.Revision+1)
		}
		return controllerModelError(request.ID, err)
	}
	nodeIDs := []string{params.NodeID}
	if oldNodeID != "" && oldNodeID != params.NodeID {
		nodeIDs = []string{oldNodeID, params.NodeID}
	}
	return controller.finishMutationLocked(ctx, request, digest, stored, "device.bind", nodeIDs)
}

type DeviceListEntry struct {
	ID          string     `json:"id"`
	MAC         string     `json:"mac"`
	Hostname    string     `json:"hostname,omitempty"`
	CurrentIPv4 string     `json:"current_ipv4,omitempty"`
	FixedIPv4   string     `json:"fixed_ipv4,omitempty"`
	NodeID      string     `json:"node_id,omitempty"`
	Enabled     bool       `json:"enabled"`
	Ingress     string     `json:"ingress,omitempty"`
	LastSeen    *time.Time `json:"last_seen,omitempty"`
	Confirmed   bool       `json:"confirmed"`
	Configured  bool       `json:"configured"`
}

func mergeDeviceList(desired model.DesiredConfig, discovered []platform.DiscoveredDevice) []DeviceListEntry {
	entries := make(map[string]DeviceListEntry, len(desired.Devices)+len(discovered))
	for id, device := range desired.Devices {
		entries[id] = DeviceListEntry{
			ID: id, MAC: device.MAC, Hostname: device.Hostname, FixedIPv4: device.FixedIPv4.String(),
			NodeID: device.NodeID, Enabled: device.Enabled, Configured: true,
		}
	}
	for _, device := range discovered {
		entry := entries[device.ID]
		entry.ID, entry.MAC, entry.Hostname = device.ID, device.MAC, device.Hostname
		entry.CurrentIPv4, entry.Ingress, entry.Confirmed = device.IPv4.String(), device.Ingress, device.Confirmed
		lastSeen := device.LastSeen
		entry.LastSeen = &lastSeen
		entries[device.ID] = entry
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]DeviceListEntry, 0, len(ids))
	for _, id := range ids {
		result = append(result, entries[id])
	}
	return result
}

func (controller *Controller) findDiscoveredDeviceLocked(ctx context.Context, id string) (platform.DiscoveredDevice, error) {
	devices, err := controller.deviceSource.List(ctx)
	if err != nil {
		return platform.DiscoveredDevice{}, errors.New("device discovery failed")
	}
	for _, device := range devices {
		if device.ID == id {
			if !device.Confirmed || !device.IPv4.Is4() || device.MAC == "" {
				return platform.DiscoveredDevice{}, codeError(ErrorCodeInvalidConfig, "discovered device is not confirmed")
			}
			return device, nil
		}
	}
	return platform.DiscoveredDevice{}, codeError(ErrorCodeNotFound, "discovered device was not found")
}

func (controller *Controller) handleDeviceUnbind(ctx context.Context, request api.Request) api.Response {
	var params unbindParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" || params.DeviceID == "" || params.ExpectedRevision == nil || *params.ExpectedRevision == 0 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	digest := controllerDigest(request.Method, params)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if response, handled := controller.replayLocked(request, digest); handled {
		return response
	}
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	current, err := controller.desiredStore.Load()
	if err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if current.Revision != *params.ExpectedRevision {
		return controllerError(request.ID, ErrorCodeRevisionConflict)
	}
	device, exists := current.Devices[params.DeviceID]
	if !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	oldNodeID := device.NodeID
	next := cloneControllerConfig(current)
	device.NodeID = ""
	device.Enabled = false
	next.Devices[params.DeviceID] = device
	if controller.leaseManager != nil {
		if err := controller.leaseManager.Remove(ctx, current.Devices[params.DeviceID], current.Revision+1); err != nil {
			return controllerError(request.ID, ErrorCodeInternal)
		}
	}
	nodeIDs := []string(nil)
	if oldNodeID != "" {
		nodeIDs = []string{oldNodeID}
	}
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		observed, observeErr := controller.desiredStore.Load()
		if observeErr == nil && controllerConfigMatchesStoredMutation(current, next, observed) {
			return controller.finishObservedMutationLocked(request, digest, observed, "device.unbind", nodeIDs)
		}
		if observeErr == nil && !controllerConfigsEqual(observed, current) {
			return controllerError(request.ID, ErrorCodeInternal)
		}
		if controller.leaseManager != nil {
			rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), controller.leaseRollbackTimeout)
			rollbackErr := controller.leaseManager.Apply(rollbackCtx, current.Devices[params.DeviceID], current.Revision+1)
			cancelRollback()
			if rollbackErr != nil {
				repairCtx, cancelRepair := context.WithTimeout(context.Background(), controller.leaseRollbackTimeout)
				repaired, repairErr := controller.desiredStore.Replace(repairCtx, current.Revision, next)
				if repairErr == nil {
					response := controller.finishMutationLocked(repairCtx, request, digest, repaired, "device.unbind", nodeIDs)
					cancelRepair()
					return response
				}
				observed, observeErr = controller.desiredStore.Load()
				if observeErr == nil && controllerConfigMatchesStoredMutation(current, next, observed) {
					response := controller.finishObservedMutationLocked(request, digest, observed, "device.unbind", nodeIDs)
					cancelRepair()
					return response
				}
				cancelRepair()
			}
		}
		return controllerModelError(request.ID, err)
	}
	return controller.finishMutationLocked(ctx, request, digest, stored, "device.unbind", nodeIDs)
}

func (controller *Controller) handleDeviceBindingsReplace(ctx context.Context, request api.Request) api.Response {
	var params replaceDeviceBindingsParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" || params.NodeID == "" || params.ExpectedRevision == nil || *params.ExpectedRevision == 0 || len(params.DeviceIDs) > 60 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	selected := make(map[string]struct{}, len(params.DeviceIDs))
	for _, deviceID := range params.DeviceIDs {
		if deviceID == "" {
			return controllerError(request.ID, ErrorCodeInvalidRequest)
		}
		if _, exists := selected[deviceID]; exists {
			return controllerError(request.ID, ErrorCodeInvalidRequest)
		}
		selected[deviceID] = struct{}{}
	}
	digest := controllerDigest(request.Method, params)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if response, handled := controller.replayLocked(request, digest); handled {
		return response
	}
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	current, err := controller.desiredStore.Load()
	if err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if current.Revision != *params.ExpectedRevision {
		return controllerError(request.ID, ErrorCodeRevisionConflict)
	}
	target, exists := current.Nodes[params.NodeID]
	if !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	if !current.Global.Enabled || !target.Enabled {
		return controllerError(request.ID, ErrorCodeInvalidConfig)
	}
	discovered := make(map[string]platform.DiscoveredDevice)
	if controller.deviceSource != nil {
		listed, listErr := controller.deviceSource.List(ctx)
		if listErr != nil {
			return controllerError(request.ID, ErrorCodeInternal)
		}
		for _, device := range listed {
			if device.ID != "" {
				discovered[device.ID] = device
			}
		}
	}
	next := cloneControllerConfig(current)
	oldNodes := make(map[string]struct{})
	targetChanged := false
	for id, device := range current.Devices {
		if device.NodeID != params.NodeID {
			continue
		}
		if _, keep := selected[id]; keep {
			continue
		}
		device.NodeID = ""
		device.Enabled = false
		next.Devices[id] = device
		targetChanged = true
	}
	for _, id := range params.DeviceIDs {
		device, configured := current.Devices[id]
		if configured && device.NodeID == params.NodeID && device.Enabled {
			continue
		}
		observed, found := discovered[id]
		if controller.deviceSource != nil {
			if !found {
				return controllerError(request.ID, ErrorCodeNotFound)
			}
			if !observed.Confirmed || !observed.IPv4.Is4() || observed.MAC == "" {
				return controllerError(request.ID, ErrorCodeInvalidConfig)
			}
			if configured && device.MAC != observed.MAC {
				return controllerError(request.ID, ErrorCodeInvalidConfig)
			}
		}
		if !configured {
			if controller.deviceSource == nil {
				return controllerError(request.ID, ErrorCodeNotFound)
			}
			device = model.Device{ID: observed.ID, MAC: observed.MAC, Hostname: observed.Hostname, FixedIPv4: observed.IPv4}
		}
		if device.NodeID != "" && device.NodeID != params.NodeID {
			oldNodes[device.NodeID] = struct{}{}
		}
		device.NodeID = params.NodeID
		device.Enabled = true
		next.Devices[id] = device
		targetChanged = true
	}
	if !targetChanged && len(oldNodes) == 0 {
		return controller.finishMutationLocked(ctx, request, digest, current, "device.bindings.replace", nil)
	}
	beforeLeases := activeLeaseDevices(current)
	afterLeases := activeLeaseDevices(next)
	projectionManager, ok := controller.leaseManager.(platform.LeaseProjectionManager)
	if !ok {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if err := projectionManager.Replace(ctx, beforeLeases, afterLeases, current.Revision+1); err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		observed, observeErr := controller.desiredStore.Load()
		if observeErr == nil && controllerConfigMatchesStoredMutation(current, next, observed) {
			return controller.finishObservedMutationLocked(request, digest, observed, "device.bindings.replace", orderedBindingNodeIDs(oldNodes, params.NodeID, targetChanged))
		}
		if observeErr == nil && controllerConfigsEqual(observed, current) {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), controller.leaseRollbackTimeout)
			_ = projectionManager.Replace(rollbackCtx, afterLeases, beforeLeases, current.Revision+1)
			cancel()
		}
		return controllerModelError(request.ID, err)
	}
	return controller.finishMutationLocked(ctx, request, digest, stored, "device.bindings.replace", orderedBindingNodeIDs(oldNodes, params.NodeID, targetChanged))
}

func activeLeaseDevices(cfg model.DesiredConfig) []model.Device {
	ids := make([]string, 0, len(cfg.Devices))
	for id, device := range cfg.Devices {
		if device.Enabled && device.NodeID != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	devices := make([]model.Device, 0, len(ids))
	for _, id := range ids {
		devices = append(devices, cfg.Devices[id])
	}
	return devices
}

func orderedBindingNodeIDs(oldNodes map[string]struct{}, target string, targetChanged bool) []string {
	ids := make([]string, 0, len(oldNodes)+1)
	for nodeID := range oldNodes {
		if nodeID != target {
			ids = append(ids, nodeID)
		}
	}
	sort.Strings(ids)
	if targetChanged {
		ids = append(ids, target)
	}
	return ids
}

func (controller *Controller) handleNodeAction(ctx context.Context, request api.Request) api.Response {
	var params nodeActionParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" || params.NodeID == "" || params.ExpectedRevision == nil || *params.ExpectedRevision == 0 || (params.Action != "connect" && params.Action != "reconnect" && params.Action != "stop") {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	digest := controllerDigest(request.Method, params)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if response, handled := controller.replayLocked(request, digest); handled {
		return response
	}
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	current, err := controller.desiredStore.Load()
	if err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if current.Revision != *params.ExpectedRevision {
		return controllerError(request.ID, ErrorCodeRevisionConflict)
	}
	node, exists := current.Nodes[params.NodeID]
	if !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	if node.DeletePending {
		return controllerError(request.ID, ErrorCodeInvalidConfig)
	}
	stored := current
	wantEnabled := params.Action != "stop"
	if params.Action != "reconnect" && node.Enabled != wantEnabled {
		next := cloneControllerConfig(current)
		node.Enabled = wantEnabled
		next.Nodes[params.NodeID] = node
		stored, err = controller.desiredStore.Replace(ctx, current.Revision, next)
		if err != nil {
			return controllerModelError(request.ID, err)
		}
	}
	return controller.finishMutationLocked(ctx, request, digest, stored, "node."+params.Action, []string{params.NodeID})
}

func (controller *Controller) handleNodeSave(ctx context.Context, request api.Request) api.Response {
	var params nodeSaveParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" ||
		params.ExpectedRevision == nil || *params.ExpectedRevision == 0 ||
		strings.TrimSpace(params.Name) == "" || strings.TrimSpace(params.Server) == "" ||
		params.Port == 0 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	if params.Protocol != model.ProtocolL2TP {
		return controllerError(request.ID, ErrorCodeUnsupported)
	}
	var expiresAt *time.Time
	if params.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, params.ExpiresAt)
		if err != nil {
			return controllerError(request.ID, ErrorCodeInvalidRequest)
		}
		parsed = parsed.UTC()
		expiresAt = &parsed
	}
	digest := controllerDigest(request.Method, params)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if response, handled := controller.replayLocked(request, digest); handled {
		return response
	}
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	current, err := controller.desiredStore.Load()
	if err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if current.Revision != *params.ExpectedRevision {
		return controllerError(request.ID, ErrorCodeRevisionConflict)
	}
	nodeID := params.NodeID
	previous, updating := current.Nodes[nodeID]
	if nodeID != "" && !updating {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	if updating && previous.DeletePending {
		return controllerError(request.ID, ErrorCodeInvalidConfig)
	}
	if !updating {
		if len(current.Nodes) >= 60 {
			return controllerError(request.ID, ErrorCodeCapacityExceeded)
		}
		nodeID = controller.allocateNodeIDLocked(current.Nodes)
		if nodeID == "" {
			return controllerError(request.ID, ErrorCodeCapacityExceeded)
		}
	}
	username := params.Username
	if updating && username == "" {
		username = previous.Username
	}
	if username == "" {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	password := params.Password
	if updating && password == "" {
		password = previous.Password
	}
	node := model.Node{
		ID: nodeID, Name: strings.TrimSpace(params.Name), Protocol: params.Protocol, Enabled: params.Enabled,
		Server: strings.TrimSpace(params.Server), Port: params.Port, Username: username, Password: password,
		ExpiresAt: expiresAt, PolicyID: previous.PolicyID, Revision: previous.Revision,
	}
	if !updating {
		node.PolicyID = firstFreePolicyID(current.Nodes)
	}
	next := cloneControllerConfig(current)
	next.Nodes[nodeID] = node
	if err := model.Validate(next); err != nil {
		return controllerModelError(request.ID, err)
	}
	nodeIDs := []string(nil)
	if (updating && previous.Enabled) || (current.Global.Enabled && node.Enabled) {
		nodeIDs = []string{nodeID}
	}
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		observed, observeErr := controller.desiredStore.Load()
		if observeErr == nil && controllerConfigMatchesStoredMutation(current, next, observed) {
			return controller.finishObservedReplayDurableMutationLocked(request, digest, observed, "node.save", nodeIDs)
		}
		return controllerModelError(request.ID, err)
	}
	return controller.finishReplayDurableMutationLocked(ctx, request, digest, stored, "node.save", nodeIDs)
}

func (controller *Controller) handleNodeDelete(ctx context.Context, request api.Request) api.Response {
	var params nodeDeleteParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" || params.NodeID == "" ||
		params.ExpectedRevision == nil || *params.ExpectedRevision == 0 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	digest := controllerDigest(request.Method, params)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if response, handled := controller.replayLocked(request, digest); handled {
		return response
	}
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	current, err := controller.desiredStore.Load()
	if err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if current.Revision != *params.ExpectedRevision {
		return controllerError(request.ID, ErrorCodeRevisionConflict)
	}
	node, exists := current.Nodes[params.NodeID]
	if !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	if node.DeletePending {
		return controller.finishReplayDurableMutationLocked(ctx, request, digest, current, "node.delete", []string{params.NodeID})
	}
	next := cloneControllerConfig(current)
	node.Enabled = false
	node.DeletePending = true
	next.Nodes[params.NodeID] = node
	for id, device := range next.Devices {
		if device.NodeID != params.NodeID {
			continue
		}
		device.NodeID = ""
		device.Enabled = false
		next.Devices[id] = device
	}
	for id, binding := range next.PendingBindings {
		if binding.NodeID == params.NodeID {
			delete(next.PendingBindings, id)
		}
	}
	if err := model.Validate(next); err != nil {
		return controllerModelError(request.ID, err)
	}
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		observed, observeErr := controller.desiredStore.Load()
		if observeErr == nil && controllerConfigMatchesStoredMutation(current, next, observed) {
			return controller.finishObservedReplayDurableMutationLocked(request, digest, observed, "node.delete", []string{params.NodeID})
		}
		return controllerModelError(request.ID, err)
	}
	return controller.finishReplayDurableMutationLocked(ctx, request, digest, stored, "node.delete", []string{params.NodeID})
}

func (controller *Controller) allocateNodeIDLocked(nodes map[string]model.Node) string {
	for attempt := 0; attempt < 64; attempt++ {
		id := controller.newNodeID()
		if id == "" {
			continue
		}
		if _, exists := nodes[id]; !exists {
			return id
		}
	}
	return ""
}

func firstFreePolicyID(nodes map[string]model.Node) uint16 {
	used := make(map[uint16]struct{}, len(nodes))
	for _, node := range nodes {
		used[node.PolicyID] = struct{}{}
	}
	for policyID := uint16(1); policyID <= 60; policyID++ {
		if _, exists := used[policyID]; !exists {
			return policyID
		}
	}
	return 0
}

func randomControllerNodeID() string {
	id := strings.TrimPrefix(randomReconciliationID(), "reconcile-")
	return "node_" + strings.ReplaceAll(id, "-", "_")
}

func (controller *Controller) handleImportPreview(ctx context.Context, request api.Request) api.Response {
	var params importPreviewParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" || params.Raw == "" || params.ExpectedRevision == nil || *params.ExpectedRevision == 0 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	current, err := controller.desiredStore.Load()
	if err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if current.Revision != *params.ExpectedRevision {
		return controllerError(request.ID, ErrorCodeRevisionConflict)
	}
	preview, err := controller.importer.Preview(ctx, importer.PreviewRequest{Protocol: params.Protocol, Raw: params.Raw, Base: current})
	if err != nil {
		return controllerImporterError(request.ID, err)
	}
	return controllerResult(request.ID, preview)
}

func (controller *Controller) handleImportCommit(ctx context.Context, request api.Request) api.Response {
	var params importCommitParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" || params.PreviewID == "" || params.PreviewHash == "" || params.ExpectedRevision == nil || *params.ExpectedRevision == 0 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	digest := controllerDigest(request.Method, params)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if response, handled := controller.replayLocked(request, digest); handled {
		return response
	}
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	current, err := controller.desiredStore.Load()
	if err != nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if current.Revision != *params.ExpectedRevision {
		return controllerError(request.ID, ErrorCodeRevisionConflict)
	}
	commit := importer.CommitRequest{PreviewID: params.PreviewID, PreviewHash: params.PreviewHash, ExpectedRevision: *params.ExpectedRevision}
	candidates, err := controller.importer.ValidateCommit(ctx, commit)
	if err != nil {
		return controllerImporterError(request.ID, err)
	}
	next, nodeIDs, err := importer.Merge(current, candidates)
	if err != nil {
		return controllerModelError(request.ID, err)
	}
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		observed, observeErr := controller.desiredStore.Load()
		if observeErr == nil && controllerConfigMatchesStoredMutation(current, next, observed) {
			controller.importer.Consume(params.PreviewID)
			return controller.finishObservedDurableMutationLocked(request, digest, observed, "import.commit", nodeIDs)
		}
		return controllerModelError(request.ID, err)
	}
	controller.importer.Consume(params.PreviewID)
	return controller.finishDurableMutationLocked(ctx, request, digest, stored, "import.commit", nodeIDs)
}

func (controller *Controller) finishMutationLocked(ctx context.Context, request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string) api.Response {
	return controller.finishMutationWithDurabilityLocked(ctx, request, digest, desired, kind, nodeIDs, false, false)
}

func (controller *Controller) finishDurableMutationLocked(ctx context.Context, request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string) api.Response {
	return controller.finishMutationWithDurabilityLocked(ctx, request, digest, desired, kind, nodeIDs, true, false)
}

func (controller *Controller) finishReplayDurableMutationLocked(ctx context.Context, request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string) api.Response {
	return controller.finishMutationWithDurabilityLocked(ctx, request, digest, desired, kind, nodeIDs, true, true)
}

func (controller *Controller) finishMutationWithDurabilityLocked(ctx context.Context, request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string, requireRuntimeDurable, persistReplayWithJob bool) api.Response {
	beforeJobs := controller.jobs.Snapshot()
	// Desired configuration was durably replaced before this function. Keep
	// the in-memory revision aligned even if later runtime persistence fails.
	controller.desired = cloneControllerConfig(desired)
	job := newControllerJob(controller.newJobID(), kind, controller.now(), desired.Revision, nodeIDs)
	if err := controller.jobs.Put(job); err != nil {
		return controllerModelError(request.ID, err)
	}
	resultBytes, err := json.Marshal(mutationResult{JobID: job.ID, ConfigRevision: desired.Revision})
	if err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		return controllerError(request.ID, ErrorCodeInternal)
	}
	record := IdempotencyRecord{
		RequestID: request.ID, Method: request.Method, Digest: digest,
		Result: append(json.RawMessage(nil), resultBytes...), ConfigRevision: desired.Revision, CreatedAt: controller.now(),
	}
	if requireRuntimeDurable {
		if persistReplayWithJob {
			controller.addIdempotencyLocked(record)
			if err := controller.persistLocked(ctx); err != nil {
				if controller.scheduler != nil && !isTerminalJob(job.State) {
					controller.scheduler.Submit(job)
				}
				return controllerError(request.ID, ErrorCodeInternal)
			}
			if controller.scheduler != nil && !isTerminalJob(job.State) {
				controller.scheduler.Submit(job)
			}
			return api.Response{Version: api.ProtocolVersion, ID: request.ID, Result: resultBytes}
		}
		// Persist the promised job before adding the success replay record. If
		// Save reports an error after its atomic rename, a restart may observe
		// the job, but it can never observe a success response we did not return.
		if err := controller.persistLocked(ctx); err != nil {
			if controller.scheduler != nil && !isTerminalJob(job.State) {
				controller.scheduler.Submit(job)
			}
			return controllerError(request.ID, ErrorCodeInternal)
		}
		controller.addIdempotencyLocked(record)
		// The job is already durable. Failure of this best-effort second write
		// only means a restart may lose idempotent replay, not the returned job.
		_ = controller.persistLocked(ctx)
		if controller.scheduler != nil && !isTerminalJob(job.State) {
			controller.scheduler.Submit(job)
		}
		return api.Response{Version: api.ProtocolVersion, ID: request.ID, Result: resultBytes}
	}
	controller.addIdempotencyLocked(record)
	if err := controller.persistLocked(ctx); err != nil {
		// Desired state is already committed. Runtime metadata is observational,
		// so retain and submit the in-memory cleanup job even when its first
		// snapshot write fails. Later scheduler transitions retry persistence;
		// startup reconciliation covers a restart in the meantime.
		if controller.scheduler != nil && !isTerminalJob(job.State) {
			controller.scheduler.Submit(job)
		}
		return api.Response{Version: api.ProtocolVersion, ID: request.ID, Result: resultBytes}
	}
	if controller.scheduler != nil && !isTerminalJob(job.State) {
		controller.scheduler.Submit(job)
	}
	return api.Response{Version: api.ProtocolVersion, ID: request.ID, Result: resultBytes}
}

func (controller *Controller) finishObservedMutationLocked(request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string) api.Response {
	return controller.finishObservedMutationWithDurabilityLocked(request, digest, desired, kind, nodeIDs, false, false)
}

func (controller *Controller) finishObservedDurableMutationLocked(request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string) api.Response {
	return controller.finishObservedMutationWithDurabilityLocked(request, digest, desired, kind, nodeIDs, true, false)
}

func (controller *Controller) finishObservedReplayDurableMutationLocked(request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string) api.Response {
	return controller.finishObservedMutationWithDurabilityLocked(request, digest, desired, kind, nodeIDs, true, true)
}

func (controller *Controller) finishObservedMutationWithDurabilityLocked(request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string, requireRuntimeDurable, persistReplayWithJob bool) api.Response {
	durableCtx, cancel := context.WithTimeout(context.Background(), controller.leaseRollbackTimeout)
	err := controller.desiredStore.EnsureDurable(durableCtx)
	if err == nil {
		response := controller.finishMutationWithDurabilityLocked(durableCtx, request, digest, desired, kind, nodeIDs, requireRuntimeDurable, persistReplayWithJob)
		cancel()
		return response
	}
	cancel()

	// The new desired state is visible but crash durability is uncertain. Do
	// not claim success or persist a possibly-ahead runtime revision, but still
	// revoke old access immediately in this process.
	controller.desired = cloneControllerConfig(desired)
	job := newControllerJob(controller.newJobID(), kind, controller.now(), desired.Revision, nodeIDs)
	if err := controller.jobs.Put(job); err == nil && controller.scheduler != nil && !isTerminalJob(job.State) {
		controller.scheduler.Submit(job)
	}
	return controllerError(request.ID, ErrorCodeInternal)
}

func (controller *Controller) handleJobGet(request api.Request) api.Response {
	var params jobGetParams
	if decodeControllerParams(request.Params, &params) != nil || params.JobID == "" {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	job, exists := controller.jobs.Get(params.JobID)
	if !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	return controllerResult(request.ID, job)
}

func (controller *Controller) handleJobList(request api.Request) api.Response {
	var params emptyParams
	if decodeControllerParams(request.Params, &params) != nil {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	return controllerResult(request.ID, struct {
		Jobs []Job `json:"jobs"`
	}{controller.jobs.List()})
}

func (controller *Controller) handleEvents(request api.Request) api.Response {
	var params eventsParams
	if decodeControllerParams(request.Params, &params) != nil || params.Limit < 1 || params.Limit > 200 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	events := controller.jobs.Events()
	filtered := make([]NodeEvent, 0, params.Limit)
	for _, event := range events {
		if event.Sequence > params.AfterSequence {
			filtered = append(filtered, event)
			if len(filtered) == params.Limit {
				break
			}
		}
	}
	return controllerResult(request.ID, struct {
		Events []NodeEvent `json:"events"`
	}{filtered})
}

func (controller *Controller) handleDiagnosticsCreate(request api.Request) api.Response {
	var params emptyParams
	if decodeControllerParams(request.Params, &params) != nil {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	if controller.diagnostics == nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	status, err := controller.diagnostics.Create()
	if err != nil {
		if errors.Is(err, diagnostics.ErrDiagnosticCapacity) {
			return controllerError(request.ID, ErrorCodeCapacityExceeded)
		}
		return controllerError(request.ID, ErrorCodeInternal)
	}
	return controllerResult(request.ID, status)
}

func (controller *Controller) handleDiagnosticsGet(request api.Request) api.Response {
	var params diagnosticJobParams
	if decodeControllerParams(request.Params, &params) != nil || !diagnosticJobParamPattern.MatchString(params.JobID) {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	if controller.diagnostics == nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	status, exists := controller.diagnostics.Get(params.JobID)
	if !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	return controllerResult(request.ID, status)
}

func (controller *Controller) handleDiagnosticsClaim(request api.Request) api.Response {
	var params diagnosticArtifactParams
	if decodeControllerParams(request.Params, &params) != nil || !diagnosticArtifactParamPattern.MatchString(params.ArtifactID) {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	if controller.diagnostics == nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	claim, err := controller.diagnostics.Claim(params.ArtifactID)
	if err != nil {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	return controllerResult(request.ID, diagnosticClaimResult{ArtifactID: claim.ArtifactID, Path: claim.Path, Filename: claim.Filename, Size: claim.Size})
}

func (controller *Controller) handleDiagnosticsRelease(request api.Request) api.Response {
	var params diagnosticArtifactParams
	if decodeControllerParams(request.Params, &params) != nil || !diagnosticArtifactParamPattern.MatchString(params.ArtifactID) {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	if controller.diagnostics == nil {
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if err := controller.diagnostics.Release(params.ArtifactID); err != nil {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	return controllerResult(request.ID, struct {
		Released bool `json:"released"`
	}{true})
}

// DiagnosticSnapshot captures only public controller DTOs. Authentication
// material is returned separately so the collector can redact it from command
// output without ever serializing it into a seed entry.
func (controller *Controller) DiagnosticSnapshot(ctx context.Context) (diagnostics.Snapshot, error) {
	if controller == nil || contextDone(ctx) != nil {
		return diagnostics.Snapshot{}, errors.New("diagnostic snapshot is unavailable")
	}
	controller.mu.Lock()
	secrets := make([]string, 0, len(controller.desired.Nodes)*4)
	for _, node := range controller.desired.Nodes {
		secrets = append(secrets, node.Username, node.Password, node.SLPToken, node.SLPObfsKey)
	}
	controller.mu.Unlock()
	requests := []struct{ name, method, params string }{
		{"status.json", "status.get", `{}`},
		{"jobs.json", "job.list", `{}`},
		{"events.json", "system.events", `{"after_sequence":0,"limit":200}`},
	}
	entries := make(map[string][]byte, len(requests))
	for _, item := range requests {
		if err := contextDone(ctx); err != nil {
			return diagnostics.Snapshot{}, err
		}
		response := controller.Handle(ctx, controllerRequestForDiagnostic(item.method, item.params))
		if response.Error != nil || !json.Valid(response.Result) {
			return diagnostics.Snapshot{}, errors.New("diagnostic snapshot collection failed")
		}
		entries[item.name] = append([]byte(nil), response.Result...)
	}
	entries["config-summary.json"] = append([]byte(nil), entries["status.json"]...)
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	fdCount := -1
	if descriptors, err := os.ReadDir("/proc/self/fd"); err == nil {
		fdCount = len(descriptors)
	}
	metrics, err := json.Marshal(struct {
		Goroutines int    `json:"goroutines"`
		FDCount    int    `json:"fd_count"`
		HeapBytes  uint64 `json:"heap_bytes"`
		SysBytes   uint64 `json:"sys_bytes"`
	}{runtime.NumGoroutine(), fdCount, memory.HeapAlloc, memory.Sys})
	if err != nil {
		return diagnostics.Snapshot{}, errors.New("diagnostic metrics collection failed")
	}
	entries["daemon-metrics.json"] = metrics
	entries["managed-processes.json"] = diagnostics.ReadManagedProcessMetadata("/proc")
	return diagnostics.Snapshot{Entries: entries, Secrets: secrets}, nil
}

func controllerRequestForDiagnostic(method, params string) api.Request {
	return api.Request{Version: api.ProtocolVersion, ID: "diagnostic-snapshot", Method: method, Params: json.RawMessage(params)}
}

func (controller *Controller) handleInterfaceEvent(ctx context.Context, request api.Request) api.Response {
	var params interfaceEventParams
	if decodeControllerParams(request.Params, &params) != nil || request.ID == "" || len(params.Interface) != 8 || params.Interface[:4] != "ppv2" ||
		(params.Action != "ifup" && params.Action != "ifdown" && params.Action != "update") {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	policy, err := strconv.ParseUint(params.Interface[4:], 10, 16)
	if err != nil || policy == 0 || policy > 60 {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if contextDone(ctx) != nil {
		return controllerError(request.ID, "operation_timeout")
	}
	nodeID := ""
	enabled := false
	for id, node := range controller.desired.Nodes {
		if node.PolicyID == uint16(policy) {
			nodeID, enabled = id, node.Enabled && controller.desired.Global.Enabled
			break
		}
	}
	if nodeID == "" {
		return controllerError(request.ID, ErrorCodeInvalidRequest)
	}
	if !enabled {
		return controllerResult(request.ID, interfaceEventResult{ConfigRevision: controller.desired.Revision, Ignored: true})
	}
	for _, job := range controller.jobs.List() {
		if isTerminalJob(job.State) {
			continue
		}
		for _, progress := range job.Nodes {
			if progress.NodeID == nodeID {
				return controllerResult(request.ID, interfaceEventResult{JobID: job.ID, ConfigRevision: controller.desired.Revision})
			}
		}
	}
	job := newControllerJob(controller.newJobID(), "system.recover", controller.now(), controller.desired.Revision, []string{nodeID})
	beforeJobs := controller.jobs.Snapshot()
	if err := controller.jobs.Put(job); err != nil {
		return controllerModelError(request.ID, err)
	}
	if err := controller.persistLocked(ctx); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		return controllerError(request.ID, ErrorCodeInternal)
	}
	if controller.scheduler != nil {
		controller.scheduler.Submit(job)
	}
	return controllerResult(request.ID, interfaceEventResult{JobID: job.ID, ConfigRevision: controller.desired.Revision})
}

func (controller *Controller) replayLocked(request api.Request, digest string) (api.Response, bool) {
	record, exists := controller.idempotency[request.ID]
	if !exists {
		return api.Response{}, false
	}
	if record.Method != request.Method || record.Digest != digest {
		return controllerError(request.ID, ErrorCodeDuplicate), true
	}
	return api.Response{Version: api.ProtocolVersion, ID: request.ID, Result: append(json.RawMessage(nil), record.Result...)}, true
}

func (controller *Controller) addIdempotencyLocked(record IdempotencyRecord) *IdempotencyRecord {
	controller.idempotency[record.RequestID] = cloneIdempotencyRecord(record)
	controller.idempotencyOrder = append(controller.idempotencyOrder, record.RequestID)
	if len(controller.idempotencyOrder) <= MaxRuntimeIdempotencyRecords {
		return nil
	}
	oldestID := controller.idempotencyOrder[0]
	controller.idempotencyOrder = controller.idempotencyOrder[1:]
	oldest := controller.idempotency[oldestID]
	delete(controller.idempotency, oldestID)
	return &oldest
}

func (controller *Controller) persistLocked(ctx context.Context) error {
	statuses := make([]NodeStatus, 0, len(controller.statuses))
	statusIDs := make([]string, 0, len(controller.statuses))
	for id := range controller.statuses {
		statusIDs = append(statusIDs, id)
	}
	sort.Strings(statusIDs)
	for _, id := range statusIDs {
		statuses = append(statuses, cloneNodeStatus(controller.statuses[id]))
	}
	records := make([]IdempotencyRecord, 0, len(controller.idempotencyOrder))
	for _, id := range controller.idempotencyOrder {
		records = append(records, cloneIdempotencyRecord(controller.idempotency[id]))
	}
	return controller.runtimeStore.Save(ctx, RuntimeSnapshot{
		SchemaVersion: RuntimeSnapshotSchemaVersion, ConfigRevision: controller.desired.Revision,
		Jobs: controller.jobs.Snapshot(), NodeStatuses: statuses, Idempotency: records,
	})
}

func newControllerJob(id, kind string, at time.Time, revision uint64, nodeIDs []string) Job {
	if len(nodeIDs) == 0 {
		return Job{ID: id, Kind: kind, Creator: "api", CreatedAt: at, ConfigRevision: revision, State: JobSucceeded}
	}
	job := Job{
		ID: id, Kind: kind, Creator: "api", CreatedAt: at, ConfigRevision: revision,
		State: JobQueued, Total: len(nodeIDs), Queued: len(nodeIDs), Nodes: make([]NodeProgress, 0, len(nodeIDs)),
	}
	for _, nodeID := range nodeIDs {
		job.Nodes = append(job.Nodes, NodeProgress{NodeID: nodeID, Step: "queued", State: model.StateQueued})
	}
	return job
}

func decodeControllerParams(raw json.RawMessage, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return errors.New("parameters must be an object")
	}
	envelope, err := json.Marshal(api.Request{Version: api.ProtocolVersion, ID: "validation", Method: "status.get", Params: raw})
	if err != nil {
		return err
	}
	if _, err := api.ParseRequest(envelope); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("parameters contain trailing data")
	}
	return nil
}

func controllerDigest(method string, params any) string {
	encoded, _ := json.Marshal(struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}{method, params})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func controllerConfigMatchesStoredMutation(current, next, observed model.DesiredConfig) bool {
	if current.Revision == ^uint64(0) {
		return false
	}
	expected := cloneControllerConfig(next)
	expected.Revision = current.Revision + 1
	for id, node := range expected.Nodes {
		previous, exists := current.Nodes[id]
		if exists && sameControllerNodeIgnoringRevision(previous, node) {
			node.Revision = previous.Revision
		} else {
			node.Revision = expected.Revision
		}
		expected.Nodes[id] = node
	}
	return controllerConfigsEqual(expected, observed)
}

func sameControllerNodeIgnoringRevision(left, right model.Node) bool {
	left.Revision = 0
	right.Revision = 0
	return reflect.DeepEqual(left, right)
}

func controllerConfigsEqual(left, right model.DesiredConfig) bool {
	return reflect.DeepEqual(left, right)
}

func controllerResult(id string, value any) api.Response {
	encoded, err := json.Marshal(value)
	if err != nil {
		return controllerError(id, ErrorCodeInternal)
	}
	return api.Response{Version: api.ProtocolVersion, ID: id, Result: encoded}
}

func controllerModelError(id string, err error) api.Response {
	var codeErr *model.CodeError
	if errors.As(err, &codeErr) {
		switch codeErr.Code {
		case ErrorCodeInvalidRequest, ErrorCodeInvalidConfig, ErrorCodeRevisionConflict, ErrorCodeCapacityExceeded, ErrorCodeDuplicate, ErrorCodeNotFound, ErrorCodeUnsupported:
			return controllerError(id, codeErr.Code)
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return controllerError(id, "operation_timeout")
	}
	return controllerError(id, ErrorCodeInternal)
}

func controllerImporterError(id string, err error) api.Response {
	var coded *model.CodeError
	if !errors.As(err, &coded) {
		return controllerModelError(id, err)
	}
	switch coded.Code {
	case importer.ErrorRevisionConflict:
		return controllerError(id, ErrorCodeRevisionConflict)
	case importer.ErrorCapacityExceeded, importer.ErrorPreviewCapacity:
		return controllerError(id, ErrorCodeCapacityExceeded)
	case importer.ErrorPreviewNotFound, importer.ErrorPreviewExpired:
		return controllerError(id, ErrorCodeNotFound)
	case importer.ErrorPreviewBlocked:
		return controllerError(id, ErrorCodeInvalidConfig)
	default:
		return controllerError(id, ErrorCodeInvalidRequest)
	}
}

func controllerError(id, code string) api.Response {
	messages := map[string]string{
		ErrorCodeInvalidRequest:   "request parameters are invalid",
		ErrorCodeInvalidConfig:    "desired configuration is invalid",
		ErrorCodeRevisionConflict: "configuration revision does not match",
		ErrorCodeCapacityExceeded: "controller capacity is exceeded",
		ErrorCodeDuplicate:        "request ID is already used by different work",
		ErrorCodeNotFound:         "requested object was not found",
		ErrorCodeUnsupported:      "requested protocol is not supported",
		ErrorCodeInternal:         "internal control error",
		"operation_timeout":       "operation timed out",
	}
	message, exists := messages[code]
	if !exists {
		code, message = ErrorCodeInternal, messages[ErrorCodeInternal]
	}
	return api.Response{Version: api.ProtocolVersion, ID: id, Error: &api.Error{Code: code, Message: message}}
}

func cloneControllerConfig(cfg model.DesiredConfig) model.DesiredConfig {
	clone := cfg
	clone.Global.ManagementPorts = append([]uint16(nil), cfg.Global.ManagementPorts...)
	clone.Global.DoHEndpoints = append([]model.DoHEndpoint(nil), cfg.Global.DoHEndpoints...)
	clone.Nodes = make(map[string]model.Node, len(cfg.Nodes))
	for id, node := range cfg.Nodes {
		if node.ExpiresAt != nil {
			expires := *node.ExpiresAt
			node.ExpiresAt = &expires
		}
		clone.Nodes[id] = node
	}
	clone.Devices = make(map[string]model.Device, len(cfg.Devices))
	for id, device := range cfg.Devices {
		clone.Devices[id] = device
	}
	clone.PendingBindings = make(map[string]model.PendingBinding, len(cfg.PendingBindings))
	for id, binding := range cfg.PendingBindings {
		clone.PendingBindings[id] = binding
	}
	return clone
}

func cloneIdempotencyRecord(record IdempotencyRecord) IdempotencyRecord {
	record.Result = append(json.RawMessage(nil), record.Result...)
	return record
}

func contextDone(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (controller *Controller) String() string {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return fmt.Sprintf("engine.Controller{Revision:%d Jobs:%d Statuses:%d}", controller.desired.Revision, len(controller.jobs.List()), len(controller.statuses))
}
