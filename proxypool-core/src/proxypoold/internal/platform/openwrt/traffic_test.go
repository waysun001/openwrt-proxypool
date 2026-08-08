package openwrt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInterfaceTrafficReaderReadsSysfsCounters(t *testing.T) {
	root := t.TempDir()
	writeTrafficCounters(t, root, "l2tp-ppv20001", "123\n", "456\n")
	reader := NewSysfsTrafficReader(root)
	counters, err := reader.ReadInterfaceCounters("l2tp-ppv20001")
	if err != nil {
		t.Fatalf("ReadInterfaceCounters() error = %v", err)
	}
	if counters.RXBytes != 123 || counters.TXBytes != 456 {
		t.Fatalf("ReadInterfaceCounters() = %#v", counters)
	}
}

func TestInterfaceTrafficReaderRejectsUnsafeOrInvalidCounters(t *testing.T) {
	tests := []struct {
		name          string
		interfaceName string
		rx            string
		tx            string
		omitTX        bool
	}{
		{name: "path traversal", interfaceName: "../escape", rx: "1", tx: "2"},
		{name: "interface name too long", interfaceName: "abcdefghijklmnop", rx: "1", tx: "2"},
		{name: "missing counter", interfaceName: "ppp0", rx: "1", omitTX: true},
		{name: "negative counter", interfaceName: "ppp0", rx: "-1", tx: "2"},
		{name: "non decimal counter", interfaceName: "ppp0", rx: "one", tx: "2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.interfaceName == "../escape" || len(test.interfaceName) > 15 {
				if _, err := NewSysfsTrafficReader(root).ReadInterfaceCounters(test.interfaceName); err == nil {
					t.Fatal("ReadInterfaceCounters() error = nil")
				}
				return
			}
			statistics := filepath.Join(root, test.interfaceName, "statistics")
			if err := os.MkdirAll(statistics, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(statistics, "rx_bytes"), []byte(test.rx), 0o600); err != nil {
				t.Fatal(err)
			}
			if !test.omitTX {
				if err := os.WriteFile(filepath.Join(statistics, "tx_bytes"), []byte(test.tx), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := NewSysfsTrafficReader(root).ReadInterfaceCounters(test.interfaceName); err == nil {
				t.Fatal("ReadInterfaceCounters() error = nil")
			}
		})
	}
}

func TestInterfaceTrafficReaderRejectsSymlinkCounter(t *testing.T) {
	root := t.TempDir()
	statistics := filepath.Join(root, "ppp0", "statistics")
	if err := os.MkdirAll(statistics, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside")
	if err := os.WriteFile(target, []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(statistics, "rx_bytes")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statistics, "tx_bytes"), []byte("456"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSysfsTrafficReader(root).ReadInterfaceCounters("ppp0"); err == nil {
		t.Fatal("ReadInterfaceCounters() error = nil")
	}
}

func writeTrafficCounters(t *testing.T, root, interfaceName, rx, tx string) {
	t.Helper()
	statistics := filepath.Join(root, interfaceName, "statistics")
	if err := os.MkdirAll(statistics, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statistics, "rx_bytes"), []byte(rx), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statistics, "tx_bytes"), []byte(tx), 0o600); err != nil {
		t.Fatal(err)
	}
}
