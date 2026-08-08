package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/config"
	"proxypoold/internal/model"
)

// PlatformMutator is the future dataplane boundary. Phase 1 retains a caller-
// supplied fail-on-call implementation for safety testing but never invokes it
// and does not provide a command-executing production implementation.
type PlatformMutator interface {
	Mutate(context.Context, string) error
}

type ConfigState string

const (
	ConfigStateReady             ConfigState = "ready"
	ConfigStateMigrationRequired ConfigState = "migration_required"
	ConfigStateInvalid           ConfigState = "invalid_config"
)

type StatusError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ConfigSummary struct {
	State         ConfigState  `json:"state"`
	SchemaVersion int          `json:"schema_version,omitempty"`
	Revision      uint64       `json:"revision,omitempty"`
	Error         *StatusError `json:"error,omitempty"`
}

type DesiredSummary struct {
	Enabled         bool                          `json:"enabled"`
	Nodes           []DesiredNodeSummary          `json:"nodes"`
	Devices         []DesiredDeviceStatus         `json:"devices"`
	PendingBindings []DesiredPendingBindingStatus `json:"pending_bindings,omitempty"`
}

// DesiredNodeSummary exposes fields needed to edit a node while returning only
// credential-presence booleans, never usernames, passwords, tokens or keys.
type DesiredNodeSummary struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Note          string         `json:"note"`
	Protocol      model.Protocol `json:"protocol"`
	Enabled       bool           `json:"enabled"`
	DeletePending bool           `json:"delete_pending"`
	Server        string         `json:"server"`
	Port          uint16         `json:"port"`
	HasUsername   bool           `json:"has_username"`
	HasPassword   bool           `json:"has_password"`
	ExpiresAt     *time.Time     `json:"expires_at,omitempty"`
	PolicyID      uint16         `json:"policy_id"`
	Revision      uint64         `json:"revision"`
}

type DesiredDeviceStatus struct {
	ID        string `json:"id"`
	MAC       string `json:"mac"`
	Hostname  string `json:"hostname"`
	FixedIPv4 string `json:"fixed_ipv4"`
	NodeID    string `json:"node_id,omitempty"`
	Enabled   bool   `json:"enabled"`
}

type DesiredPendingBindingStatus struct {
	ID         string `json:"id"`
	LegacyIPv4 string `json:"legacy_ipv4"`
	NodeID     string `json:"node_id"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type RuntimeSummary struct {
	Nodes []RuntimeNodeSummary `json:"nodes"`
}

type RuntimeNodeSummary struct {
	NodeID    string             `json:"node_id"`
	JobID     string             `json:"job_id,omitempty"`
	State     model.RuntimeState `json:"state"`
	Attempts  uint64             `json:"attempts"`
	LastError *PublicError       `json:"last_error,omitempty"`
	RetryAt   *time.Time         `json:"retry_at,omitempty"`
	Traffic   TrafficSnapshot    `json:"traffic"`
}

type ReconciliationSummary struct {
	ID             string    `json:"id,omitempty"`
	Kind           string    `json:"kind,omitempty"`
	Creator        string    `json:"creator,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	ConfigRevision uint64    `json:"config_revision,omitempty"`
	State          JobState  `json:"state,omitempty"`
	Total          int       `json:"total"`
	Succeeded      int       `json:"succeeded"`
}

// ShadowStatus is a dedicated control-plane DTO. It never embeds the desired
// model, parser errors, adapters, requests, or job internals.
type ShadowStatus struct {
	Mode           string                `json:"mode"`
	Config         ConfigSummary         `json:"config"`
	Desired        DesiredSummary        `json:"desired"`
	Runtime        RuntimeSummary        `json:"runtime"`
	Reconciliation ReconciliationSummary `json:"reconciliation"`
}

func (status ShadowStatus) String() string {
	errorCode := ""
	if status.Config.Error != nil {
		errorCode = status.Config.Error.Code
	}
	return fmt.Sprintf("engine.ShadowStatus{Mode:%q ConfigState:%q ErrorCode:%q DesiredNodes:%d DesiredDevices:%d RuntimeNodes:%d ReconciliationID:%q ReconciliationState:%q}", status.Mode, status.Config.State, errorCode, len(status.Desired.Nodes), len(status.Desired.Devices), len(status.Runtime.Nodes), status.Reconciliation.ID, status.Reconciliation.State)
}

func (status ShadowStatus) GoString() string { return status.String() }

func (status ShadowStatus) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, status.String())
}

type ShadowOption func(*Shadow)

