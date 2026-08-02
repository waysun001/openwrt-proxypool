package diagnostics

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadManagedProcessMetadataUsesOnlyAllowlistedProcFields(t *testing.T) {
	root := t.TempDir()
	process := filepath.Join(root, "123")
	if err := os.MkdirAll(filepath.Join(process, "fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(process, "comm"), []byte("xl2tpd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := "Name:\txl2tpd\nState:\tS (sleeping)\nPid:\t123\nPPid:\t1\nVmRSS:\t2048 kB\nThreads:\t2\nSecret:\tpassword-not-allowed\n"
	if err := os.WriteFile(filepath.Join(process, "status"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(process, "cmdline"), []byte("xl2tpd\x00--password\x00argv-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(process, "fd", "1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "124"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "124", "comm"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output := ReadManagedProcessMetadata(root)
	for _, want := range []string{`"pid":123`, `"name":"xl2tpd"`, `"rss_kib":2048`, `"fd_count":1`} {
		if !bytes.Contains(output, []byte(want)) {
			t.Fatalf("metadata %s missing from %s", want, output)
		}
	}
	for _, forbidden := range []string{"password-not-allowed", "argv-secret", "unrelated"} {
		if bytes.Contains(output, []byte(forbidden)) {
			t.Fatalf("metadata leaked %s", forbidden)
		}
	}
}
