package diagnostics

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var artifactTestTime = time.Date(2032, 4, 5, 6, 7, 8, 0, time.UTC)

func TestArtifactStoreWritesPrivateBoundedArchiveAndClaimsOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "diagnostics")
	store, err := NewArtifactStore(root,
		WithArtifactClock(func() time.Time { return artifactTestTime }),
		WithArtifactIDSource(func() string { return "diag-0123456789abcdef" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Write(context.Background(), []Entry{
		{Name: "status.json", Data: []byte(`{"state":"ready"}`)},
		{Name: "ip-rules.txt", Data: []byte("rule")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID != "diag-0123456789abcdef" || artifact.State != ArtifactReady || artifact.Size <= 0 || artifact.Filename != "proxypool-diagnostics-diag-0123456789abcdef.tar.gz" {
		t.Fatalf("artifact = %#v", artifact)
	}
	claim, err := store.Claim(artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Path == "" || claim.Filename != artifact.Filename || claim.Size != artifact.Size {
		t.Fatalf("claim = %#v", claim)
	}
	info, err := os.Lstat(claim.Path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("artifact mode = %v err=%v", info.Mode(), err)
	}
	if _, err := store.Claim(artifact.ID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("second claim error = %v", err)
	}
	entries := readArchive(t, claim.Path)
	if string(entries["status.json"]) != `{"state":"ready"}` || string(entries["ip-rules.txt"]) != "rule" {
		t.Fatalf("archive entries = %#v", entries)
	}
	if err := store.Release(artifact.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(claim.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released artifact remains: %v", err)
	}
}

func TestArtifactStoreRejectsUnsafeRootIDsEntriesAndSymlinkReplacement(t *testing.T) {
	if _, err := NewArtifactStore("relative"); err == nil {
		t.Fatal("relative artifact root accepted")
	}
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(parent, "linked")
	if err := os.Symlink(realParent, linkedParent); err == nil && runtime.GOOS != "windows" {
		if _, err := NewArtifactStore(filepath.Join(linkedParent, "diagnostics")); err == nil {
			t.Fatal("artifact root below a symlink parent accepted")
		}
	}
	root := filepath.Join(t.TempDir(), "diagnostics")
	store, err := NewArtifactStore(root, WithArtifactIDSource(func() string { return "../escape" }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(context.Background(), []Entry{{Name: "safe.txt", Data: []byte("x")}}); err == nil {
		t.Fatal("unsafe artifact id accepted")
	}

	store, err = NewArtifactStore(root, WithArtifactIDSource(func() string { return "diag-aaaaaaaaaaaaaaaa" }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(context.Background(), []Entry{{Name: "../escape", Data: []byte("x")}}); err == nil {
		t.Fatal("unsafe archive entry accepted")
	}
	artifact, err := store.Write(context.Background(), []Entry{{Name: "safe.txt", Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, artifact.ID+".tar.gz")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.Claim(artifact.ID); !errors.Is(err, ErrArtifactUnsafe) {
		t.Fatalf("symlink claim error = %v", err)
	}
}

func TestArtifactStoreExpiresAndCleansUnclaimedFiles(t *testing.T) {
	now := artifactTestTime
	root := filepath.Join(t.TempDir(), "diagnostics")
	store, err := NewArtifactStore(root,
		WithArtifactClock(func() time.Time { return now }),
		WithArtifactTTL(time.Minute),
		WithArtifactIDSource(func() string { return "diag-bbbbbbbbbbbbbbbb" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Write(context.Background(), []Entry{{Name: "safe.txt", Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := store.Claim(artifact.ID); !errors.Is(err, ErrArtifactExpired) {
		t.Fatalf("expired claim error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, artifact.ID+".tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired artifact remains: %v", err)
	}
}

func TestArtifactStoreRejectsUnboundedEntryCount(t *testing.T) {
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "diagnostics"), WithArtifactIDSource(func() string { return "diag-cccccccccccccccc" }))
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]Entry, maxArchiveEntries+1)
	if _, err := store.Write(context.Background(), entries); err == nil {
		t.Fatal("unbounded artifact entry count accepted")
	}
}

func TestArtifactStoreRemovesOrphansOnStartupAndCleanupAll(t *testing.T) {
	root := filepath.Join(t.TempDir(), "diagnostics")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(root, "diag-0123456789abcdef.tar.gz")
	temporary := filepath.Join(root, ".diagnostic-old")
	if err := os.WriteFile(orphan, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(root, WithArtifactIDSource(func() string { return "diag-dddddddddddddddd" }))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{orphan, temporary} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("startup orphan remains: %s", path)
		}
	}
	artifact, err := store.Write(context.Background(), []Entry{{Name: "safe.txt", Data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, artifact.ID+".tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("CleanupAll left an artifact")
	}
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	result := make(map[string][]byte)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		result[header.Name] = data
	}
	return result
}
