package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"time"
)

const (
	RuntimeSnapshotSchemaVersion = 1
	MaxRuntimeNodeStatuses       = 60
	MaxRuntimeIdempotencyRecords = 256
	maxRuntimeSnapshotBytes      = 4 << 20
)

var ErrRuntimeSnapshotNotFound = errors.New("runtime snapshot not found")

// RuntimeSnapshot contains only restart metadata. Desired node configuration
// and credentials remain exclusively in the root-owned configuration store.
type RuntimeSnapshot struct {
	SchemaVersion  int                 `json:"schema_version"`
	ConfigRevision uint64              `json:"config_revision"`
	Jobs           JobSnapshot         `json:"jobs"`
	NodeStatuses   []NodeStatus        `json:"node_statuses"`
	Idempotency    []IdempotencyRecord `json:"idempotency,omitempty"`
}

// IdempotencyRecord persists only a successful mutation's safe public result.
// Digest is the SHA-256 digest of its method-specific canonical parameters.
type IdempotencyRecord struct {
	RequestID      string          `json:"request_id"`
	Method         string          `json:"method"`
	Digest         string          `json:"digest"`
	Result         json.RawMessage `json:"result"`
	ConfigRevision uint64          `json:"config_revision"`
	CreatedAt      time.Time       `json:"created_at"`
}

// RuntimeStore serializes all runtime snapshot writes for one daemon process.
type RuntimeStore struct {
	path string
	ops  runtimeFS
	mu   sync.Mutex
}

type runtimeFS interface {
	CreateTemp(string, string) (runtimeTempFile, error)
	ReadFile(string) ([]byte, error)
	Lstat(string) (os.FileInfo, error)
	Remove(string) error
	Rename(string, string) error
	SyncDir(string) error
}

type runtimeTempFile interface {
	io.Writer
	Sync() error
	Close() error
	Chmod(os.FileMode) error
	Name() string
}

type osRuntimeFS struct{}

func (osRuntimeFS) CreateTemp(dir, pattern string) (runtimeTempFile, error) {
	return os.CreateTemp(dir, pattern)
}
func (osRuntimeFS) ReadFile(path string) ([]byte, error)   { return os.ReadFile(path) }
func (osRuntimeFS) Lstat(path string) (os.FileInfo, error) { return os.Lstat(path) }
func (osRuntimeFS) Remove(path string) error               { return os.Remove(path) }
func (osRuntimeFS) Rename(oldPath, newPath string) error   { return os.Rename(oldPath, newPath) }
func (osRuntimeFS) SyncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func NewRuntimeStore(path string) *RuntimeStore {
	return newRuntimeStore(path, osRuntimeFS{})
}

func newRuntimeStore(path string, ops runtimeFS) *RuntimeStore {
	return &RuntimeStore{path: path, ops: ops}
}

func (s *RuntimeStore) Load() (RuntimeSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := rejectRuntimeSymlink(s.ops, s.path); err != nil {
		return RuntimeSnapshot{}, err
	}
	contents, err := s.ops.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeSnapshot{}, ErrRuntimeSnapshotNotFound
	}
	if err != nil || len(contents) > maxRuntimeSnapshotBytes {
		return RuntimeSnapshot{}, errors.New("runtime snapshot read failed")
	}
	snapshot, err := decodeRuntimeSnapshot(contents)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	return snapshot, nil
}

