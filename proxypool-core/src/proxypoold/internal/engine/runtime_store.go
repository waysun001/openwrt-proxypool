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
)

const (
	RuntimeSnapshotSchemaVersion = 1
	MaxRuntimeNodeStatuses       = 60
	maxRuntimeSnapshotBytes      = 4 << 20
)

// RuntimeSnapshot contains only restart metadata. Desired node configuration
// and credentials remain exclusively in the root-owned configuration store.
type RuntimeSnapshot struct {
	SchemaVersion  int          `json:"schema_version"`
	ConfigRevision uint64       `json:"config_revision"`
	Jobs           JobSnapshot  `json:"jobs"`
	NodeStatuses   []NodeStatus `json:"node_statuses"`
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
	if snapshot.SchemaVersion != RuntimeSnapshotSchemaVersion || len(snapshot.NodeStatuses) > MaxRuntimeNodeStatuses {
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
	seenNodes := make(map[string]struct{}, len(snapshot.NodeStatuses))
	for _, status := range snapshot.NodeStatuses {
		if status.NodeID == "" || status.JobID == "" || status.Generation == 0 || status.UpdatedAt.IsZero() || !validRuntimeState(status.State) {
			return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
		}
		if _, exists := seenNodes[status.NodeID]; exists {
			return RuntimeSnapshot{}, errors.New("runtime snapshot is invalid")
		}
		seenNodes[status.NodeID] = struct{}{}
		normalized.NodeStatuses = append(normalized.NodeStatuses, cloneNodeStatus(status))
	}
	return normalized, nil
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
