package diagnostics

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"
)

const (
	ArtifactReady      = "ready"
	defaultArtifactTTL = 15 * time.Minute
	maxArchiveEntries  = MaxSeedEntries + MaxCommands + 1
)

var (
	ErrArtifactNotFound = errors.New("diagnostic artifact not found")
	ErrArtifactExpired  = errors.New("diagnostic artifact expired")
	ErrArtifactUnsafe   = errors.New("diagnostic artifact is unsafe")
	artifactIDPattern   = regexp.MustCompile(`^diag-[a-f0-9]{16,64}$`)
)

type Artifact struct {
	ID        string    `json:"artifact_id"`
	State     string    `json:"state"`
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ArtifactClaim struct {
	Path     string
	Filename string
	Size     int64
}

type artifactRecord struct {
	artifact Artifact
	path     string
}

type ArtifactStoreOption func(*ArtifactStore)

func WithArtifactClock(clock func() time.Time) ArtifactStoreOption {
	return func(store *ArtifactStore) {
		if clock != nil {
			store.now = clock
		}
	}
}

func WithArtifactTTL(ttl time.Duration) ArtifactStoreOption {
	return func(store *ArtifactStore) {
		if ttl > 0 {
			store.ttl = ttl
		}
	}
}

func WithArtifactIDSource(source func() string) ArtifactStoreOption {
	return func(store *ArtifactStore) {
		if source != nil {
			store.newID = source
		}
	}
}

type ArtifactStore struct {
	mu      sync.Mutex
	root    string
	now     func() time.Time
	ttl     time.Duration
	newID   func() string
	records map[string]artifactRecord
}

func NewArtifactStore(root string, options ...ArtifactStoreOption) (*ArtifactStore, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("diagnostic artifact root is invalid")
	}
	store := &ArtifactStore{root: root, now: time.Now, ttl: defaultArtifactTTL, newID: randomArtifactID, records: make(map[string]artifactRecord)}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	if err := ensureArtifactRoot(root); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *ArtifactStore) Write(ctx context.Context, entries []Entry) (Artifact, error) {
	if store == nil || store.now == nil || store.newID == nil || ctx == nil {
		return Artifact{}, errors.New("diagnostic artifact store is invalid")
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if len(entries) > maxArchiveEntries {
		return Artifact{}, errors.New("diagnostic artifact has too many entries")
	}
	total := 0
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if err := validateEntryName(entry.Name, seen); err != nil || len(entry.Data) > MaxEntryBytes {
			return Artifact{}, errors.New("diagnostic artifact entry is invalid")
		}
		total += len(entry.Data)
		if total > MaxBundleBytes {
			return Artifact{}, errors.New("diagnostic artifact exceeds limit")
		}
	}
	id := store.newID()
	if !artifactIDPattern.MatchString(id) {
		return Artifact{}, errors.New("diagnostic artifact id is invalid")
	}
	finalPath := filepath.Join(store.root, id+".tar.gz")
	if !pathWithinRoot(store.root, finalPath) {
		return Artifact{}, ErrArtifactUnsafe
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.records[id]; exists {
		return Artifact{}, errors.New("diagnostic artifact id is duplicated")
	}
	if _, err := os.Lstat(finalPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return Artifact{}, errors.New("diagnostic artifact target already exists")
	}
	createdAt := store.now().UTC()
	temporary, err := os.CreateTemp(store.root, ".diagnostic-*")
	if err != nil {
		return Artifact{}, errors.New("diagnostic artifact temporary file failed")
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return Artifact{}, errors.New("diagnostic artifact permissions failed")
	}
	if err := writeTarGzip(ctx, temporary, entries, createdAt); err != nil {
		return Artifact{}, err
	}
	if err := temporary.Sync(); err != nil {
		return Artifact{}, errors.New("diagnostic artifact sync failed")
	}
	if err := temporary.Close(); err != nil {
		return Artifact{}, errors.New("diagnostic artifact close failed")
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return Artifact{}, errors.New("diagnostic artifact publish failed")
	}
	committed = true
	info, err := os.Lstat(finalPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		_ = os.Remove(finalPath)
		return Artifact{}, ErrArtifactUnsafe
	}
	artifact := Artifact{
		ID: id, State: ArtifactReady, Filename: "proxypool-diagnostics-" + id + ".tar.gz",
		Size: info.Size(), CreatedAt: createdAt, ExpiresAt: createdAt.Add(store.ttl),
	}
	store.records[id] = artifactRecord{artifact: artifact, path: finalPath}
	return artifact, nil
}

func (store *ArtifactStore) Claim(id string) (ArtifactClaim, error) {
	if store == nil || !artifactIDPattern.MatchString(id) {
		return ArtifactClaim{}, ErrArtifactNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record, exists := store.records[id]
	if !exists {
		return ArtifactClaim{}, ErrArtifactNotFound
	}
	if !store.now().Before(record.artifact.ExpiresAt) {
		delete(store.records, id)
		_ = os.Remove(record.path)
		return ArtifactClaim{}, ErrArtifactExpired
	}
	if !pathWithinRoot(store.root, record.path) {
		delete(store.records, id)
		return ArtifactClaim{}, ErrArtifactUnsafe
	}
	info, err := os.Lstat(record.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != record.artifact.Size || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		delete(store.records, id)
		return ArtifactClaim{}, ErrArtifactUnsafe
	}
	delete(store.records, id)
	return ArtifactClaim{Path: record.path, Filename: record.artifact.Filename, Size: record.artifact.Size}, nil
}

func ensureArtifactRoot(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return errors.New("diagnostic artifact root creation failed")
		}
		info, err = os.Lstat(root)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("diagnostic artifact root is unsafe")
	}
	if runtime.GOOS != "windows" {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(root) {
			return errors.New("diagnostic artifact root is unsafe")
		}
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return errors.New("diagnostic artifact root permissions failed")
	}
	return nil
}

func writeTarGzip(ctx context.Context, writer io.Writer, entries []Entry, modified time.Time) error {
	gzipWriter := gzip.NewWriter(writer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return err
		}
		header := &tar.Header{Name: entry.Name, Mode: 0o600, Size: int64(len(entry.Data)), ModTime: modified, Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			return errors.New("diagnostic archive header failed")
		}
		if _, err := tarWriter.Write(entry.Data); err != nil {
			return errors.New("diagnostic archive entry failed")
		}
	}
	if err := tarWriter.Close(); err != nil {
		return errors.New("diagnostic archive finalize failed")
	}
	if err := gzipWriter.Close(); err != nil {
		return errors.New("diagnostic compression finalize failed")
	}
	return nil
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && len(relative) > 0 && relative[:1] != "."
}

func randomArtifactID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	return "diag-" + hex.EncodeToString(value[:])
}
