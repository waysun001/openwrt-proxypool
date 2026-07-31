package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"proxypoold/internal/model"
)

// Store serializes all V2 configuration writes for one daemon instance.
type Store struct {
	path string
	ops  fsOps
	mu   sync.Mutex
}

type fsOps interface {
	CreateTemp(string, string) (tempFile, error)
	ReadFile(string) ([]byte, error)
	Remove(string) error
	Rename(string, string) error
	SyncDir(string) error
}

type tempFile interface {
	io.Writer
	Sync() error
	Close() error
	Chmod(os.FileMode) error
	Name() string
}

type osFS struct{}

func (osFS) CreateTemp(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) }
func (osFS) ReadFile(path string) ([]byte, error)             { return os.ReadFile(path) }
func (osFS) Remove(path string) error                         { return os.Remove(path) }
func (osFS) Rename(oldPath, newPath string) error             { return os.Rename(oldPath, newPath) }
func (osFS) SyncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func NewStore(path string) *Store            { return newStore(path, osFS{}) }
func newStore(path string, ops fsOps) *Store { return &Store{path: path, ops: ops} }

// Load reads and validates the current on-disk configuration.
func (s *Store) Load() (model.DesiredConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Replace persists next only if expectedRevision matches the on-disk revision.
func (s *Store) Replace(ctx context.Context, expectedRevision uint64, next model.DesiredConfig) (model.DesiredConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return model.DesiredConfig{}, err
	}
	current, err := s.loadLocked()
	if err != nil {
		return model.DesiredConfig{}, err
	}
	if current.Revision != expectedRevision {
		return model.DesiredConfig{}, revisionConflict()
	}
	if model.Validate(next) != nil {
		return model.DesiredConfig{}, invalidConfig()
	}
	for id, oldNode := range current.Nodes {
		if newNode, exists := next.Nodes[id]; exists && newNode.PolicyID != oldNode.PolicyID {
			return model.DesiredConfig{}, invalidConfig()
		}
	}
	next = withRevision(next, current.Revision+1)
	if err := contextError(ctx); err != nil {
		return model.DesiredConfig{}, err
	}

	dir := filepath.Dir(s.path)
	file, err := s.ops.CreateTemp(dir, ".proxypool-v2-*")
	if err != nil {
		return model.DesiredConfig{}, errors.New("configuration temporary file creation failed")
	}
	tempPath := file.Name()
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		_ = s.ops.Remove(tempPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		return model.DesiredConfig{}, errors.New("configuration temporary permission change failed")
	}
	if err := Encode(file, next); err != nil {
		return model.DesiredConfig{}, err
	}
	if err := contextError(ctx); err != nil {
		return model.DesiredConfig{}, err
	}
	if err := file.Sync(); err != nil {
		return model.DesiredConfig{}, errors.New("configuration temporary sync failed")
	}
	if err := file.Close(); err != nil {
		return model.DesiredConfig{}, errors.New("configuration temporary close failed")
	}
	closed = true
	if err := contextError(ctx); err != nil {
		return model.DesiredConfig{}, err
	}
	contents, err := s.ops.ReadFile(tempPath)
	if err != nil {
		return model.DesiredConfig{}, errors.New("configuration temporary validation failed")
	}
	if _, err := Decode(bytes.NewReader(contents)); err != nil {
		return model.DesiredConfig{}, invalidConfig()
	}
	if err := contextError(ctx); err != nil {
		return model.DesiredConfig{}, err
	}
	if err := s.ops.Rename(tempPath, s.path); err != nil {
		return model.DesiredConfig{}, errors.New("configuration atomic rename failed")
	}
	if err := s.ops.SyncDir(dir); err != nil {
		return model.DesiredConfig{}, errors.New("configuration directory sync failed")
	}
	return next, nil
}

func (s *Store) loadLocked() (model.DesiredConfig, error) {
	contents, err := s.ops.ReadFile(s.path)
	if err != nil {
		return model.DesiredConfig{}, errors.New("configuration read failed")
	}
	cfg, err := Decode(bytes.NewReader(contents))
	if err != nil {
		return model.DesiredConfig{}, err
	}
	return cfg, nil
}

func withRevision(cfg model.DesiredConfig, revision uint64) model.DesiredConfig {
	cfg.Revision = revision
	for id, node := range cfg.Nodes {
		node.Revision = revision
		cfg.Nodes[id] = node
	}
	return cfg
}
func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
func revisionConflict() error {
	return &model.CodeError{Code: "revision_conflict", Message: "configuration revision does not match"}
}
