package config

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

func TestMigrateV1PreservesCredentialsAndLearnsOrDefersBindings(t *testing.T) {
	source, err := os.ReadFile("testdata/v1-realistic.uci")
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), source...)
	base := validConfig()
	base.Revision = 9
	base.Nodes = map[string]model.Node{
		"node_l2tp_a": {ID: "node_l2tp_a", Name: "Existing", Protocol: model.ProtocolSOCKS5, Server: "existing.example", Port: 1080, PolicyID: 1},
	}
	base.Devices = map[string]model.Device{}
	base.PendingBindings = map[string]model.PendingBinding{}
	now := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	discovered := []platform.DiscoveredDevice{{
		ID: "device_001122334455", MAC: "00:11:22:33:44:55", IPv4: netip.MustParseAddr("192.168.9.20"),
		Hostname: "phone", Ingress: "lan1", LastSeen: now, Confirmed: true,
	}}

	result, err := MigrateV1(source, base, discovered, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, original) {
		t.Fatal("migration changed source bytes")
	}
	if result.SourceSHA256 == "" || result.MigratedNodes != 3 || result.LearnedDevices != 1 || result.PendingBindings != 1 {
		t.Fatalf("migration summary = %#v", result)
	}
	if result.Config.Revision != 9 || len(result.Config.Nodes) != 4 {
		t.Fatalf("migrated config revision/nodes = %d/%d", result.Config.Revision, len(result.Config.Nodes))
	}
	var l2tp model.Node
	for _, node := range result.Config.Nodes {
		if node.Name == "Legacy L2TP" {
			l2tp = node
		}
	}
	if l2tp.ID == "" || l2tp.ID == "node_l2tp_a" || l2tp.PolicyID != 2 || l2tp.Username != "legacy-user" || l2tp.Password != "legacy-password" || !l2tp.Enabled || l2tp.ExpiresAt == nil || l2tp.ExpiresAt.Format("2006-01-02") != "2027-01-02" {
		t.Fatalf("migrated L2TP = %#v", l2tp)
	}
	device := result.Config.Devices["device_001122334455"]
	if device.NodeID != l2tp.ID || device.FixedIPv4.String() != "192.168.9.20" || !device.Enabled {
		t.Fatalf("learned device = %#v", device)
	}
	if len(result.Config.PendingBindings) != 1 {
		t.Fatalf("pending bindings = %#v", result.Config.PendingBindings)
	}
	for _, pending := range result.Config.PendingBindings {
		if pending.LegacyIPv4.String() != "192.168.9.21" || pending.CreatedAt != now || pending.ErrorCode != "" {
			t.Fatalf("pending binding = %#v", pending)
		}
	}
	if err := model.Validate(result.Config); err != nil {
		t.Fatalf("migrated config is invalid: %v", err)
	}
}

func TestMigrateV1RejectsDuplicateBindWithoutPartialResult(t *testing.T) {
	source, err := os.ReadFile("testdata/v1-duplicate-bind.uci")
	if err != nil {
		t.Fatal(err)
	}
	result, err := MigrateV1(source, emptyMigrationBase(), nil, time.Now().UTC())
	if err == nil || result.Config.SchemaVersion != 0 || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate result/error = %#v / %v", result, err)
	}
}