func WithShadowClock(clock func() time.Time) ShadowOption {
	return func(shadow *Shadow) {
		if clock != nil {
			shadow.now = clock
		}
	}
}

func WithJobIDSource(source func() string) ShadowOption {
	return func(shadow *Shadow) {
		if source != nil {
			shadow.newJobID = source
		}
	}
}

// Shadow owns read-only Phase 1 state. mutationBoundary is deliberately never
// called; retaining it makes accidental future mutation observable in tests.
type Shadow struct {
	configPath       string
	mutationBoundary PlatformMutator
	now              func() time.Time
	newJobID         func() string

	mu     sync.RWMutex
	status ShadowStatus
}

func NewShadow(configPath string, mutationBoundary PlatformMutator, options ...ShadowOption) *Shadow {
	shadow := &Shadow{
		configPath:       configPath,
		mutationBoundary: mutationBoundary,
		now:              time.Now,
		newJobID:         randomReconciliationID,
	}
	for _, option := range options {
		if option != nil {
			option(shadow)
		}
	}
	return shadow
}

// Start rebuilds process-local observation and reconciliation state. It only
// reads the UCI file and never touches platform or persistent configuration.
func (shadow *Shadow) Start() {
	inspection := config.InspectFile(shadow.configPath)
	status := ShadowStatus{
		Mode:    "v2_shadow",
		Desired: DesiredSummary{Nodes: []DesiredNodeSummary{}, Devices: []DesiredDeviceStatus{}},
		Runtime: RuntimeSummary{Nodes: []RuntimeNodeSummary{}},
	}
	switch inspection.State() {
	case config.ConfigReady:
		desired, ok := inspection.Desired()
		if !ok {
			status.Config = invalidConfigSummary()
			break
		}
		status.Config = ConfigSummary{State: ConfigStateReady, SchemaVersion: desired.SchemaVersion, Revision: desired.Revision}
		status.Desired, status.Runtime = summarizeDesired(desired)
		job := newShadowReconciliation(desired, shadow.newJobID(), shadow.now())
		status.Reconciliation = summarizeReconciliation(job)
	case config.ConfigMigrationRequired:
		status.Config = ConfigSummary{State: ConfigStateMigrationRequired, Error: &StatusError{Code: "migration_required", Message: "legacy configuration requires migration"}}
	default:
		status.Config = invalidConfigSummary()
	}

	shadow.mu.Lock()
	shadow.status = status
	shadow.mu.Unlock()
}

// Run is useful for workers that share the daemon cancellation context.
func (shadow *Shadow) Run(ctx context.Context) error {
	shadow.Start()
	<-ctx.Done()
	return nil
}

func (shadow *Shadow) Status() ShadowStatus {
	shadow.mu.RLock()
	defer shadow.mu.RUnlock()
	return cloneShadowStatus(shadow.status)
}

