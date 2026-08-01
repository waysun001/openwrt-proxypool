package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

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

// EnsureDurable retries the directory sync required after an atomic rename.
// It lets callers distinguish a visible replacement from a crash-durable one
// after Replace reports an ambiguous post-rename error.
func (s *Store) EnsureDurable(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := s.ops.SyncDir(filepath.Dir(s.path)); err != nil {
		return errors.New("configuration directory sync failed")
	}
	return contextError(ctx)
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
	if current.Revision == math.MaxUint64 || validateCodecConfig(next) != nil {
		return model.DesiredConfig{}, invalidConfig()
	}
	for id, oldNode := range current.Nodes {
		if newNode, exists := next.Nodes[id]; exists && newNode.PolicyID != oldNode.PolicyID {
			return model.DesiredConfig{}, invalidConfig()
		}
	}
	next = withRevision(current, cloneConfig(next), current.Revision+1)
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
	decoded, err := Decode(bytes.NewReader(contents))
	if err != nil || !configsEqual(decoded, next) {
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

func withRevision(current, next model.DesiredConfig, revision uint64) model.DesiredConfig {
	next.Revision = revision
	for id, node := range next.Nodes {
		previous, exists := current.Nodes[id]
		if exists && sameNodeIgnoringRevision(previous, node) {
			node.Revision = previous.Revision
		} else {
			node.Revision = revision
		}
		next.Nodes[id] = node
	}
	return next
}

func sameNodeIgnoringRevision(a, b model.Node) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Protocol == b.Protocol && a.Enabled == b.Enabled &&
		a.Server == b.Server && a.Port == b.Port && a.Username == b.Username && a.Password == b.Password &&
		a.SLPToken == b.SLPToken && a.SLPTransport == b.SLPTransport && a.SLPObfs == b.SLPObfs &&
		a.SLPObfsKey == b.SLPObfsKey && a.SLPInsecure == b.SLPInsecure && a.PolicyID == b.PolicyID &&
		equalTime(a.ExpiresAt, b.ExpiresAt)
}

func cloneConfig(cfg model.DesiredConfig) model.DesiredConfig {
	clone := cfg
	clone.Global.ManagementPorts = append([]uint16(nil), cfg.Global.ManagementPorts...)
	clone.Global.DoHEndpoints = append([]model.DoHEndpoint(nil), cfg.Global.DoHEndpoints...)
	clone.Nodes = make(map[string]model.Node, len(cfg.Nodes))
	for id, node := range cfg.Nodes {
		if node.ExpiresAt != nil {
			timeCopy := *node.ExpiresAt
			node.ExpiresAt = &timeCopy
		}
		clone.Nodes[id] = node
	}
	clone.Devices = make(map[string]model.Device, len(cfg.Devices))
	for id, device := range cfg.Devices {
		clone.Devices[id] = device
	}
	return clone
}

func configsEqual(a, b model.DesiredConfig) bool {
	if a.SchemaVersion != b.SchemaVersion || a.Revision != b.Revision || a.Global.Enabled != b.Global.Enabled || a.Global.RuntimeBackend != b.Global.RuntimeBackend || a.Global.MaxNodes != b.Global.MaxNodes || a.Global.LANDevice != b.Global.LANDevice || a.Global.L2TPConcurrency != b.Global.L2TPConcurrency || a.Global.ProxyConcurrency != b.Global.ProxyConcurrency || a.Global.ConnectTimeout != b.Global.ConnectTimeout || a.Global.StopTimeout != b.Global.StopTimeout || len(a.Global.ManagementPorts) != len(b.Global.ManagementPorts) || len(a.Global.DoHEndpoints) != len(b.Global.DoHEndpoints) || len(a.Nodes) != len(b.Nodes) || len(a.Devices) != len(b.Devices) {
		return false
	}
	for i := range a.Global.ManagementPorts {
		if a.Global.ManagementPorts[i] != b.Global.ManagementPorts[i] {
			return false
		}
	}
	for i := range a.Global.DoHEndpoints {
		if a.Global.DoHEndpoints[i] != b.Global.DoHEndpoints[i] {
			return false
		}
	}
	for id, nodeA := range a.Nodes {
		nodeB, ok := b.Nodes[id]
		if !ok || nodeA.ID != nodeB.ID || nodeA.Name != nodeB.Name || nodeA.Protocol != nodeB.Protocol || nodeA.Enabled != nodeB.Enabled || nodeA.Server != nodeB.Server || nodeA.Port != nodeB.Port || nodeA.Username != nodeB.Username || nodeA.Password != nodeB.Password || nodeA.SLPToken != nodeB.SLPToken || nodeA.SLPTransport != nodeB.SLPTransport || nodeA.SLPObfs != nodeB.SLPObfs || nodeA.SLPObfsKey != nodeB.SLPObfsKey || nodeA.SLPInsecure != nodeB.SLPInsecure || nodeA.PolicyID != nodeB.PolicyID || nodeA.Revision != nodeB.Revision || !equalTime(nodeA.ExpiresAt, nodeB.ExpiresAt) {
			return false
		}
	}
	for id, deviceA := range a.Devices {
		deviceB, ok := b.Devices[id]
		if !ok || deviceA != deviceB {
			return false
		}
	}
	return true
}

func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
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
