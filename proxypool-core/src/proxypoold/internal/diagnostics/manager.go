package diagnostics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sync"
	"time"
)

const (
	DiagnosticQueued  = "queued"
	DiagnosticRunning = "running"
	DiagnosticReady   = "ready"
	DiagnosticFailed  = "failed"

	MaxConcurrentDiagnosticJobs = 2
	MaxRetainedDiagnosticJobs   = 32
)

var diagnosticJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var (
	ErrManagerUnavailable = errors.New("diagnostic manager is unavailable")
	ErrDiagnosticCapacity = errors.New("diagnostic capacity exceeded")
)

type Snapshot struct {
	Entries map[string][]byte
	Secrets []string
}

type SnapshotProvider func(context.Context) (Snapshot, error)
type BundleBuilder func(context.Context, Snapshot) ([]Entry, error)

type DiagnosticStatus struct {
	ID        string    `json:"job_id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ErrorCode string    `json:"error_code,omitempty"`
	Artifact  *Artifact `json:"artifact,omitempty"`
}

type ManagerOption func(*Manager)

func WithManagerIDSource(source func() string) ManagerOption {
	return func(manager *Manager) {
		if source != nil {
			manager.newID = source
		}
	}
}

type Manager struct {
	mu         sync.RWMutex
	ctx        context.Context
	store      *ArtifactStore
	provider   SnapshotProvider
	build      BundleBuilder
	newID      func() string
	now        func() time.Time
	jobs       map[string]DiagnosticStatus
	order      []string
	running    int
	workers    sync.WaitGroup
	done       chan struct{}
	stopped    bool
	cleanupErr error
}

func NewManager(ctx context.Context, store *ArtifactStore, provider SnapshotProvider, build BundleBuilder, options ...ManagerOption) *Manager {
	manager := &Manager{
		ctx: ctx, store: store, provider: provider, build: build, newID: randomDiagnosticJobID,
		now: time.Now, jobs: make(map[string]DiagnosticStatus),
		done: make(chan struct{}),
	}
	for _, option := range options {
		if option != nil {
			option(manager)
		}
	}
	if ctx != nil && store != nil {
		go manager.cleanupLoop()
	} else {
		close(manager.done)
	}
	return manager
}

func (manager *Manager) cleanupLoop() {
	defer close(manager.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-manager.ctx.Done():
			manager.mu.Lock()
			manager.stopped = true
			manager.mu.Unlock()
			manager.workers.Wait()
			cleanupErr := manager.store.CleanupAll()
			manager.mu.Lock()
			manager.cleanupErr = cleanupErr
			manager.mu.Unlock()
			return
		case <-ticker.C:
			_ = manager.store.CleanupExpired()
		}
	}
}

func (manager *Manager) Create() (DiagnosticStatus, error) {
	if manager == nil || manager.ctx == nil || manager.store == nil || manager.provider == nil || manager.newID == nil || manager.now == nil {
		return DiagnosticStatus{}, ErrManagerUnavailable
	}
	if err := manager.ctx.Err(); err != nil {
		return DiagnosticStatus{}, errors.New("diagnostic manager is stopped")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.stopped || manager.ctx.Err() != nil {
		return DiagnosticStatus{}, errors.New("diagnostic manager is stopped")
	}
	manager.pruneLocked()
	if manager.running >= MaxConcurrentDiagnosticJobs {
		return DiagnosticStatus{}, ErrDiagnosticCapacity
	}
	id := manager.newID()
	if !diagnosticJobIDPattern.MatchString(id) {
		return DiagnosticStatus{}, errors.New("diagnostic job id is invalid")
	}
	if _, exists := manager.jobs[id]; exists {
		return DiagnosticStatus{}, errors.New("diagnostic job id is duplicated")
	}
	now := manager.now().UTC()
	status := DiagnosticStatus{ID: id, State: DiagnosticQueued, CreatedAt: now, UpdatedAt: now}
	if len(manager.order) >= MaxRetainedDiagnosticJobs {
		return DiagnosticStatus{}, ErrDiagnosticCapacity
	}
	manager.jobs[id] = status
	manager.order = append(manager.order, id)
	manager.running++
	manager.workers.Add(1)
	go manager.run(id)
	return cloneDiagnosticStatus(status), nil
}

func (manager *Manager) Get(id string) (DiagnosticStatus, bool) {
	if manager == nil || !diagnosticJobIDPattern.MatchString(id) {
		return DiagnosticStatus{}, false
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	status, exists := manager.jobs[id]
	return cloneDiagnosticStatus(status), exists
}

func (manager *Manager) Claim(artifactID string) (ArtifactClaim, error) {
	if manager == nil || manager.store == nil {
		return ArtifactClaim{}, ErrArtifactNotFound
	}
	return manager.store.Claim(artifactID)
}

func (manager *Manager) Release(artifactID string) error {
	if manager == nil || manager.store == nil {
		return ErrArtifactNotFound
	}
	return manager.store.Release(artifactID)
}

func (manager *Manager) run(id string) {
	defer manager.workers.Done()
	manager.update(id, DiagnosticRunning, "", nil, false)
	snapshot, err := manager.provider(manager.ctx)
	if err != nil {
		manager.fail(id, err)
		return
	}
	if manager.build == nil {
		manager.update(id, DiagnosticFailed, "collection_failed", nil, true)
		return
	}
	entries, err := manager.build(manager.ctx, snapshot)
	if err != nil {
		manager.fail(id, err)
		return
	}
	artifact, err := manager.store.Write(manager.ctx, entries)
	if err != nil {
		manager.fail(id, err)
		return
	}
	manager.update(id, DiagnosticReady, "", &artifact, true)
}

func (manager *Manager) Wait() error {
	if manager == nil || manager.done == nil {
		return nil
	}
	<-manager.done
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.cleanupErr
}

func (manager *Manager) fail(id string, cause error) {
	code := "collection_failed"
	if manager.ctx.Err() != nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		code = "collection_cancelled"
	}
	manager.update(id, DiagnosticFailed, code, nil, true)
}

func (manager *Manager) update(id, state, errorCode string, artifact *Artifact, finished bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	status, exists := manager.jobs[id]
	if !exists {
		return
	}
	status.State, status.ErrorCode, status.UpdatedAt = state, errorCode, manager.now().UTC()
	if artifact != nil {
		copy := *artifact
		status.Artifact = &copy
	}
	manager.jobs[id] = status
	if finished && manager.running > 0 {
		manager.running--
	}
}

func (manager *Manager) pruneLocked() {
	for len(manager.order) >= MaxRetainedDiagnosticJobs {
		removed := false
		for index, id := range manager.order {
			status := manager.jobs[id]
			if status.State == DiagnosticQueued || status.State == DiagnosticRunning {
				continue
			}
			manager.order = append(manager.order[:index], manager.order[index+1:]...)
			delete(manager.jobs, id)
			removed = true
			break
		}
		if !removed {
			return
		}
	}
}

func cloneDiagnosticStatus(status DiagnosticStatus) DiagnosticStatus {
	if status.Artifact != nil {
		artifact := *status.Artifact
		status.Artifact = &artifact
	}
	return status
}

func randomDiagnosticJobID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	return "diagnostic-" + hex.EncodeToString(value[:])
}
