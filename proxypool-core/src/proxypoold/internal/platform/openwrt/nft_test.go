package openwrt

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

func TestAuthorizerPublishesExactExpiringL2TPTupleAndReadsItBack(t *testing.T) {
	runner := &inputRecordingRunner{}
	lease := platform.AuthorizationLease{
		NodeID: "node_a", MAC: "AA:BB:CC:DD:EE:01", IPv4: netip.MustParseAddr("192.168.9.22"),
		PolicyID: 42, Generation: 9, Protocol: model.ProtocolL2TP, Interface: "l2tp-ppv20042",
	}
	runner.readback = authorizerReadback(lease)
	authorizer := NewAuthorizer(runner, filepath.Join(t.TempDir(), "leases.json"))
	if err := authorizer.Publish(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	inputs := append([]string(nil), runner.inputs...)
	args := append([]string(nil), runner.inputArgs...)
	runner.mu.Unlock()
	if len(inputs) != 2 || !strings.Contains(args[0], "-c -f -") || !strings.Contains(args[1], "-f -") {
		t.Fatalf("nft check/apply calls = %q args=%q", inputs, args)
	}
	transaction := inputs[1]
	for _, exact := range []string{
		"add element inet proxypool_guard v2_policy_marks { aa:bb:cc:dd:ee:01 . 192.168.9.22 timeout 20s : 0x005a002a }",
		"add element inet proxypool_guard v2_dns_clients { aa:bb:cc:dd:ee:01 . 192.168.9.22 timeout 20s }",
		`add element inet proxypool_guard v2_l2tp_paths { aa:bb:cc:dd:ee:01 . 192.168.9.22 . "l2tp-ppv20042" timeout 20s }`,
		`add element inet proxypool_guard v2_l2tp_return_paths { 192.168.9.22 . "l2tp-ppv20042" timeout 20s }`,
	} {
		if !strings.Contains(transaction, exact) {
			t.Fatalf("transaction is missing %q:\n%s", exact, transaction)
		}
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(authorizer.manifestPath), filepath.Base(authorizer.manifestPath)))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("manifest mode/error = %v/%v", info, err)
	}
}

