package importer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"proxypoold/internal/model"
)

const (
	PreviewLifetime       = 10 * time.Minute
	MaxConcurrentPreviews = 16

	ErrorPreviewNotFound  = "preview_not_found"
	ErrorPreviewMismatch  = "preview_mismatch"
	ErrorPreviewBlocked   = "preview_blocked"
	ErrorPreviewExpired   = "preview_expired"
	ErrorPreviewCapacity  = "preview_capacity"
	ErrorRevisionConflict = "revision_conflict"
)

type PreviewRequest struct {
	Protocol model.Protocol
	Raw      string
	Base     model.DesiredConfig
}

func (request PreviewRequest) String() string {
	return fmt.Sprintf("importer.PreviewRequest{Protocol:%q Raw:<redacted> BaseRevision:%d}", request.Protocol, request.Base.Revision)
}

func (request PreviewRequest) GoString() string { return request.String() }

type CommitRequest struct {
	PreviewID        string
	PreviewHash      string
	ExpectedRevision uint64
}

type PreviewRow struct {
	SanitizedRow
	Action string `json:"action"`
}

type Preview struct {
	ID           string       `json:"preview_id"`
	Hash         string       `json:"preview_hash"`
	BaseRevision uint64       `json:"base_revision"`
	ExpiresAt    time.Time    `json:"expires_at"`
	Blocked      bool         `json:"blocked"`
	Added        int          `json:"added"`
	Skipped      int          `json:"skipped"`
	Rows         []PreviewRow `json:"rows"`
	Errors       []LineError  `json:"errors,omitempty"`
}

func (preview Preview) String() string {
	return fmt.Sprintf("importer.Preview{ID:%q BaseRevision:%d Blocked:%t Added:%d Skipped:%d Rows:%d Errors:%d}",
		preview.ID, preview.BaseRevision, preview.Blocked, preview.Added, preview.Skipped, len(preview.Rows), len(preview.Errors))
}

type Option func(*Manager)

func WithClock(clock func() time.Time) Option {
	return func(manager *Manager) {
		if clock != nil {
			manager.now = clock
		}
	}
}

func WithIDSource(source func() string) Option {
	return func(manager *Manager) {
		if source != nil {
			manager.newID = source
		}
	}
}

type previewEntry struct {
	preview Preview
	nodes   []Candidate
}

type Manager struct {
	mu       sync.Mutex
	now      func() time.Time
	newID    func() string
	previews map[string]previewEntry
}

func New(options ...Option) *Manager {
	manager := &Manager{now: time.Now, newID: randomPreviewID, previews: make(map[string]previewEntry)}
	for _, option := range options {
		if option != nil {
			option(manager)
		}
	}
	return manager
}