// Handle implements only the Phase 1 read method, even when called without the
// API server's method allowlist.
func (shadow *Shadow) Handle(ctx context.Context, request api.Request) api.Response {
	if request.Method != "status.get" {
		return api.Response{Version: api.ProtocolVersion, ID: request.ID, Error: &api.Error{Code: "unknown_method", Message: "unknown control method"}}
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(request.Params, &params); err != nil || params == nil || len(params) != 0 {
		return api.Response{Version: api.ProtocolVersion, ID: request.ID, Error: &api.Error{Code: "invalid_request", Message: "status.get requires empty parameters"}}
	}
	select {
	case <-ctx.Done():
		return api.Response{Version: api.ProtocolVersion, ID: request.ID, Error: &api.Error{Code: "operation_timeout", Message: "operation timed out"}}
	default:
	}
	encoded, err := json.Marshal(shadow.Status())
	if err != nil {
		return api.Response{Version: api.ProtocolVersion, ID: request.ID, Error: &api.Error{Code: "internal_error", Message: "internal control error"}}
	}
	return api.Response{Version: api.ProtocolVersion, ID: request.ID, Result: encoded}
}

func invalidConfigSummary() ConfigSummary {
	return ConfigSummary{State: ConfigStateInvalid, Error: &StatusError{Code: "invalid_config", Message: "configuration is invalid"}}
}

func summarizeDesired(desired model.DesiredConfig) (DesiredSummary, RuntimeSummary) {
	summary := DesiredSummary{Enabled: desired.Global.Enabled, Nodes: make([]DesiredNodeSummary, 0, len(desired.Nodes)), Devices: make([]DesiredDeviceStatus, 0, len(desired.Devices))}
	runtimeSummary := RuntimeSummary{Nodes: make([]RuntimeNodeSummary, 0, len(desired.Nodes))}
	nodeIDs := make([]string, 0, len(desired.Nodes))
	for id := range desired.Nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	for _, id := range nodeIDs {
		node := desired.Nodes[id]
		summary.Nodes = append(summary.Nodes, DesiredNodeSummary{
			ID: node.ID, Name: node.Name, Note: node.Note, Protocol: node.Protocol, Enabled: node.Enabled,
			DeletePending: node.DeletePending, Server: node.Server, Port: node.Port,
			HasUsername: node.Username != "", HasPassword: node.Password != "", ExpiresAt: node.ExpiresAt,
			PolicyID: node.PolicyID, Revision: node.Revision,
		})
		runtimeSummary.Nodes = append(runtimeSummary.Nodes, RuntimeNodeSummary{NodeID: node.ID, State: model.StateDisabled})
	}
	deviceIDs := make([]string, 0, len(desired.Devices))
	for id := range desired.Devices {
		deviceIDs = append(deviceIDs, id)
	}
	sort.Strings(deviceIDs)
	for _, id := range deviceIDs {
		device := desired.Devices[id]
		summary.Devices = append(summary.Devices, DesiredDeviceStatus{ID: device.ID, MAC: device.MAC, Hostname: device.Hostname, FixedIPv4: device.FixedIPv4.String(), NodeID: device.NodeID, Enabled: device.Enabled})
	}
	pendingIDs := make([]string, 0, len(desired.PendingBindings))
	for id := range desired.PendingBindings {
		pendingIDs = append(pendingIDs, id)
	}
	sort.Strings(pendingIDs)
	if len(pendingIDs) > 0 {
		summary.PendingBindings = make([]DesiredPendingBindingStatus, 0, len(pendingIDs))
	}
	for _, id := range pendingIDs {
		pending := desired.PendingBindings[id]
		summary.PendingBindings = append(summary.PendingBindings, DesiredPendingBindingStatus{
			ID: pending.ID, LegacyIPv4: pending.LegacyIPv4.String(), NodeID: pending.NodeID, ErrorCode: pending.ErrorCode,
		})
	}
	return summary, runtimeSummary
}

func newShadowReconciliation(desired model.DesiredConfig, id string, createdAt time.Time) Job {
	job := Job{
		ID:             id,
		Kind:           "reconciliation",
		Creator:        "system",
		CreatedAt:      createdAt,
		ConfigRevision: desired.Revision,
		State:          JobSucceeded,
		Total:          len(desired.Nodes),
		Succeeded:      len(desired.Nodes),
		Nodes:          make([]NodeProgress, 0, len(desired.Nodes)),
	}
	ids := make([]string, 0, len(desired.Nodes))
	for id := range desired.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, nodeID := range ids {
		job.Nodes = append(job.Nodes, NodeProgress{NodeID: nodeID, Step: "shadow_observed", State: model.StateDisabled})
	}
	return job
}

func summarizeReconciliation(job Job) ReconciliationSummary {
	return ReconciliationSummary{ID: job.ID, Kind: job.Kind, Creator: job.Creator, CreatedAt: job.CreatedAt, ConfigRevision: job.ConfigRevision, State: job.State, Total: job.Total, Succeeded: job.Succeeded}
}

func cloneShadowStatus(status ShadowStatus) ShadowStatus {
	status.Desired.Nodes = append([]DesiredNodeSummary(nil), status.Desired.Nodes...)
	for index := range status.Desired.Nodes {
		if status.Desired.Nodes[index].ExpiresAt != nil {
			expires := *status.Desired.Nodes[index].ExpiresAt
			status.Desired.Nodes[index].ExpiresAt = &expires
		}
	}
	status.Desired.Devices = append([]DesiredDeviceStatus(nil), status.Desired.Devices...)
	status.Runtime.Nodes = append([]RuntimeNodeSummary(nil), status.Runtime.Nodes...)
	for index := range status.Runtime.Nodes {
		status.Runtime.Nodes[index].LastError = clonePublicError(status.Runtime.Nodes[index].LastError)
		if status.Runtime.Nodes[index].RetryAt != nil {
			retryAt := *status.Runtime.Nodes[index].RetryAt
			status.Runtime.Nodes[index].RetryAt = &retryAt
		}
	}
	if status.Config.Error != nil {
		copy := *status.Config.Error
		status.Config.Error = &copy
	}
	return status
}

var fallbackJobSequence atomic.Uint64

func randomReconciliationID() string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err == nil {
		return "reconcile-" + hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("reconcile-%d-%d", time.Now().UnixNano(), fallbackJobSequence.Add(1))
}
