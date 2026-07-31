package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackagedDefaultIsStrictEmptyV2ShadowWithOperationalDNS(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "files", "proxypool.config")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspection := Classify(contents)
	if inspection.State() != ConfigReady {
		t.Fatalf("packaged default state = %q, want %q", inspection.State(), ConfigReady)
	}
	desired, ok := inspection.Desired()
	if !ok {
		t.Fatal("packaged default did not decode")
	}
	if desired.Global.RuntimeBackend != "v2_shadow" || len(desired.Nodes) != 0 || len(desired.Devices) != 0 {
		t.Fatalf("default must be an empty V2 shadow configuration: backend=%q nodes=%d devices=%d", desired.Global.RuntimeBackend, len(desired.Nodes), len(desired.Devices))
	}
	for _, endpoint := range desired.Global.DoHEndpoints {
		if strings.Contains(endpoint.URL, ".example") || strings.Contains(endpoint.ServerName, ".example") {
			t.Fatalf("documentation-only DNS hostname shipped as operational default: %#v", endpoint)
		}
		address, err := netip.ParseAddr(endpoint.BootstrapIP)
		if err != nil || address.IsPrivate() || address.IsLoopback() || address.IsUnspecified() || isDocumentationAddress(address) {
			t.Fatalf("non-operational DNS bootstrap address shipped as default: %q", endpoint.BootstrapIP)
		}
	}

	overlayPath := filepath.Join("..", "..", "..", "..", "..", "files", "etc", "config", "proxypool")
	if _, err := os.Lstat(overlayPath); !os.IsNotExist(err) {
		t.Fatalf("ImageBuilder must not override the INSTALL_CONF default: %v", err)
	}
}

func isDocumentationAddress(address netip.Addr) bool {
	prefixes := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