func (s *RuntimeStore) Save(ctx context.Context, snapshot RuntimeSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := runtimeContextError(ctx); err != nil {
		return err
	}
	normalized, err := normalizeRuntimeSnapshot(snapshot)
	if err != nil {
		return err
	}
	if err := rejectRuntimeSymlink(s.ops, s.path); err != nil {
		return err
	}

	directory := filepath.Dir(s.path)
	file, err := s.ops.CreateTemp(directory, ".proxypool-runtime-*")
	if err != nil {
		return errors.New("runtime temporary file creation failed")
	}
	temporaryPath := file.Name()
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		_ = s.ops.Remove(temporaryPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.New("runtime temporary permission change failed")
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(normalized); err != nil {
		return errors.New("runtime snapshot encode failed")
	}
	if err := runtimeContextError(ctx); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return errors.New("runtime temporary sync failed")
	}
	if err := file.Close(); err != nil {
		return errors.New("runtime temporary close failed")
	}
	closed = true
	if err := runtimeContextError(ctx); err != nil {
		return err
	}
	contents, err := s.ops.ReadFile(temporaryPath)
	if err != nil || len(contents) > maxRuntimeSnapshotBytes {
		return errors.New("runtime temporary validation failed")
	}
	decoded, err := decodeRuntimeSnapshot(contents)
	if err != nil || !reflect.DeepEqual(decoded, normalized) {
		return errors.New("runtime temporary validation failed")
	}
	if err := runtimeContextError(ctx); err != nil {
		return err
	}
	if err := rejectRuntimeSymlink(s.ops, s.path); err != nil {
		return err
	}
	if err := s.ops.Rename(temporaryPath, s.path); err != nil {
		return errors.New("runtime atomic rename failed")
	}
	if err := s.ops.SyncDir(directory); err != nil {
		return errors.New("runtime directory sync failed")
	}
	return nil
}

func decodeRuntimeSnapshot(contents []byte) (RuntimeSnapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var snapshot RuntimeSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
	}
	return normalizeRuntimeSnapshot(snapshot)
}

func normalizeRuntimeSnapshot(snapshot RuntimeSnapshot) (RuntimeSnapshot, error) {
	if snapshot.SchemaVersion != RuntimeSnapshotSchemaVersion || len(snapshot.NodeStatuses) > MaxRuntimeNodeStatuses || len(snapshot.Idempotency) > MaxRuntimeIdempotencyRecords {
		return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
	}
	jobs, err := normalizeJobSnapshot(snapshot.Jobs)
	if err != nil {
		return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
	}
	normalized := RuntimeSnapshot{
		SchemaVersion:  snapshot.SchemaVersion,
		ConfigRevision: snapshot.ConfigRevision,
		Jobs:           jobs,
		NodeStatuses:   make([]NodeStatus, 0, len(snapshot.NodeStatuses)),
	}
	if len(snapshot.Idempotency) > 0 {
		normalized.Idempotency = make([]IdempotencyRecord, 0, len(snapshot.Idempotency))
	}
	seenNodes := make(map[string]struct{}, len(snapshot.NodeStatuses))
	for _, status := range snapshot.NodeStatuses {
		if status.NodeID == "" || status.JobID == "" || status.Generation == 0 || status.UpdatedAt.IsZero() || !validRuntimeState(status.State) {
			return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
		}
		if _, exists := seenNodes[status.NodeID]; exists {
			return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
		}
		seenNodes[status.NodeID] = struct{}{}
		status = cloneNodeStatus(status)
		status.RetryAt = canonicalRuntimeTime(status.RetryAt)
		status.UpdatedAt = canonicalRuntimeTime(status.UpdatedAt)
		normalized.NodeStatuses = append(normalized.NodeStatuses, status)
	}
	seenRequests := make(map[string]struct{}, len(snapshot.Idempotency))
	for _, record := range snapshot.Idempotency {
		if record.RequestID == "" || record.Method == "" || len(record.Digest) != 64 || !json.Valid(record.Result) || record.CreatedAt.IsZero() {
			return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
		}
		if _, exists := seenRequests[record.RequestID]; exists {
			return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
		}
		seenRequests[record.RequestID] = struct{}{}
		record.Result = append(json.RawMessage(nil), record.Result...)
		record.CreatedAt = canonicalRuntimeTime(record.CreatedAt)
		normalized.Idempotency = append(normalized.Idempotency, record)
	}
	return normalized, nil
}

func canonicalRuntimeTime(value time.Time) time.Time {
	return value.UTC().Round(0)
}

func rejectRuntimeSymlink(ops runtimeFS, path string) error {
	info, err := ops.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("runtime snapshot path is unsafe")
	}
	return nil
}

func runtimeContextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