func (manager *Manager) Preview(ctx context.Context, request PreviewRequest) (Preview, error) {
	if manager == nil || manager.now == nil || manager.newID == nil || request.Base.Revision == 0 {
		return Preview{}, codeError(ErrorInvalidFields, "预览请求无效")
	}
	if err := ctx.Err(); err != nil {
		return Preview{}, err
	}
	parsed := Parse(request.Protocol, request.Raw)
	existing := make(map[string]struct{}, len(request.Base.Nodes))
	for _, node := range request.Base.Nodes {
		existing[naturalKey(candidateFromNode(node))] = struct{}{}
	}

	accepted := make([]Candidate, 0, len(parsed.Nodes))
	rows := make([]PreviewRow, 0, len(parsed.Nodes))
	added, skipped := 0, 0
	for _, candidate := range parsed.Nodes {
		action := "add"
		if _, exists := existing[naturalKey(candidate)]; exists {
			action = "skip"
			skipped++
		} else {
			accepted = append(accepted, candidate)
			added++
		}
		row := SanitizedRows(ParseResult{Nodes: []Candidate{candidate}})[0]
		rows = append(rows, PreviewRow{SanitizedRow: row, Action: action})
	}
	errorsList := append([]LineError(nil), parsed.Errors...)
	if len(request.Base.Nodes)+added > MaxImportRecords {
		errorsList = append(errorsList, lineError(0, ErrorCapacityExceeded, "导入后节点总数不能超过 60"))
		accepted = nil
	}

	now := manager.now().UTC()
	hash := previewHash(request.Raw, request.Protocol, request.Base.Revision)
	preview := Preview{
		Hash: hash, BaseRevision: request.Base.Revision, ExpiresAt: now.Add(PreviewLifetime),
		Blocked: len(errorsList) > 0, Added: added, Skipped: skipped, Rows: rows, Errors: errorsList,
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.removeExpiredLocked(now)
	if len(manager.previews) >= MaxConcurrentPreviews {
		return Preview{}, codeError(ErrorPreviewCapacity, "待提交预览数量已达上限")
	}
	preview.ID = manager.newID()
	if preview.ID == "" {
		return Preview{}, codeError(ErrorInvalidFields, "无法生成预览 ID")
	}
	if _, exists := manager.previews[preview.ID]; exists {
		return Preview{}, codeError(ErrorPreviewCapacity, "预览 ID 冲突")
	}
	manager.previews[preview.ID] = previewEntry{preview: preview, nodes: cloneCandidates(accepted)}
	return preview, nil
}

func (manager *Manager) Commit(ctx context.Context, request CommitRequest) ([]Candidate, error) {
	nodes, err := manager.ValidateCommit(ctx, request)
	if err != nil {
		return nil, err
	}
	manager.Consume(request.PreviewID)
	return nodes, nil
}

func (manager *Manager) ValidateCommit(ctx context.Context, request CommitRequest) ([]Candidate, error) {
	if manager == nil || request.PreviewID == "" || request.PreviewHash == "" || request.ExpectedRevision == 0 {
		return nil, codeError(ErrorInvalidFields, "提交请求无效")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entry, exists := manager.previews[request.PreviewID]
	if !exists {
		return nil, codeError(ErrorPreviewNotFound, "预览不存在")
	}
	if manager.now().UTC().After(entry.preview.ExpiresAt) {
		delete(manager.previews, request.PreviewID)
		return nil, codeError(ErrorPreviewExpired, "预览已过期")
	}
	if request.PreviewHash != entry.preview.Hash {
		return nil, codeError(ErrorPreviewMismatch, "预览校验值不匹配")
	}
	if request.ExpectedRevision != entry.preview.BaseRevision {
		return nil, codeError(ErrorRevisionConflict, "配置版本已变化")
	}
	if entry.preview.Blocked {
		return nil, codeError(ErrorPreviewBlocked, "预览包含阻断错误")
	}
	return cloneCandidates(entry.nodes), nil
}

func (manager *Manager) Consume(previewID string) {
	if manager == nil || previewID == "" {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	delete(manager.previews, previewID)
}

func Merge(base model.DesiredConfig, candidates []Candidate) (model.DesiredConfig, []string, error) {
	next := cloneDesired(base)
	if len(next.Nodes)+len(candidates) > MaxImportRecords {
		return model.DesiredConfig{}, nil, codeError(ErrorCapacityExceeded, "导入后节点总数不能超过 60")
	}
	usedPolicies := make(map[uint16]struct{}, len(next.Nodes))
	usedNames := make(map[string]struct{}, len(next.Nodes))
	for _, node := range next.Nodes {
		usedPolicies[node.PolicyID] = struct{}{}
		usedNames[strings.ToLower(strings.TrimSpace(node.Name))] = struct{}{}
	}
	nodeIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		policy := firstPolicyID(usedPolicies)
		if policy == 0 {
			return model.DesiredConfig{}, nil, codeError(ErrorCapacityExceeded, "没有可用的节点策略编号")
		}
		usedPolicies[policy] = struct{}{}
		id := stableNodeID(candidate, next.Nodes)
		name := uniqueNodeName(candidate, policy, usedNames)
		usedNames[strings.ToLower(name)] = struct{}{}
		node := model.Node{
			ID: id, Name: name, Protocol: candidate.Protocol, Enabled: candidate.Protocol == model.ProtocolL2TP,
			Server: candidate.Server, Port: candidate.Port, Username: candidate.Username, Password: candidate.Password,
			SLPToken: candidate.SLPToken, SLPTransport: candidate.SLPTransport, PolicyID: policy,
		}
		if candidate.ExpiresAt != nil {
			expiry := *candidate.ExpiresAt
			node.ExpiresAt = &expiry
		}
		next.Nodes[id] = node
		if node.Enabled {
			nodeIDs = append(nodeIDs, id)
		}
	}
	if err := model.Validate(next); err != nil {
		return model.DesiredConfig{}, nil, err
	}
	return next, nodeIDs, nil
}

func (manager *Manager) removeExpiredLocked(now time.Time) {
	for id, entry := range manager.previews {
		if now.After(entry.preview.ExpiresAt) {
			delete(manager.previews, id)
		}
	}
}

func candidateFromNode(node model.Node) Candidate {
	return Candidate{Protocol: node.Protocol, Server: normalizeServer(node.Server), Port: node.Port, Username: node.Username, Password: node.Password, SLPToken: node.SLPToken, SLPTransport: node.SLPTransport}
}

func cloneCandidates(candidates []Candidate) []Candidate {
	cloned := append([]Candidate(nil), candidates...)
	for index := range cloned {
		if cloned[index].ExpiresAt != nil {
			expiry := *cloned[index].ExpiresAt
			cloned[index].ExpiresAt = &expiry
		}
	}
	return cloned
}

func cloneDesired(config model.DesiredConfig) model.DesiredConfig {
	clone := config
	clone.Global.ManagementPorts = append([]uint16(nil), config.Global.ManagementPorts...)
	clone.Global.DoHEndpoints = append([]model.DoHEndpoint(nil), config.Global.DoHEndpoints...)
	clone.Nodes = make(map[string]model.Node, len(config.Nodes)+1)
	for id, node := range config.Nodes {
		if node.ExpiresAt != nil {
			expiry := *node.ExpiresAt
			node.ExpiresAt = &expiry
		}
		clone.Nodes[id] = node
	}
	clone.Devices = make(map[string]model.Device, len(config.Devices))
	for id, device := range config.Devices {
		clone.Devices[id] = device
	}
	clone.PendingBindings = make(map[string]model.PendingBinding, len(config.PendingBindings))
	for id, binding := range config.PendingBindings {
		clone.PendingBindings[id] = binding
	}
	return clone
}

func firstPolicyID(used map[uint16]struct{}) uint16 {
	for value := uint16(1); value <= MaxImportRecords; value++ {
		if _, exists := used[value]; !exists {
			return value
		}
	}
	return 0
}

func stableNodeID(candidate Candidate, existing map[string]model.Node) string {
	digest := sha256.Sum256([]byte(naturalKey(candidate)))
	base := "node_" + hex.EncodeToString(digest[:6])
	if _, exists := existing[base]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidateID := base + "_" + strconv.Itoa(suffix)
		if _, exists := existing[candidateID]; !exists {
			return candidateID
		}
	}
}

func uniqueNodeName(candidate Candidate, policy uint16, used map[string]struct{}) string {
	base := strings.ToUpper(string(candidate.Protocol)) + " " + candidate.Server + ":" + strconv.Itoa(int(candidate.Port))
	if _, exists := used[strings.ToLower(base)]; !exists {
		return base
	}
	return base + " #" + strconv.Itoa(int(policy))
}

func previewHash(raw string, protocol model.Protocol, revision uint64) string {
	digest := sha256.Sum256([]byte(raw + "\x00" + string(protocol) + "\x00" + strconv.FormatUint(revision, 10)))
	return hex.EncodeToString(digest[:])
}

func randomPreviewID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return ""
	}
	return hex.EncodeToString(buffer)
}

func codeError(code, message string) error { return &model.CodeError{Code: code, Message: message} }