func TestAuthorizerPublishesExpiringSOCKS5RedirectAndCounterElements(t *testing.T) {
	runner := &inputRecordingRunner{}
	lease := platform.AuthorizationLease{
		NodeID: "node_proxy", MAC: "AA:BB:CC:DD:EE:02", IPv4: netip.MustParseAddr("192.168.9.23"),
		PolicyID: 2, Generation: 10, Protocol: model.ProtocolSOCKS5, Interface: "psx0002", RedirectPort: 12002,
	}
	runner.readback = []byte(strings.Join([]string{
		"aa:bb:cc:dd:ee:02", "192.168.9.23", "12002", "0x005a0002", "v2_policy_marks", "v2_dns_clients",
		"v2_tcp_redirect_ports", "v2_tcp_redirects", "v2_proxy_uploads", "v2_proxy_downloads",
	}, " "))
	authorizer := NewAuthorizer(runner, filepath.Join(t.TempDir(), "leases.json"))
	if err := authorizer.Publish(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	transaction := runner.inputs[1]
	runner.mu.Unlock()
	for _, exact := range []string{
		"add element inet proxypool_guard v2_tcp_redirect_ports { aa:bb:cc:dd:ee:02 . 192.168.9.23 timeout 20s : 12002 }",
		"add element inet proxypool_guard v2_tcp_redirects { aa:bb:cc:dd:ee:02 . 192.168.9.23 . 12002 timeout 20s }",
		"add element inet proxypool_guard v2_proxy_uploads { aa:bb:cc:dd:ee:02 . 192.168.9.23 . 0x005a0002 timeout 20s }",
		"add element inet proxypool_guard v2_proxy_downloads { 192.168.9.23 . 12002 timeout 20s }",
	} {
		if !strings.Contains(transaction, exact) {
			t.Fatalf("transaction is missing %q:\n%s", exact, transaction)
		}
	}
}

func TestAuthorizerRejectsStaleGenerationWithoutCallingNft(t *testing.T) {
	runner := &inputRecordingRunner{}
	lease := validAuthorizationLease()
	runner.readback = authorizerReadback(lease)
	path := filepath.Join(t.TempDir(), "leases.json")
	authorizer := NewAuthorizer(runner, path)
	if err := authorizer.Publish(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	before := len(runner.inputs)
	runner.mu.Unlock()
	lease.Generation--
	authorizer = NewAuthorizer(runner, path)
	if err := authorizer.Publish(context.Background(), lease); err == nil {
		t.Fatal("stale generation was accepted")
	}
	runner.mu.Lock()
	after := len(runner.inputs)
	runner.mu.Unlock()
	if after != before {
		t.Fatal("stale generation reached nft")
	}
}

func TestAuthorizerRefreshRecreatesExistingTimedElementsAtomically(t *testing.T) {
	runner := &inputRecordingRunner{}
	lease := validAuthorizationLease()
	runner.readback = authorizerReadback(lease)
	authorizer := NewAuthorizer(runner, filepath.Join(t.TempDir(), "leases.json"))
	if err := authorizer.Publish(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.Publish(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	refresh := runner.inputs[3]
	runner.mu.Unlock()
	for _, name := range []string{"v2_policy_marks", "v2_dns_clients", "v2_l2tp_paths", "v2_l2tp_return_paths"} {
		deleteLine := "delete element inet proxypool_guard " + name
		addLine := "add element inet proxypool_guard " + name
		if !strings.Contains(refresh, deleteLine) || !strings.Contains(refresh, addLine) || strings.Index(refresh, deleteLine) > strings.Index(refresh, addLine) {
			t.Fatalf("refresh did not atomically recreate %s:\n%s", name, refresh)
		}
	}
}

func TestAuthorizerApplyFailureDoesNotPublishManifest(t *testing.T) {
	runner := &inputRecordingRunner{failApply: true}
	path := filepath.Join(t.TempDir(), "leases.json")
	if err := NewAuthorizer(runner, path).Publish(context.Background(), validAuthorizationLease()); err == nil {
		t.Fatal("nft failure was accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed publish left manifest: %v", err)
	}
}

func TestAuthorizerRejectsInvalidTupleBeforeNft(t *testing.T) {
	for _, mutate := range []func(*platform.AuthorizationLease){
		func(lease *platform.AuthorizationLease) { lease.MAC = "bad; flush ruleset" },
		func(lease *platform.AuthorizationLease) { lease.IPv4 = netip.MustParseAddr("8.8.8.8") },
		func(lease *platform.AuthorizationLease) { lease.PolicyID = 0 },
		func(lease *platform.AuthorizationLease) { lease.Generation = 0 },
		func(lease *platform.AuthorizationLease) { lease.Interface = `ppp0\"; flush ruleset` },
	} {
		runner := &inputRecordingRunner{}
		lease := validAuthorizationLease()
		mutate(&lease)
		if err := NewAuthorizer(runner, filepath.Join(t.TempDir(), "leases.json")).Publish(context.Background(), lease); err == nil {
			t.Fatal("invalid authorization lease was accepted")
		}
		if len(runner.inputs) != 0 {
			t.Fatal("invalid authorization lease reached nft")
		}
	}
}

func validAuthorizationLease() platform.AuthorizationLease {
	return platform.AuthorizationLease{
		NodeID: "node_a", MAC: "aa:bb:cc:dd:ee:01", IPv4: netip.MustParseAddr("192.168.9.22"),
		PolicyID: 42, Generation: 9, Protocol: model.ProtocolL2TP, Interface: "l2tp-ppv20042",
	}
}

func authorizerReadback(lease platform.AuthorizationLease) []byte {
	return []byte(strings.Join([]string{
		strings.ToLower(lease.MAC), lease.IPv4.String(), lease.Interface, "0x005a002a",
		"v2_policy_marks", "v2_dns_clients", "v2_l2tp_paths", "v2_l2tp_return_paths",
	}, " "))
}

type inputRecordingRunner struct {
	mu        sync.Mutex
	inputs    []string
	inputArgs []string
	readback  []byte
	failApply bool
	published bool
}

func (runner *inputRecordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "/usr/sbin/nft" || !strings.HasPrefix(strings.Join(args, " "), "-nn get element inet proxypool_guard ") {
		return nil, errors.New("unexpected command")
	}
	runner.mu.Lock()
	published := runner.published
	runner.mu.Unlock()
	if !published {
		return nil, errors.New("element absent")
	}
	return []byte(strings.Join(args, " ") + " " + string(runner.readback)), nil
}

func (runner *inputRecordingRunner) RunInput(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.inputs = append(runner.inputs, string(input))
	runner.inputArgs = append(runner.inputArgs, strings.Join(args, " "))
	if name != "/usr/sbin/nft" {
		return nil, errors.New("unexpected executable")
	}
	if runner.failApply && strings.Join(args, " ") == "-f -" {
		return nil, errors.New("apply failed")
	}
	if strings.Join(args, " ") == "-f -" {
		runner.published = true
	}
	return nil, nil
}

var _ platform.InputCommandRunner = (*inputRecordingRunner)(nil)
