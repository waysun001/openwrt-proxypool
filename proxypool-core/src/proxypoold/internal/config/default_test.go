package config

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackagedDefaultRemainsTheExactLegacyV1UpgradeBaseline(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "files", "proxypool.config")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspection := Classify(contents)
	if inspection.State() != ConfigMigrationRequired || inspection.StartupClass() != StartupV1 {
		t.Fatalf("packaged default state/class = %q/%q", inspection.State(), inspection.StartupClass())
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(contents)); got != "00f37918933d1e7a66fc0b83b7791c164e15ea835a7fa6bee5761701f9291958" {
		t.Fatalf("packaged V1 baseline bytes changed: sha256=%s", got)
	}
}

func TestImageBuilderDefaultQueuesColdV2ActivationFromStrictV1Baseline(t *testing.T) {
	overlayRoot := filepath.Join("..", "..", "..", "..", "..", "files", "etc", "config")
	legacyContents, err := os.ReadFile(filepath.Join(overlayRoot, "proxypool"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(legacyContents)); got != "00f37918933d1e7a66fc0b83b7791c164e15ea835a7fa6bee5761701f9291958" {
		t.Fatalf("ImageBuilder legacy rollback config changed: sha256=%s", got)
	}
	selector := InspectRuntimeSelectorFile(filepath.Join(overlayRoot, "proxypool_runtime"))
	if selector != RuntimeSelectionV1 {
		t.Fatalf("ImageBuilder selector=%q", selector)
	}
	activationRequestPath := filepath.Join("..", "..", "..", "..", "..", "files", "etc", "proxypool", "v2-activation-request")
	activationRequest, err := os.ReadFile(activationRequestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(activationRequest) != "image\n" {
		t.Fatalf("ImageBuilder V2 activation request=%q", activationRequest)
	}
	contents, err := os.ReadFile(filepath.Join(overlayRoot, "proxypool_v2"))
	if err != nil {
		t.Fatal(err)
	}
	inspection := Classify(contents)
	if inspection.State() != ConfigReady || inspection.StartupClass() != StartupV2Shadow {
		t.Fatalf("ImageBuilder default state/class = %q/%q", inspection.State(), inspection.StartupClass())
	}
	desired, ok := inspection.Desired()
	if !ok {
		t.Fatal("ImageBuilder default did not decode")
	}
	if desired.Global.RuntimeBackend != "v2_shadow" || len(desired.Nodes) != 0 || len(desired.Devices) != 0 {
		t.Fatalf("ImageBuilder default must be empty V2 shadow: backend=%q nodes=%d devices=%d", desired.Global.RuntimeBackend, len(desired.Nodes), len(desired.Devices))
	}
	if len(desired.Global.DoHEndpoints) != 2 {
		t.Fatalf("ImageBuilder default DNS endpoint count = %d, want 2", len(desired.Global.DoHEndpoints))
	}
	primary, backup := desired.Global.DoHEndpoints[0], desired.Global.DoHEndpoints[1]
	if primary.URL != "https://dns.alidns.com/dns-query" || primary.BootstrapIP != "223.5.5.5" || primary.ServerName != "dns.alidns.com" {
		t.Fatalf("ImageBuilder primary DNS endpoint = %#v", primary)
	}
	if backup.URL != "https://dns.alidns.com/dns-query" || backup.BootstrapIP != "223.6.6.6" || backup.ServerName != "dns.alidns.com" {
		t.Fatalf("ImageBuilder backup DNS endpoint = %#v", backup)
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
