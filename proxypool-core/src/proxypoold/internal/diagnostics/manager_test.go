package diagnostics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerCreatesArtifactAsynchronouslyAndClaimsOnce(t *testing.T) {
	serviceCtx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "diagnostics"),
		WithArtifactClock(func() time.Time { return artifactTestTime }),
		WithArtifactIDSource(func() string { return "diag-dddddddddddddddd" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	provider := func(ctx context.Context) (Snapshot, error) {
		close(started)
		select {
		case <-release:
			return Snapshot{Entries: map[string][]byte{"status.json": []byte(`{"password":"secret-value","state":"ready"}`)}, Secrets: []string{"secret-value"}}, nil
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	}
	build := func(ctx context.Context, snapshot Snapshot) ([]Entry, error) {
		return NewCollector(nil, NewRedactor(snapshot.Secrets), nil).Collect(ctx, snapshot.Entries)
	}
	manager := NewManager(serviceCtx, store, provider, build, WithManagerIDSource(func() string { return "diagnostic-job-1" }))
	status, err := manager.Create()
	if err != nil || status.ID != "diagnostic-job-1" || status.State != DiagnosticQueued {
		t.Fatalf("Create = %#v, %v", status, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("diagnostic worker did not start")
	}
	if current, ok := manager.Get(status.ID); !ok || (current.State != DiagnosticRunning && current.State != DiagnosticQueued) {
		t.Fatalf("running status = %#v ok=%t", current, ok)
	}
	close(release)
	ready := waitDiagnosticState(t, manager, status.ID, DiagnosticReady)
	if ready.Artifact == nil || ready.Artifact.ID != "diag-dddddddddddddddd" {
		t.Fatalf("ready status = %#v", ready)
	}
	claim, err := manager.Claim(ready.Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	archive := readArchive(t, claim.Path)
	if strings.Contains(string(archive["status.json"]), "secret-value") {
		t.Fatal("async diagnostic archive leaked a secret")
	}
	if _, err := manager.Claim(ready.Artifact.ID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("second manager claim error = %v", err)
	}
	if err := manager.Release(ready.Artifact.ID); err != nil {
		t.Fatal(err)
	}
}

func TestManagerReportsExpiredArtifactWithoutUsingBrowserTime(t *testing.T) {
	serviceCtx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	now := artifactTestTime
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "diagnostics"),
		WithArtifactClock(func() time.Time { return now }),
		WithArtifactIDSource(func() string { return "diag-ffffffffffffffff" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(serviceCtx, store,
		func(context.Context) (Snapshot, error) {
			return Snapshot{Entries: map[string][]byte{"status.json": []byte(`{}`)}}, nil
		},
		func(ctx context.Context, snapshot Snapshot) ([]Entry, error) {
			return NewCollector(nil, NewRedactor(nil), nil).Collect(ctx, snapshot.Entries)
		},
		WithManagerIDSource(func() string { return "diagnostic-job-expiry" }),
	)
	created, err := manager.Create()
	if err != nil {
		t.Fatal(err)
	}
	ready := waitDiagnosticState(t, manager, created.ID, DiagnosticReady)
	if ready.Artifact == nil {
		t.Fatal("ready diagnostic omitted artifact")
	}
	now = now.Add(defaultArtifactTTL + time.Second)
	expired, ok := manager.Get(created.ID)
	if !ok || expired.State != DiagnosticExpired || expired.Artifact != nil {
		t.Fatalf("expired status = %#v ok=%t", expired, ok)
	}
}

func TestManagerFailureIsGenericAndServiceCancellationStopsWork(t *testing.T) {
	serviceCtx, cancelService := context.WithCancel(context.Background())
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	provider := func(ctx context.Context) (Snapshot, error) {
		<-ctx.Done()
		return Snapshot{}, errors.New("credential=DO-NOT-RETURN")
	}
	manager := NewManager(serviceCtx, store, provider, nil, WithManagerIDSource(func() string { return "diagnostic-job-2" }))
	status, err := manager.Create()
	if err != nil {
		t.Fatal(err)
	}
	cancelService()
	failed := waitDiagnosticState(t, manager, status.ID, DiagnosticFailed)
	if failed.ErrorCode != "collection_cancelled" || strings.Contains(failed.ErrorCode, "DO-NOT-RETURN") {
		t.Fatalf("failed status = %#v", failed)
	}
}

func TestManagerHistoryRemainsBoundedBehindLongRunningOldestJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (Snapshot, error) { return Snapshot{}, errors.New("fixture failure") }
	sequence := 0
	manager := NewManager(ctx, store, provider, nil, WithManagerIDSource(func() string {
		sequence++
		return "diagnostic-job-" + strings.Repeat("x", sequence)
	}))
	for index := 0; index < MaxRetainedDiagnosticJobs+8; index++ {
		status, err := manager.Create()
		if err != nil {
			t.Fatal(err)
		}
		waitDiagnosticState(t, manager, status.ID, DiagnosticFailed)
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if len(manager.jobs) > MaxRetainedDiagnosticJobs || len(manager.order) > MaxRetainedDiagnosticJobs {
		t.Fatalf("diagnostic history grew to jobs=%d order=%d", len(manager.jobs), len(manager.order))
	}
}

func TestManagerWaitsForWorkersAndDeletesArtifactsOnCancellation(t *testing.T) {
	serviceCtx, cancel := context.WithCancel(context.Background())
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "diagnostics"), WithArtifactIDSource(func() string { return "diag-eeeeeeeeeeeeeeee" }))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	manager := NewManager(serviceCtx, store, func(ctx context.Context) (Snapshot, error) {
		close(started)
		<-ctx.Done()
		return Snapshot{}, ctx.Err()
	}, func(context.Context, Snapshot) ([]Entry, error) {
		return []Entry{{Name: "safe.txt", Data: []byte("x")}}, nil
	})
	if _, err := manager.Create(); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	if err := manager.Wait(); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	running := manager.running
	manager.mu.RUnlock()
	if running != 0 {
		t.Fatalf("running = %d", running)
	}
}

func TestManagerReportsShutdownCleanupFailure(t *testing.T) {
	serviceCtx, cancel := context.WithCancel(context.Background())
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(serviceCtx, store, func(context.Context) (Snapshot, error) { return Snapshot{}, nil }, nil)
	if err := os.Mkdir(filepath.Join(store.root, "unexpected-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := manager.Wait(); err == nil {
		t.Fatal("shutdown cleanup failure was swallowed")
	}
}

func waitDiagnosticState(t *testing.T, manager *Manager, id, want string) DiagnosticStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := manager.Get(id)
		if ok && status.State == want {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, _ := manager.Get(id)
	t.Fatalf("diagnostic state = %#v, want %s", status, want)
	return DiagnosticStatus{}
}
