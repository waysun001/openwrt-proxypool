package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"proxypoold/internal/model"
)

func TestReplaceRejectsStaleRevisionWithoutChangingFile(t *testing.T) {
	path := writeInitialConfig(t, 3)
	before := readConfigBytes(t, path)
	store := NewStore(path)

	_, err := store.Replace(context.Background(), 2, validConfig())
	assertCode(t, err, "revision_conflict")
	if !bytes.Equal(readConfigBytes(t, path), before) {
		t.Fatal("stale replace changed the original file")
	}
}

func TestReplaceRejectsInvalidConfigWithoutChangingFile(t *testing.T) {
	path := writeInitialConfig(t, 3)
	before := readConfigBytes(t, path)
	store := NewStore(path)
	next := validConfig()
	node := next.Nodes["node-a"]
	node.Port = 0
	next.Nodes["node-a"] = node

	_, err := store.Replace(context.Background(), 3, next)
	assertCode(t, err, "invalid_config")
	if !bytes.Equal(readConfigBytes(t, path), before) {
		t.Fatal("invalid replace changed the original file")
	}
}

func TestReplaceAdvancesRevisionAndPreservesPolicyID(t *testing.T) {
	path := writeInitialConfig(t, 3)
	store := NewStore(path)
	next := validConfig()
	next.Revision = 999
	next.Nodes["node-a"] = withNodeRevision(next.Nodes["node-a"], 999)

	got, err := store.Replace(context.Background(), 3, next)
	if err != nil {
		t.Fatalf("Replace(): %v", err)
	}
	if got.Revision != 4 || got.Nodes["node-a"].Revision != 4 {
		t.Fatalf("revision = config %d node %d, want 4", got.Revision, got.Nodes["node-a"].Revision)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(): %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("stored mode = %o, want no group/world permissions", info.Mode().Perm())
	}

	changed := validConfig()
	node := changed.Nodes["node-a"]
	node.PolicyID = 9
	changed.Nodes["node-a"] = node
	_, err = store.Replace(context.Background(), 4, changed)
	assertCode(t, err, "invalid_config")
}

func TestReplaceCleansTempAndKeepsOriginalOnWriteSyncOrRenameFailure(t *testing.T) {
	for name, fail := range map[string]string{"write": "write", "sync": "sync", "rename": "rename"} {
		t.Run(name, func(t *testing.T) {
			path := writeInitialConfig(t, 3)
			before := readConfigBytes(t, path)
			ops := &failingOps{fsOps: osFS{}, fail: fail}
			store := newStore(path, ops)

			_, err := store.Replace(context.Background(), 3, validConfig())
			if err == nil {
				t.Fatal("Replace() error = nil, want injected filesystem error")
			}
			if !bytes.Equal(readConfigBytes(t, path), before) {
				t.Fatal("failed replace changed the original file")
			}
			matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".proxypool-v2-*"))
			if globErr != nil || len(matches) != 0 {
				t.Fatal("failed replace left a temporary file")
			}
		})
	}
}

func TestReplaceReportsDirectorySyncFailureAfterRename(t *testing.T) {
	path := writeInitialConfig(t, 3)
	store := newStore(path, &failingOps{fsOps: osFS{}, fail: "dir-sync"})
	_, err := store.Replace(context.Background(), 3, validConfig())
	if err == nil {
		t.Fatal("Replace() error = nil, want directory sync failure")
	}
	loaded, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load(after directory sync failure): %v", err)
	}
	if loaded.Revision != 4 {
		t.Fatalf("revision after renamed file = %d, want 4", loaded.Revision)
	}
}

func TestReplaceWithSameExpectedRevisionHasOneWinner(t *testing.T) {
	path := writeInitialConfig(t, 3)
	store := NewStore(path)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.Replace(context.Background(), 3, validConfig())
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var codeErr *model.CodeError
		if errors.As(err, &codeErr) && codeErr.Code == "revision_conflict" {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent replace results = %d successes, %d conflicts", successes, conflicts)
	}
}

func TestLoadRejectsInvalidOnDiskConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxypool")
	if err := os.WriteFile(path, []byte("config global 'global'\noption password 'fixture-password-not-real'\n"), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	_, err := NewStore(path).Load()
	assertCode(t, err, "invalid_config")
	if strings.Contains(err.Error(), "fixture-password-not-real") {
		t.Fatal("load error leaked a secret")
	}
}

func TestReplaceHonorsCanceledContextBeforeWriting(t *testing.T) {
	path := writeInitialConfig(t, 3)
	before := readConfigBytes(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewStore(path).Replace(ctx, 3, validConfig())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Replace() error = %v, want context cancellation", err)
	}
	if !bytes.Equal(readConfigBytes(t, path), before) {
		t.Fatal("canceled replace changed the original file")
	}
}

func writeInitialConfig(t *testing.T, revision uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxypool")
	cfg := validConfig()
	cfg.Revision = revision
	for id, node := range cfg.Nodes {
		node.Revision = revision
		cfg.Nodes[id] = node
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode(initial): %v", err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatalf("write initial config: %v", err)
	}
	return path
}

func readConfigBytes(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return contents
}

func withNodeRevision(node model.Node, revision uint64) model.Node {
	node.Revision = revision
	return node
}

type failingOps struct {
	fsOps
	fail string
}

func (f *failingOps) CreateTemp(dir, pattern string) (tempFile, error) {
	file, err := f.fsOps.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return failingFile{tempFile: file, fail: f.fail}, nil
}

func (f *failingOps) Rename(oldPath, newPath string) error {
	if f.fail == "rename" {
		return errors.New("injected rename failure")
	}
	return f.fsOps.Rename(oldPath, newPath)
}

func (f *failingOps) SyncDir(path string) error {
	if f.fail == "dir-sync" {
		return errors.New("injected directory sync failure")
	}
	return f.fsOps.SyncDir(path)
}

type failingFile struct {
	tempFile
	fail string
}

func (f failingFile) Write(contents []byte) (int, error) {
	if f.fail == "write" {
		return 0, errors.New("injected write failure")
	}
	return f.tempFile.Write(contents)
}

func (f failingFile) Sync() error {
	if f.fail == "sync" {
		return errors.New("injected sync failure")
	}
	return f.tempFile.Sync()
}
