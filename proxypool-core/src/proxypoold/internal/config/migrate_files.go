package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"proxypoold/internal/model"
)

type FileMigrationResult struct {
	MigrationResult
	BackupPath     string
	TargetRevision uint64
	AlreadyApplied bool
}

type migrationMarker struct {
	SchemaVersion  int       `json:"schema_version"`
	Status         string    `json:"status"`
	SourceSHA256   string    `json:"source_sha256"`
	TargetSHA256   string    `json:"target_sha256"`
	BaseRevision   uint64    `json:"base_revision"`
	TargetRevision uint64    `json:"target_revision"`
	BackupPath     string    `json:"backup_path"`
	CreatedAt      time.Time `json:"created_at"`
}

// MigrateV1Files backs up the legacy source, writes a pending transaction
// marker, atomically replaces the V2 target, then commits the marker. A retry
// can finish an interrupted post-rename transaction without importing twice.
func MigrateV1Files(ctx context.Context, sourcePath, targetPath, backupDir, markerPath, daemonSocketPath string, now time.Time) (FileMigrationResult, error) {
	if contextError(ctx) != nil || now.IsZero() || !allAbsolutePaths(sourcePath, targetPath, backupDir, markerPath, daemonSocketPath) ||
		!allMigrationPathsDistinct(sourcePath, targetPath, markerPath, daemonSocketPath) {
		return FileMigrationResult{}, errors.New("migration file request is invalid")
	}
	if !pathUsesNoSymlink(sourcePath) || !pathUsesNoSymlink(targetPath) {
		return FileMigrationResult{}, errors.New("migration file path is unsafe")
	}
	source, err := readMigrationFile(sourcePath, maxLegacyConfigBytes)
	if err != nil {
		return FileMigrationResult{}, errors.New("legacy migration source is unsafe")
	}
	migrationTransaction, err := acquireMigrationTransactionLock(ctx, targetPath)
	if err != nil {
		return FileMigrationResult{}, errors.New("migration transaction lock failed")
	}
	defer migrationTransaction.Close()
	daemonGuard, err := tryAcquireStoreTransactionLock(daemonSocketPath + ".lock")
	if err != nil {
		return FileMigrationResult{}, errors.New("migration requires the V2 daemon to be stopped")
	}
	defer daemonGuard.Close()
	store := NewStore(targetPath)
	current, err := store.Load()
	if err != nil {
		return FileMigrationResult{}, errors.New("V2 migration target is unavailable")
	}
	sourceSHA256 := sha256Text(source)
	if marker, exists, err := loadMigrationMarker(markerPath); err != nil {
		return FileMigrationResult{}, err
	} else if exists {
		if err := verifyMigrationMarker(marker, sourceSHA256); err != nil {
			return FileMigrationResult{}, err
		}
		if marker.Status == "pending" && current.Revision == marker.BaseRevision {
			preview, err := MigrateV1(source, current, nil, marker.CreatedAt)
			if err != nil {
				return FileMigrationResult{}, err
			}
			expected := withRevision(current, cloneConfig(preview.Config), marker.TargetRevision)
			digest, err := configSHA256(expected)
			if err != nil || digest != marker.TargetSHA256 {
				return FileMigrationResult{}, errors.New("migration marker target does not match preview")
			}
			return finishPendingMigration(ctx, store, current, preview, marker, markerPath)
		}
		return resumeMigrationMarker(ctx, store, current, MigrationResult{SourceSHA256: sourceSHA256}, marker, markerPath)
	}
	preview, err := MigrateV1(source, current, nil, now.UTC().Round(0))
	if err != nil {
		return FileMigrationResult{}, err
	}
	if current.Revision == ^uint64(0) {
		return FileMigrationResult{}, errors.New("V2 migration target revision is exhausted")
	}
	expected := withRevision(current, cloneConfig(preview.Config), current.Revision+1)
	targetSHA256, err := configSHA256(expected)
	if err != nil {
		return FileMigrationResult{}, errors.New("V2 migration target encoding failed")
	}
	if err := ensureMigrationDirectory(backupDir); err != nil {
		return FileMigrationResult{}, err
	}
	backupPath := filepath.Join(backupDir, fmt.Sprintf("proxypool-v1-%s-%s.uci", now.UTC().Format("20060102T150405Z"), preview.SourceSHA256[:12]))
	if err := ensureMigrationBackup(backupPath, source); err != nil {
		return FileMigrationResult{}, err
	}
	marker := migrationMarker{
		SchemaVersion: 1, Status: "pending", SourceSHA256: preview.SourceSHA256, TargetSHA256: targetSHA256,
		BaseRevision: current.Revision, TargetRevision: current.Revision + 1, BackupPath: backupPath,
		CreatedAt: now.UTC().Round(0),
	}
	if err := writeMigrationMarker(markerPath, marker); err != nil {
		return FileMigrationResult{}, err
	}
	return finishPendingMigration(ctx, store, current, preview, marker, markerPath)
}

