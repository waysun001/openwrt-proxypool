package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"proxypoold/internal/model"
)

func TestJobStoreSnapshotRoundTripPreservesOrderAndSequence(t *testing.T) {
	source := NewJobStore()
	for index := 0; index < 3; index++ {
		if err := source.Put(runningJob(testJob(index))); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 2; index++ {
		if _, err := source.AppendEvent(NodeEvent{
			JobID: "job-000", NodeID: "node-000", Generation: 1,
			State: model.StateStarting, At: stateTestEpoch.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}

	snapshot := source.Snapshot()
	restored := NewJobStore()
	if err := restored.Restore(snapshot); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if got := restored.Snapshot(); !reflect.DeepEqual(got, snapshot) {
		t.Fatalf("round trip mismatch:\n got: %#v\nwant: %#v", got, snapshot)
	}

	// Neither the snapshot input nor subsequent returned snapshots may alias
	// retained store state.
	snapshot.Jobs[0].Nodes[0].Step = "mutated input"
	snapshot.Events[0].NodeID = "mutated input"
	copyOut := restored.Snapshot()
	copyOut.Jobs[0].Nodes[0].Step = "mutated output"
	copyOut.Events[0].NodeID = "mutated output"
	unchanged := restored.Snapshot()
	if unchanged.Jobs[0].Nodes[0].Step != "start" || unchanged.Events[0].NodeID != "node-000" {
		t.Fatalf("restored snapshot aliases caller memory: %#v", unchanged)
	}

	next, err := restored.AppendEvent(NodeEvent{
		JobID: "job-000", NodeID: "node-000", Generation: 1,
		State: model.StateValidating, At: stateTestEpoch.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Sequence != 3 {
		t.Fatalf("sequence after restore = %d, want 3", next.Sequence)
	}
}

func TestJobStoreRestoreRejectsInvalidOrDuplicateSnapshotAtomically(t *testing.T) {
	baselineStore := NewJobStore()
	if err := baselineStore.Put(runningJob(testJob(9))); err != nil {
		t.Fatal(err)
	}
	baseline := baselineStore.Snapshot()
	valid := runtimeSnapshotFixture().Jobs

	tests := map[string]func(JobSnapshot) JobSnapshot{
		"duplicate job": func(snapshot JobSnapshot) JobSnapshot {
			snapshot.Jobs = append(snapshot.Jobs, snapshot.Jobs[0])
			return snapshot
		},
		"duplicate event sequence": func(snapshot JobSnapshot) JobSnapshot {
			snapshot.Events = append(snapshot.Events, snapshot.Events[0])
			return snapshot
		},
		"event sequence above next": func(snapshot JobSnapshot) JobSnapshot {
			snapshot.NextEventSequence = 0
			return snapshot
		},
		"invalid job": func(snapshot JobSnapshot) JobSnapshot {
			snapshot.Jobs[0].State = JobState("invented")
			return snapshot
		},
		"out of order events": func(snapshot JobSnapshot) JobSnapshot {
			snapshot.Events = append(snapshot.Events, NodeEvent{
				Sequence: 1, JobID: "job-a", NodeID: "node-a", Generation: 1,
				State: model.StateStarting, At: stateTestEpoch.Add(time.Second),
			})
			return snapshot
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store := NewJobStore()
			if err := store.Restore(baseline); err != nil {
				t.Fatal(err)
			}
			before := store.Snapshot()
			if err := store.Restore(mutate(valid)); err == nil {
				t.Fatal("Restore() error = nil, want rejection")
			}
			if after := store.Snapshot(); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed restore mutated live store:\n got: %#v\nwant: %#v", after, before)
			}
		})
	}
}

func TestRuntimeStoreRoundTripUsesPrivateAtomicFileAndRedactsErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	store := NewRuntimeStore(path)
	snapshot := runtimeSnapshotFixture()
	const secret = "credential-DO-NOT-PERSIST"
	snapshot.NodeStatuses[0].LastError = &PublicError{Code: "password_" + secret, Message: "token=" + secret}

	if err := store.Save(context.Background(), snapshot); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(contents, []byte(secret)) || bytes.Contains(bytes.ToLower(contents), []byte("password")) {
		t.Fatalf("runtime file leaked credential-shaped data: %s", contents)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime mode = %o, want 600", info.Mode().Perm())
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := runtimeSnapshotFixture()
	want.NodeStatuses[0].LastError = &PublicError{Code: ErrorCodeInternal, Message: "internal node error"}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("Load() = %#v, want %#v", loaded, want)
	}
}

func TestRuntimeStoreRejectsCorruptUnknownOrUnboundedState(t *testing.T) {
	tests := map[string][]byte{
		"corrupt":        []byte(`{"schema_version":1`),
		"unknown field":  []byte(`{"schema_version":1,"config_revision":1,"jobs":{"jobs":[],"events":[],"next_event_sequence":0},"node_statuses":[],"future":true}`),
		"unknown schema": []byte(`{"schema_version":99,"config_revision":1,"jobs":{"jobs":[],"events":[],"next_event_sequence":0},"node_statuses":[]}`),
		"trailing value": []byte(`{"schema_version":1,"config_revision":1,"jobs":{"jobs":[],"events":[],"next_event_sequence":0},"node_statuses":[]} {}`),
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime.json")
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewRuntimeStore(path).Load(); err == nil {
				t.Fatal("Load() error = nil, want strict rejection")
			}
		})
	}

	path := filepath.Join(t.TempDir(), "runtime.json")
	snapshot := runtimeSnapshotFixture()
	for index := 0; index <= MaxRuntimeNodeStatuses; index++ {
		snapshot.NodeStatuses = append(snapshot.NodeStatuses, NodeStatus{
			NodeID: "overflow-node-" + strings.Repeat("x", index+1), JobID: "job-a",
			Generation: 1, State: model.StateQueued, UpdatedAt: stateTestEpoch,
		})
	}
	if err := NewRuntimeStore(path).Save(context.Background(), snapshot); err == nil {
		t.Fatal("Save(oversized snapshot) error = nil, want rejection")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid Save() created target: %v", err)
	}
}

func TestRuntimeStoreRejectsSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "runtime.json")
	before := []byte("do-not-touch")
	if err := os.WriteFile(target, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store := NewRuntimeStore(link)
	if _, err := store.Load(); err == nil {
		t.Fatal("Load(symlink) error = nil")
	}
	if err := store.Save(context.Background(), runtimeSnapshotFixture()); err == nil {
		t.Fatal("Save(symlink) error = nil")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, before) {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestRuntimeStoreCancelledSaveHasZeroMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	store := NewRuntimeStore(path)
	if err := store.Save(context.Background(), runtimeSnapshotFixture()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	next := runtimeSnapshotFixture()
	next.ConfigRevision++
	if err := store.Save(ctx, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save(cancelled) error = %v, want context.Canceled", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("cancelled Save changed runtime file")
	}
}

func TestRuntimeStoreSyncsFileBeforeRenameAndDirectoryAfterRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	ops := &recordingRuntimeFS{runtimeFS: osRuntimeFS{}}
	store := newRuntimeStore(path, ops)
	if err := store.Save(context.Background(), runtimeSnapshotFixture()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	fileSync := operationIndex(ops.operations, "file-sync")
	rename := operationIndex(ops.operations, "rename")
	dirSync := operationIndex(ops.operations, "dir-sync")
	if fileSync < 0 || rename < 0 || dirSync < 0 || !(fileSync < rename && rename < dirSync) {
		t.Fatalf("durability operation order = %v", ops.operations)
	}
}

func TestRuntimeStoreRenameFailureLeavesOldSnapshotAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	store := NewRuntimeStore(path)
	old := runtimeSnapshotFixture()
	if err := store.Save(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	ops := &recordingRuntimeFS{runtimeFS: osRuntimeFS{}, failRename: true}
	next := runtimeSnapshotFixture()
	next.ConfigRevision++
	if err := newRuntimeStore(path, ops).Save(context.Background(), next); err == nil {
		t.Fatal("Save() error = nil, want injected rename failure")
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, old) {
		t.Fatalf("failed atomic replace exposed partial state: %#v", loaded)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".proxypool-runtime-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("failed Save left temp files: %v, %v", matches, err)
	}
}

func runtimeSnapshotFixture() RuntimeSnapshot {
	job := runningJob(testJob(1))
	return RuntimeSnapshot{
		SchemaVersion:  RuntimeSnapshotSchemaVersion,
		ConfigRevision: 7,
		Jobs: JobSnapshot{
			Jobs: []Job{job},
			Events: []NodeEvent{{
				Sequence: 1, JobID: job.ID, NodeID: job.Nodes[0].NodeID,
				Generation: 2, State: model.StateStarting, Attempt: 1, At: stateTestEpoch,
			}},
			NextEventSequence: 1,
		},
		NodeStatuses: []NodeStatus{{
			NodeID: job.Nodes[0].NodeID, JobID: job.ID, Generation: 2,
			State: model.StateStarting, Attempts: 1, UpdatedAt: stateTestEpoch,
		}},
	}
}

type recordingRuntimeFS struct {
	runtimeFS
	operations    []string
	failRename    bool
	failSyncDirAt int
	syncDirCount  int
}

func (f *recordingRuntimeFS) CreateTemp(dir, pattern string) (runtimeTempFile, error) {
	f.operations = append(f.operations, "create-temp")
	file, err := f.runtimeFS.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &recordingRuntimeFile{runtimeTempFile: file, owner: f}, nil
}

func (f *recordingRuntimeFS) Rename(oldPath, newPath string) error {
	f.operations = append(f.operations, "rename")
	if f.failRename {
		return errors.New("injected rename failure")
	}
	return f.runtimeFS.Rename(oldPath, newPath)
}

func (f *recordingRuntimeFS) SyncDir(path string) error {
	f.operations = append(f.operations, "dir-sync")
	f.syncDirCount++
	if f.failSyncDirAt > 0 && f.syncDirCount == f.failSyncDirAt {
		return errors.New("injected directory sync failure")
	}
	return f.runtimeFS.SyncDir(path)
}

type recordingRuntimeFile struct {
	runtimeTempFile
	owner *recordingRuntimeFS
}

func (f *recordingRuntimeFile) Write(contents []byte) (int, error) {
	f.owner.operations = append(f.owner.operations, "write")
	return f.runtimeTempFile.Write(contents)
}

func (f *recordingRuntimeFile) Sync() error {
	f.owner.operations = append(f.owner.operations, "file-sync")
	return f.runtimeTempFile.Sync()
}

func (f *recordingRuntimeFile) Close() error {
	f.owner.operations = append(f.owner.operations, "close")
	return f.runtimeTempFile.Close()
}

func operationIndex(operations []string, want string) int {
	for index, operation := range operations {
		if operation == want {
			return index
		}
	}
	return -1
}