func TestExportV1RoundTripPreservesSecretsAndBindingIPv4(t *testing.T) {
	source, err := os.ReadFile("testdata/v1-realistic.uci")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	migrated, err := MigrateV1(source, emptyMigrationBase(), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if err := ExportV1(&exported, migrated.Config); err != nil {
		t.Fatal(err)
	}
	text := exported.String()
	for _, value := range []string{"legacy-password", "proxy-password", "legacy-token", "legacy-obfs-key", "192.168.9.20", "192.168.9.21"} {
		if !strings.Contains(text, value) {
			t.Fatalf("export omitted %q: %s", value, text)
		}
	}
	remigrated, err := MigrateV1(exported.Bytes(), emptyMigrationBase(), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(remigrated.Config.Nodes) != 3 || len(remigrated.Config.PendingBindings) != 2 {
		t.Fatalf("round trip counts = nodes %d pending %d", len(remigrated.Config.Nodes), len(remigrated.Config.PendingBindings))
	}
}

func TestMigrateV1FilesBacksUpAtomicallyAndIsIdempotent(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "proxypool")
	targetPath := filepath.Join(directory, "proxypool_v2")
	backupDir := filepath.Join(directory, "backups")
	markerPath := filepath.Join(directory, "migration.json")
	source, err := os.ReadFile("testdata/v1-realistic.uci")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	var base bytes.Buffer
	if err := Encode(&base, emptyMigrationBase()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, base.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	first, err := MigrateV1Files(context.Background(), sourcePath, targetPath, backupDir, markerPath, filepath.Join(directory, "daemon.sock"), now)
	if err != nil {
		t.Fatal(err)
	}
	if first.AlreadyApplied || first.TargetRevision != 1 || first.BackupPath == "" {
		t.Fatalf("first migration = %#v", first)
	}
	backup, err := os.ReadFile(first.BackupPath)
	if err != nil || !bytes.Equal(backup, source) {
		t.Fatalf("backup = %q / %v", backup, err)
	}
	if info, err := os.Stat(first.BackupPath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("backup mode = %v / %v", info, err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil || bytes.Contains(marker, []byte("legacy-password")) || !bytes.Contains(marker, []byte(`"status":"committed"`)) {
		t.Fatalf("marker = %s / %v", marker, err)
	}
	if info, err := os.Stat(markerPath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("marker mode = %v / %v", info, err)
	}
	second, err := MigrateV1Files(context.Background(), sourcePath, targetPath, backupDir, markerPath, filepath.Join(directory, "daemon.sock"), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyApplied || second.TargetRevision != first.TargetRevision || second.BackupPath != first.BackupPath {
		t.Fatalf("second migration = %#v, first %#v", second, first)
	}
	stored, err := NewStore(targetPath).Load()
	if err != nil || stored.Revision != 1 || len(stored.Nodes) != 3 {
		t.Fatalf("idempotent target = revision %d nodes %d / %v", stored.Revision, len(stored.Nodes), err)
	}
	unchanged, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(unchanged, source) {
		t.Fatal("migration modified the legacy source")
	}
}

func TestMigrateV1FilesRejectsSymlinkSource(t *testing.T) {
	directory := t.TempDir()
	realSource := filepath.Join(directory, "real")
	sourcePath := filepath.Join(directory, "source")
	if err := os.WriteFile(realSource, []byte("config global 'global'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realSource, sourcePath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := MigrateV1Files(context.Background(), sourcePath, filepath.Join(directory, "target"), filepath.Join(directory, "backups"), filepath.Join(directory, "marker"), filepath.Join(directory, "daemon.sock"), time.Now().UTC())
	if err == nil {
		t.Fatal("symlink source was accepted")
	}
}

func TestMigrateV1FilesResumesPendingMarkerBeforeTargetReplace(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "proxypool")
	targetPath := filepath.Join(directory, "proxypool_v2")
	backupDir := filepath.Join(directory, "backups")
	markerPath := filepath.Join(directory, "migration.json")
	source, err := os.ReadFile("testdata/v1-realistic.uci")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	base := emptyMigrationBase()
	base.Revision = 7
	var encoded bytes.Buffer
	if err := Encode(&encoded, base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureMigrationDirectory(backupDir); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(backupDir, "existing-backup.uci")
	if err := ensureMigrationBackup(backupPath, source); err != nil {
		t.Fatal(err)
	}
	migrationTime := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	preview, err := MigrateV1(source, base, nil, migrationTime)
	if err != nil {
		t.Fatal(err)
	}
	targetSHA256, err := configSHA256(withRevision(base, cloneConfig(preview.Config), 8))
	if err != nil {
		t.Fatal(err)
	}
	marker := migrationMarker{
		SchemaVersion: 1, Status: "pending", SourceSHA256: sha256Text(source), TargetSHA256: targetSHA256,
		BaseRevision: 7, TargetRevision: 8, BackupPath: backupPath,
		CreatedAt: migrationTime,
	}
	if err := writeMigrationMarker(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	result, err := MigrateV1Files(context.Background(), sourcePath, targetPath, backupDir, markerPath, filepath.Join(directory, "daemon.sock"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.AlreadyApplied || result.TargetRevision != 8 || result.BackupPath != backupPath {
		t.Fatalf("resumed migration = %#v", result)
	}
	stored, err := NewStore(targetPath).Load()
	if err != nil || stored.Revision != 8 || len(stored.Nodes) != 3 {
		t.Fatalf("resumed target = revision %d nodes %d / %v", stored.Revision, len(stored.Nodes), err)
	}
}

func TestMigrateV1FilesRejectsUnrelatedTargetAtMarkerRevision(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "proxypool")
	targetPath := filepath.Join(directory, "proxypool_v2")
	backupDir := filepath.Join(directory, "backups")
	markerPath := filepath.Join(directory, "migration.json")
	source, err := os.ReadFile("testdata/v1-realistic.uci")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	base := emptyMigrationBase()
	base.Revision = 7
	migrationTime := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	preview, err := MigrateV1(source, base, nil, migrationTime)
	if err != nil {
		t.Fatal(err)
	}
	expected := withRevision(base, cloneConfig(preview.Config), 8)
	targetSHA256, err := configSHA256(expected)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMigrationDirectory(backupDir); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(backupDir, "existing-backup.uci")
	if err := ensureMigrationBackup(backupPath, source); err != nil {
		t.Fatal(err)
	}
	marker := migrationMarker{
		SchemaVersion: 1, Status: "pending", SourceSHA256: sha256Text(source), TargetSHA256: targetSHA256,
		BaseRevision: 7, TargetRevision: 8, BackupPath: backupPath,
		CreatedAt: migrationTime,
	}
	if err := writeMigrationMarker(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	unrelated := base
	unrelated.Revision = 8
	unrelated.Nodes = map[string]model.Node{
		"node_unrelated": {ID: "node_unrelated", Name: "Unrelated", Protocol: model.ProtocolSOCKS5, Server: "unrelated.example", Port: 1080, PolicyID: 1, Revision: 8},
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, unrelated); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateV1Files(context.Background(), sourcePath, targetPath, backupDir, markerPath, filepath.Join(directory, "daemon.sock"), time.Now().UTC()); err == nil {
		t.Fatal("unrelated target at marker revision was accepted")
	}
	storedMarker, exists, err := loadMigrationMarker(markerPath)
	if err != nil || !exists || storedMarker.Status != "pending" {
		t.Fatalf("marker after conflict = %#v, exists %t, error %v", storedMarker, exists, err)
	}
}

func TestMigrationTransactionLockSerializesSameTarget(t *testing.T) {
	targetPath := filepath.Join(t.TempDir(), "proxypool_v2")
	first, err := acquireMigrationTransactionLock(context.Background(), targetPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if second, err := acquireMigrationTransactionLock(ctx, targetPath); !errors.Is(err, context.DeadlineExceeded) {
		if err == nil {
			_ = second.Close()
		}
		t.Fatalf("second migration lock error = %v, want deadline exceeded", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireMigrationTransactionLock(context.Background(), targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateV1FilesConcurrentCallsHaveOneCommittedWinner(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "proxypool")
	targetPath := filepath.Join(directory, "proxypool_v2")
	backupDir := filepath.Join(directory, "backups")
	markerPath := filepath.Join(directory, "migration.json")
	source, err := os.ReadFile("testdata/v1-realistic.uci")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, emptyMigrationBase()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	start := make(chan struct{})
	type outcome struct {
		result FileMigrationResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var workers sync.WaitGroup
	for range []int{0, 1} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := MigrateV1Files(context.Background(), sourcePath, targetPath, backupDir, markerPath, filepath.Join(directory, "daemon.sock"), now)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)
	migrated, unchanged := 0, 0
	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.result.AlreadyApplied {
			unchanged++
		} else {
			migrated++
		}
	}
	if migrated != 1 || unchanged != 1 {
		t.Fatalf("concurrent migration results = %d migrated, %d unchanged", migrated, unchanged)
	}
}

func TestMigrateV1FilesRejectsRunningDaemonBeforeMutation(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "proxypool")
	targetPath := filepath.Join(directory, "proxypool_v2")
	backupDir := filepath.Join(directory, "backups")
	markerPath := filepath.Join(directory, "migration.json")
	socketPath := filepath.Join(directory, "proxypoold.sock")
	source, err := os.ReadFile("testdata/v1-realistic.uci")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, emptyMigrationBase()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), encoded.Bytes()...)
	daemonOwner, err := acquireStoreTransactionLock(context.Background(), socketPath+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer daemonOwner.Close()
	_, err = MigrateV1Files(context.Background(), sourcePath, targetPath, backupDir, markerPath, socketPath, time.Now().UTC())
	if err == nil {
		t.Fatal("migration accepted a running daemon")
	}
	after, readErr := os.ReadFile(targetPath)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("online migration changed target: %v", readErr)
	}
	if _, markerErr := os.Lstat(markerPath); !errors.Is(markerErr, os.ErrNotExist) {
		t.Fatalf("online migration created marker: %v", markerErr)
	}
}

func TestMigrateV1FilesRejectsDaemonAndConfigLockPathCollision(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "proxypool")
	targetPath := filepath.Join(directory, "proxypool_v2")
	markerPath := filepath.Join(directory, "migration.json")
	source, err := os.ReadFile("testdata/v1-realistic.uci")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, emptyMigrationBase()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = MigrateV1Files(ctx, sourcePath, targetPath, filepath.Join(directory, "backups"), markerPath, targetPath, time.Now().UTC())
	if err == nil {
		t.Fatal("migration accepted colliding daemon and configuration lock paths")
	}
	if _, markerErr := os.Lstat(markerPath); !errors.Is(markerErr, os.ErrNotExist) {
		t.Fatalf("lock-path collision created marker: %v", markerErr)
	}
}

func emptyMigrationBase() model.DesiredConfig {
	cfg := validConfig()
	cfg.Nodes = map[string]model.Node{}
	cfg.Devices = map[string]model.Device{}
	cfg.PendingBindings = map[string]model.PendingBinding{}
	return cfg
}