func allMigrationPathsDistinct(sourcePath, targetPath, markerPath, daemonSocketPath string) bool {
	paths := []string{
		sourcePath, targetPath, markerPath, daemonSocketPath,
		targetPath + ".lock", targetPath + ".migration.lock", daemonSocketPath + ".lock",
	}
	for left := range paths {
		for right := left + 1; right < len(paths); right++ {
			if migrationPathsEqual(paths[left], paths[right]) {
				return false
			}
		}
	}
	return true
}

func acquireMigrationTransactionLock(ctx context.Context, targetPath string) (*storeTransactionLock, error) {
	return acquireStoreTransactionLock(ctx, targetPath+".migration.lock")
}

func finishPendingMigration(ctx context.Context, store *Store, current model.DesiredConfig, preview MigrationResult, marker migrationMarker, markerPath string) (FileMigrationResult, error) {
	stored, replaceErr := store.Replace(ctx, current.Revision, preview.Config)
	if replaceErr != nil {
		expected := withRevision(current, cloneConfig(preview.Config), marker.TargetRevision)
		if store.ConfirmDurable(ctx, expected) != nil {
			return FileMigrationResult{}, errors.New("V2 migration target persistence failed")
		}
		stored = expected
	}
	marker.Status = "committed"
	if err := writeMigrationMarker(markerPath, marker); err != nil {
		return FileMigrationResult{}, errors.New("migration commit marker persistence failed")
	}
	preview.Config = stored
	return FileMigrationResult{
		MigrationResult: preview, BackupPath: marker.BackupPath, TargetRevision: stored.Revision,
	}, nil
}

func resumeMigrationMarker(ctx context.Context, store *Store, current model.DesiredConfig, preview MigrationResult, marker migrationMarker, markerPath string) (FileMigrationResult, error) {
	if err := verifyMigrationMarker(marker, preview.SourceSHA256); err != nil {
		return FileMigrationResult{}, err
	}
	if marker.Status == "committed" {
		if current.Revision < marker.TargetRevision {
			return FileMigrationResult{}, errors.New("migration target is older than committed marker")
		}
		preview.Config = current
		return FileMigrationResult{
			MigrationResult: preview, BackupPath: marker.BackupPath, TargetRevision: marker.TargetRevision, AlreadyApplied: true,
		}, nil
	}
	if current.Revision != marker.TargetRevision {
		return FileMigrationResult{}, errors.New("migration marker revision does not match target")
	}
	targetSHA256, err := configSHA256(current)
	if err != nil || targetSHA256 != marker.TargetSHA256 {
		return FileMigrationResult{}, errors.New("migration marker target content does not match")
	}
	backup, err := readMigrationFile(marker.BackupPath, maxLegacyConfigBytes)
	if err != nil || sha256Text(backup) != marker.SourceSHA256 {
		return FileMigrationResult{}, errors.New("migration backup verification failed")
	}
	if err := store.ConfirmDurable(ctx, current); err != nil {
		return FileMigrationResult{}, errors.New("migration target durability is uncertain")
	}
	marker.Status = "committed"
	if err := writeMigrationMarker(markerPath, marker); err != nil {
		return FileMigrationResult{}, errors.New("migration commit marker persistence failed")
	}
	preview.Config = current
	return FileMigrationResult{
		MigrationResult: preview, BackupPath: marker.BackupPath, TargetRevision: current.Revision, AlreadyApplied: true,
	}, nil
}

