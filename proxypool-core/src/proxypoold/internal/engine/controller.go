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
	"sort"
	"sync"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/model"
)

type desiredConfigStore interface {
	Load() (model.DesiredConfig, error)
	Replace(context.Context, uint64, model.DesiredConfig) (model.DesiredConfig, error)
}

type runtimePersistence interface {
	Load() (RuntimeSnapshot, error)
	Save(context.Context, RuntimeSnapshot) error
}

type ControllerOption func(*Controller)

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

// Controller is the formal V2 single writer. Platform work is deliberately
// only queued here; Scheduler becomes the sole side-effect owner in Task 4.
type Controller struct {
	mu sync.Mutex

	desiredStore desiredConfigStore
	runtimeStore runtimePersistence
	machine      *Machine
	jobs         *JobStore

	desired          model.DesiredConfig
	statuses         map[string]NodeStatus
	idempotency      map[string]IdempotencyRecord
	idempotencyOrder []string
	now              func() time.Time
	newJobID         func() string
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
		desiredStore: desiredStore,
		runtimeStore: runtimeStore,
		machine:      machine,
		jobs:         jobs,
		statuses:     make(map[string]NodeStatus),
		idempotency:  make(map[string]IdempotencyRecord),
		now:          time.Now,
		newJobID:     randomReconciliationID,
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
		return nil, errors.New("live runtime revision is ahead of desired configuration")
	}
	if err := jobs.Restore(snapshot.Jobs); err != nil {
		return nil, errors.New("live runtime jobs are invalid")
	}
	for _, status := range snapshot.NodeStatuses {
		controller.statuses[status.NodeID] = cloneNodeStatus(status)
	}
	for _, record := range snapshot.Idempotency {
		controller.idempotency[record.RequestID] = cloneIdempotencyRecord(record)
		controller.idempotencyOrder = append(controller.idempotencyOrder, record.RequestID)
	}
	if snapshot.ConfigRevision < desired.Revision {
		// A desired write may have reached disk immediately before a crash or
		// runtime persistence failure. Never carry observational online state
		// across that revision gap.
		controller.statuses = make(map[string]NodeStatus)
		if err := controller.persistLocked(context.Background()); err != nil {
			return nil, errors.New("live runtime reconciliation persistence failed")
		}
	}
	return controller, nil
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
	case "node.action":
		return controller.handleNodeAction(ctx, request)
	case "job.get":
		return controller.handleJobGet(request)
	case "job.list":
		return controller.handleJobList(request)
	case "system.events":
		return controller.handleEvents(request)
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

type nodeActionParams struct {
	NodeID           string  `json:"node_id"`
	Action           string  `json:"action"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

type jobGetParams struct {
	JobID string `json:"job_id"`
}

type eventsParams struct {
	AfterSequence uint64 `json:"after_sequence"`
	Limit         int    `json:"limit"`
}

type mutationResult struct {
	JobID          string `json:"job_id"`
	ConfigRevision uint64 `json:"config_revision"`
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
			runtimeSummary.Nodes[index].State = status.State
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
	controller.mu.Lock()
	defer controller.mu.Unlock()
	desired, _ := summarizeDesired(controller.desired)
	return controllerResult(request.ID, struct {
		ConfigRevision uint64                `json:"config_revision"`
		Devices        []DesiredDeviceStatus `json:"devices"`
	}{controller.desired.Revision, desired.Devices})
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
	device, exists := current.Devices[params.DeviceID]
	if !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	if _, exists := current.Nodes[params.NodeID]; !exists {
		return controllerError(request.ID, ErrorCodeNotFound)
	}
	next := cloneControllerConfig(current)
	device.NodeID = params.NodeID
	device.Enabled = true
	next.Devices[params.DeviceID] = device
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		return controllerModelError(request.ID, err)
	}
	return controller.finishMutationLocked(ctx, request, digest, stored, "device.bind", []string{params.NodeID})
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
	stored, err := controller.desiredStore.Replace(ctx, current.Revision, next)
	if err != nil {
		return controllerModelError(request.ID, err)
	}
	nodeIDs := []string(nil)
	if oldNodeID != "" {
		nodeIDs = []string{oldNodeID}
	}
	return controller.finishMutationLocked(ctx, request, digest, stored, "device.unbind", nodeIDs)
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

func (controller *Controller) finishMutationLocked(ctx context.Context, request api.Request, digest string, desired model.DesiredConfig, kind string, nodeIDs []string) api.Response {
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
	removed := controller.addIdempotencyLocked(record)
	if err := controller.persistLocked(ctx); err != nil {
		_ = controller.jobs.Restore(beforeJobs)
		controller.removeIdempotencyLocked(record.RequestID)
		if removed != nil {
			controller.restoreIdempotencyFrontLocked(*removed)
		}
		return controllerError(request.ID, ErrorCodeInternal)
	}
	return api.Response{Version: api.ProtocolVersion, ID: request.ID, Result: resultBytes}
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

func (controller *Controller) removeIdempotencyLocked(requestID string) {
	delete(controller.idempotency, requestID)
	for index, id := range controller.idempotencyOrder {
		if id == requestID {
			controller.idempotencyOrder = append(controller.idempotencyOrder[:index], controller.idempotencyOrder[index+1:]...)
			return
		}
	}
}

func (controller *Controller) restoreIdempotencyFrontLocked(record IdempotencyRecord) {
	controller.idempotency[record.RequestID] = cloneIdempotencyRecord(record)
	controller.idempotencyOrder = append([]string{record.RequestID}, controller.idempotencyOrder...)
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
		case ErrorCodeInvalidRequest, ErrorCodeInvalidConfig, ErrorCodeRevisionConflict, ErrorCodeCapacityExceeded, ErrorCodeDuplicate, ErrorCodeNotFound:
			return controllerError(id, codeErr.Code)
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return controllerError(id, "operation_timeout")
	}
	return controllerError(id, ErrorCodeInternal)
}

func controllerError(id, code string) api.Response {
	messages := map[string]string{
		ErrorCodeInvalidRequest:   "request parameters are invalid",
		ErrorCodeInvalidConfig:    "desired configuration is invalid",
		ErrorCodeRevisionConflict: "configuration revision does not match",
		ErrorCodeCapacityExceeded: "controller capacity is exceeded",
		ErrorCodeDuplicate:        "request ID is already used by different work",
		ErrorCodeNotFound:         "requested object was not found",
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
