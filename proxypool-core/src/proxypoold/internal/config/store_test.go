package config

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestReplaceCleansTempAndKeepsOriginalOnAllPreRenameFailures(t *testing.T) {
	for _, fail := range []string{"create-temp", "chmod", "close", "read-temp", "decode-temp", "semantic-temp"} {
		t.Run(fail, func(t *testing.T) {
			path := writeInitialConfig(t, 3)
			before := readConfigBytes(t, path)
			ops := &failingOps{fsOps: osFS{}, fail: fail}
			_, err := newStore(path, ops).Replace(context.Background(), 3, validConfig())
			if err == nil {
				t.Fatal("Replace() error = nil, want injected failure")
			}
			if !bytes.Equal(readConfigBytes(t, path), before) {
				t.Fatal("pre-rename failure changed the original file")
			}
			matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".proxypool-v2-*"))
			if globErr != nil || len(matches) != 0 {
				t.Fatal("pre-rename failure left a temporary file")
			}
			if ops.createTempCalls != 1 || ops.renameCalls != 0 {
				t.Fatal("injected failure did not stop before rename")
			}
			if fail == "chmod" && ops.chmodCalls != 1 {
				t.Fatal("chmod failure branch was not reached")
			}
			if fail == "close" && ops.closeCalls == 0 {
				t.Fatal("close failure branch was not reached")
			}
			if (fail == "read-temp" || fail == "decode-temp" || fail == "semantic-temp") && ops.readFileCalls < 2 {
				t.Fatal("temporary validation branch was not reached")
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

func TestReplaceRejectsRevisionOverflowBeforeTemporaryCreation(t *testing.T) {
	path := writeInitialConfig(t, math.MaxUint64)
	before := readConfigBytes(t, path)
	ops := &failingOps{fsOps: osFS{}}
	_, err := newStore(path, ops).Replace(context.Background(), math.MaxUint64, validConfig())
	assertCode(t, err, "invalid_config")
	if ops.createTempCalls != 0 || !bytes.Equal(readConfigBytes(t, path), before) {
		t.Fatal("overflow replace touched the filesystem")
	}
}

func TestReplaceDoesNotMutateCallerConfig(t *testing.T) {
	for name, fail := range map[string]string{"success": "", "rename failure": "rename"} {
		t.Run(name, func(t *testing.T) {
			path := writeInitialConfig(t, 3)
			next := specialValueConfig("not-a-real-secret")
			now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.FixedZone("UTC+8", 8*60*60))
			node := next.Nodes["node-a"]
			node.ExpiresAt = &now
			next.Nodes["node-a"] = node
			before := cloneForTest(next)
			_, err := newStore(path, &failingOps{fsOps: osFS{}, fail: fail}).Replace(context.Background(), 3, next)
			if fail == "" && err != nil {
				t.Fatalf("Replace(): %v", err)
			}
			if fail != "" && err == nil {
				t.Fatal("Replace() error = nil, want rename failure")
			}
			if !safeConfigsEqual(next, before) {
				t.Fatal("Replace mutated the caller config")
			}
		})
	}
}

func TestReplaceRejectsCodecGlobalInvariants(t *testing.T) {
	for name, mutate := range map[string]func(*model.DesiredConfig){
		"node capacity": func(cfg *model.DesiredConfig) { cfg.Global.MaxNodes = 1 },
		"missing DoH":   func(cfg *model.DesiredConfig) { cfg.Global.DoHEndpoints = nil },
		"missing port":  func(cfg *model.DesiredConfig) { cfg.Global.ManagementPorts = nil },
	} {
		t.Run(name, func(t *testing.T) {
			path := writeInitialConfig(t, 3)
			next := validConfig()
			mutate(&next)
			before := readConfigBytes(t, path)
			_, err := NewStore(path).Replace(context.Background(), 3, next)
			assertCode(t, err, "invalid_config")
			if !bytes.Equal(readConfigBytes(t, path), before) {
				t.Fatal("invalid invariant replace changed the original file")
			}
		})
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
	fail            string
	createTempCalls int
	readFileCalls   int
	renameCalls     int
	chmodCalls      int
	closeCalls      int
}

func (f *failingOps) CreateTemp(dir, pattern string) (tempFile, error) {
	f.createTempCalls++
	if f.fail == "create-temp" {
		return nil, errors.New("injected create temporary failure")
	}
	file, err := f.fsOps.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return failingFile{tempFile: file, fail: f.fail, owner: f}, nil
}

func (f *failingOps) ReadFile(path string) ([]byte, error) {
	f.readFileCalls++
	if f.readFileCalls == 2 && f.fail == "read-temp" {
		return nil, errors.New("injected temporary read failure")
	}
	if f.readFileCalls == 2 && f.fail == "decode-temp" {
		return []byte("malformed temporary content"), nil
	}
	contents, err := f.fsOps.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if f.readFileCalls == 2 && f.fail == "semantic-temp" {
		return bytes.Replace(contents, []byte("option revision '4'"), []byte("option revision '5'"), 1), nil
	}
	return contents, nil
}

func (f *failingOps) Rename(oldPath, newPath string) error {
	f.renameCalls++
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
	fail  string
	owner *failingOps
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

func (f failingFile) Chmod(mode os.FileMode) error {
	f.owner.chmodCalls++
	if f.fail == "chmod" {
		return errors.New("injected chmod failure")
	}
	return f.tempFile.Chmod(mode)
}

func (f failingFile) Close() error {
	f.owner.closeCalls++
	err := f.tempFile.Close()
	if f.fail == "close" && f.owner.closeCalls == 1 {
		return errors.New("injected close failure")
	}
	return err
}

func cloneForTest(cfg model.DesiredConfig) model.DesiredConfig {
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