func verifyMigrationMarker(marker migrationMarker, sourceSHA256 string) error {
	if marker.SchemaVersion != 1 || marker.SourceSHA256 != sourceSHA256 || len(marker.TargetSHA256) != 64 || marker.CreatedAt.IsZero() ||
		marker.CreatedAt.Location() != time.UTC || marker.BackupPath == "" || !filepath.IsAbs(marker.BackupPath) ||
		(marker.Status != "pending" && marker.Status != "committed") || marker.BaseRevision == ^uint64(0) || marker.TargetRevision != marker.BaseRevision+1 {
		return errors.New("migration marker conflicts with source")
	}
	backup, err := readMigrationFile(marker.BackupPath, maxLegacyConfigBytes)
	if err != nil || sha256Text(backup) != marker.SourceSHA256 {
		return errors.New("migration backup verification failed")
	}
	return nil
}

func loadMigrationMarker(path string) (migrationMarker, bool, error) {
	contents, err := readMigrationFile(path, 4096)
	if errors.Is(err, os.ErrNotExist) {
		return migrationMarker{}, false, nil
	}
	if err != nil {
		return migrationMarker{}, false, errors.New("migration marker is unsafe")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var marker migrationMarker
	if err := decoder.Decode(&marker); err != nil {
		return migrationMarker{}, false, errors.New("migration marker is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return migrationMarker{}, false, errors.New("migration marker is invalid")
	}
	return marker, true, nil
}

func writeMigrationMarker(path string, marker migrationMarker) error {
	encoded, err := json.Marshal(marker)
	if err != nil {
		return errors.New("migration marker encoding failed")
	}
	encoded = append(encoded, '\n')
	if err := writePrivateAtomic(path, encoded); err != nil {
		return errors.New("migration marker persistence failed")
	}
	return nil
}

func ensureMigrationBackup(path string, source []byte) error {
	if existing, err := readMigrationFile(path, maxLegacyConfigBytes); err == nil {
		if bytes.Equal(existing, source) {
			return nil
		}
		return errors.New("migration backup path already contains different data")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("migration backup path is unsafe")
	}
	if err := writePrivateAtomic(path, source); err != nil {
		return errors.New("migration backup persistence failed")
	}
	return nil
}

func readMigrationFile(path string, maximum int64) ([]byte, error) {
	if !pathUsesNoSymlink(path) {
		return nil, errors.New("migration file path is unsafe")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maximum {
		return nil, errors.New("migration file is unsafe")
	}
	contents, err := os.ReadFile(path)
	if err != nil || int64(len(contents)) > maximum {
		return nil, errors.New("migration file read failed")
	}
	return contents, nil
}

func writePrivateAtomic(path string, contents []byte) error {
	directory := filepath.Dir(path)
	if err := ensureMigrationDirectory(directory); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("migration destination is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("migration destination inspection failed")
	}
	file, err := os.CreateTemp(directory, ".proxypool-migration-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		_ = os.Remove(temporary)
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncMigrationDirectory(directory)
}

func ensureMigrationDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("migration directory must be absolute")
	}
	if !existingMigrationAncestorIsSafe(path) {
		return errors.New("migration directory path is unsafe")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return errors.New("migration directory creation failed")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("migration directory is unsafe")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return errors.New("migration directory permission failed")
	}
	if !pathUsesNoSymlink(path) {
		return errors.New("migration directory path is unsafe")
	}
	return nil
}

func existingMigrationAncestorIsSafe(path string) bool {
	current := filepath.Clean(path)
	for {
		if _, err := os.Lstat(current); err == nil {
			return pathUsesNoSymlink(current)
		} else if !errors.Is(err, os.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

func syncMigrationDirectory(path string) error {
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

func allAbsolutePaths(paths ...string) bool {
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			return false
		}
	}
	return true
}

func pathUsesNoSymlink(path string) bool {
	if runtime.GOOS == "windows" {
		return windowsPathUsesNoSymlink(path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(path)
		resolved, err = filepath.EvalSymlinks(parent)
		if err != nil {
			return false
		}
		return migrationPathsEqual(resolved, parent)
	}
	if err != nil {
		return false
	}
	return migrationPathsEqual(resolved, path)
}

func windowsPathUsesNoSymlink(path string) bool {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	current := volume + string(os.PathSeparator)
	remainder := strings.TrimPrefix(clean, current)
	for _, component := range strings.Split(remainder, string(os.PathSeparator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func migrationPathsEqual(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func sha256Text(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func configSHA256(cfg model.DesiredConfig) (string, error) {
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		return "", err
	}
	return sha256Text(encoded.Bytes()), nil
}
