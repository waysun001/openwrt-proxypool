package openwrt

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

func TestDeviceInventoryMergesLeaseAndIngressWithoutUserMACInput(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	leases := strings.Join([]string{
		"1893560000 00:11:22:33:44:55 192.168.9.10 phone client-a",
		"1 00:11:22:33:44:66 192.168.9.11 expired client-b",
		"0 AA:BB:CC:DD:EE:FF 192.168.9.12 * *",
	}, "\n") + "\n"
	path := writeLeaseFixture(t, leases)
	runner := &scriptedRunner{responses: []runnerResponse{
		{output: `{"up":true,"device":"br-lan","l3_device":"br-lan"}`},
		{output: `[{"mac":"00:11:22:33:44:55","dev":"wlan0","master":"br-lan"},{"mac":"aa:bb:cc:dd:ee:ff","dev":"lan2","master":"br-lan"}]`},
	}}
	inventory := NewDeviceInventory(path, runner, WithDeviceInventoryClock(func() time.Time { return now }))

	got, err := inventory.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []platform.DiscoveredDevice{
		{ID: "device_001122334455", MAC: "00:11:22:33:44:55", IPv4: netip.MustParseAddr("192.168.9.10"), Hostname: "phone", Ingress: "wlan0", LastSeen: now, Confirmed: true},
		{ID: "device_aabbccddeeff", MAC: "aa:bb:cc:dd:ee:ff", IPv4: netip.MustParseAddr("192.168.9.12"), Ingress: "lan2", LastSeen: now, Confirmed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}
	if len(runner.calls) != 2 || runner.calls[0].name != "/bin/ubus" || runner.calls[1].name != "/usr/sbin/bridge" {
		t.Fatalf("inventory calls = %#v", runner.calls)
	}
}

func TestDeviceInventorySanitizesHostnameAndRejectsAmbiguousIdentity(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	tests := []struct {
		name     string
		leases   string
		wantErr  bool
		wantHost string
	}{
		{name: "control hostname", leases: "0 00:11:22:33:44:55 192.168.9.10 bad\\x01name *\n", wantHost: ""},
		{name: "duplicate mac", leases: "0 00:11:22:33:44:55 192.168.9.10 one *\n0 00:11:22:33:44:55 192.168.9.11 two *\n", wantErr: true},
		{name: "duplicate ipv4", leases: "0 00:11:22:33:44:55 192.168.9.10 one *\n0 00:11:22:33:44:66 192.168.9.10 two *\n", wantErr: true},
		{name: "malformed mac", leases: "0 not-a-mac 192.168.9.10 one *\n", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeLeaseFixture(t, strings.ReplaceAll(test.leases, `\\x01`, string([]byte{1})))
			runner := &scriptedRunner{responses: []runnerResponse{
				{output: `{"up":true,"device":"br-lan","l3_device":"br-lan"}`},
				{output: `[{"mac":"00:11:22:33:44:55","dev":"lan1","master":"br-lan"}]`},
			}}
			got, err := NewDeviceInventory(path, runner, WithDeviceInventoryClock(func() time.Time { return now })).List(context.Background())
			if test.wantErr {
				if err == nil {
					t.Fatalf("List() = %#v, want error", got)
				}
				return
			}
			if err != nil || len(got) != 1 || got[0].Hostname != test.wantHost {
				t.Fatalf("List() = %#v, error=%v", got, err)
			}
		})
	}
}

func TestDeviceInventoryFailsClosedOnLANOrFDBUncertainty(t *testing.T) {
	path := writeLeaseFixture(t, "0 00:11:22:33:44:55 192.168.9.10 phone *\n")
	for name, responses := range map[string][]runnerResponse{
		"lan down":     {{output: `{"up":false,"device":"br-lan","l3_device":"br-lan"}`}},
		"wrong bridge": {{output: `{"up":true,"device":"eth0","l3_device":"eth0"}`}},
		"ubus failure": {{err: errors.New("failure")}},
		"duplicate ingress": {
			{output: `{"up":true,"device":"br-lan","l3_device":"br-lan"}`},
			{output: `[{"mac":"00:11:22:33:44:55","dev":"lan1","master":"br-lan"},{"mac":"00:11:22:33:44:55","dev":"wlan0","master":"br-lan"}]`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDeviceInventory(path, &scriptedRunner{responses: responses}).List(context.Background()); err == nil {
				t.Fatal("List() error = nil")
			}
		})
	}
}

func TestLeaseManagerAppliesExactOwnedReservationAndRechecksLease(t *testing.T) {
	device := discoveredModelDevice()
	source := &staticDeviceSource{devices: []platform.DiscoveredDevice{discoveredPlatformDevice()}}
	runner := &leaseRunner{}
	manager := NewLeaseManager(runner, source, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))
	if err := manager.Apply(context.Background(), device, 4); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	for _, want := range [][]string{
		{"-q", "set", "dhcp.proxypool_device_001122334455=host"},
		{"-q", "set", "dhcp.proxypool_device_001122334455.mac=00:11:22:33:44:55"},
		{"-q", "set", "dhcp.proxypool_device_001122334455.ip=192.168.9.10"},
		{"-q", "commit", "dhcp"},
	} {
		if !runner.hasCall("/sbin/uci", want) {
			t.Fatalf("missing exact UCI argv %q in %#v", want, runner.calls)
		}
	}
	if !runner.hasCall("/etc/init.d/dnsmasq", []string{"reload"}) || source.calls != 2 {
		t.Fatalf("reload/recheck missing: calls=%#v source_calls=%d", runner.calls, source.calls)
	}
	for _, call := range runner.calls {
		if call.name == "/bin/sh" || call.name == "/bin/ash" {
			t.Fatalf("lease manager invoked a shell: %#v", call)
		}
	}
}

func TestLeaseManagerRejectsConflictAndCleansNewReservationAfterReloadFailure(t *testing.T) {
	device := discoveredModelDevice()
	for _, test := range []struct {
		name       string
		show       string
		failReload bool
		wantDelete bool
	}{
		{name: "foreign ipv4", show: "dhcp.foreign=host\ndhcp.foreign.mac='00:11:22:33:44:99'\ndhcp.foreign.ip='192.168.9.10'\n"},
		{name: "reload failure", failReload: true, wantDelete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &leaseRunner{show: test.show, failReload: test.failReload}
			manager := NewLeaseManager(runner, &staticDeviceSource{devices: []platform.DiscoveredDevice{discoveredPlatformDevice()}}, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))
			if err := manager.Apply(context.Background(), device, 4); err == nil {
				t.Fatal("Apply() error = nil")
			}
			deleted := runner.hasCall("/sbin/uci", []string{"-q", "delete", "dhcp.proxypool_device_001122334455"})
			if deleted != test.wantDelete {
				t.Fatalf("cleanup delete=%t, want %t; calls=%#v", deleted, test.wantDelete, runner.calls)
			}
		})
	}
}

func TestLeaseManagerRejectsUnsafeOrUndiscoveredDeviceBeforeMutation(t *testing.T) {
	base := discoveredModelDevice()
	tests := []struct {
		name   string
		mutate func(*model.Device)
	}{
		{name: "router address", mutate: func(device *model.Device) { device.FixedIPv4 = netip.MustParseAddr("192.168.9.1") }},
		{name: "outside lan", mutate: func(device *model.Device) { device.FixedIPv4 = netip.MustParseAddr("192.168.8.10") }},
		{name: "unsafe id", mutate: func(device *model.Device) { device.ID = "../../bad" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := base
			test.mutate(&device)
			runner := &leaseRunner{}
			manager := NewLeaseManager(runner, &staticDeviceSource{devices: []platform.DiscoveredDevice{discoveredPlatformDevice()}}, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))
			if err := manager.Apply(context.Background(), device, 4); err == nil || len(runner.calls) != 0 {
				t.Fatalf("Apply() error=%v calls=%#v", err, runner.calls)
			}
		})
	}
}

func TestLeaseManagerRejectsPreexistingPendingDHCPDeltaWithoutMutation(t *testing.T) {
	runner := &leaseRunner{changes: "dhcp.lan.ignore='1'\n"}
	manager := NewLeaseManager(runner, &staticDeviceSource{devices: []platform.DiscoveredDevice{discoveredPlatformDevice()}}, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))
	if err := manager.Apply(context.Background(), discoveredModelDevice(), 4); err == nil {
		t.Fatal("Apply() error = nil")
	}
	for _, call := range runner.calls {
		if call.name == "/sbin/uci" && len(call.args) >= 2 && (call.args[1] == "set" || call.args[1] == "commit" || call.args[1] == "delete") {
			t.Fatalf("pending delta reached mutating command: %#v", call)
		}
	}
}

func TestLeaseManagerReplaceCommitsAndReloadsOnce(t *testing.T) {
	before := discoveredModelDevice()
	after := secondDiscoveredModelDevice()
	runner := newTransactionalLeaseRunner(before)
	source := &staticDeviceSource{devices: []platform.DiscoveredDevice{discoveredPlatformDevice(), secondDiscoveredPlatformDevice()}}
	manager := NewLeaseManager(runner, source, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))

	if err := manager.Replace(context.Background(), []model.Device{before}, []model.Device{after}, 4); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if runner.commits != 1 || runner.reloads != 1 {
		t.Fatalf("commits=%d reloads=%d, want 1/1; calls=%#v", runner.commits, runner.reloads, runner.calls)
	}
	if _, exists := runner.committed["proxypool_"+before.ID]; exists {
		t.Fatalf("removed reservation remains: %#v", runner.committed)
	}
	got, exists := runner.committed["proxypool_"+after.ID]
	want := uciHost{Type: "host", MAC: after.MAC, IP: after.FixedIPv4.String(), Name: after.Hostname}
	if !exists || got != want {
		t.Fatalf("replacement reservation = %#v exists=%t, want %#v", got, exists, want)
	}
}

func TestLeaseManagerReplaceAddsReservationWithoutDeletingMissingSection(t *testing.T) {
	after := secondDiscoveredModelDevice()
	runner := newTransactionalLeaseRunner()
	manager := NewLeaseManager(runner, &staticDeviceSource{devices: []platform.DiscoveredDevice{secondDiscoveredPlatformDevice()}}, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))

	if err := manager.Replace(context.Background(), nil, []model.Device{after}, 4); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if runner.hasCall("/sbin/uci", []string{"-q", "delete", "dhcp.proxypool_" + after.ID}) {
		t.Fatalf("new reservation attempted to delete a missing section: %#v", runner.calls)
	}
}

func TestLeaseManagerReplaceRejectsConflictBeforeMutation(t *testing.T) {
	before := discoveredModelDevice()
	after := secondDiscoveredModelDevice()
	runner := newTransactionalLeaseRunner(before)
	runner.committed["foreign"] = uciHost{Type: "host", MAC: "00:11:22:33:44:99", IP: after.FixedIPv4.String()}
	runner.resetWorking()
	manager := NewLeaseManager(runner, &staticDeviceSource{devices: []platform.DiscoveredDevice{secondDiscoveredPlatformDevice()}}, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))

	if err := manager.Replace(context.Background(), []model.Device{before}, []model.Device{after}, 4); err == nil {
		t.Fatal("Replace() error = nil")
	}
	if runner.commits != 0 || runner.reloads != 0 || !reflect.DeepEqual(runner.committed, runner.working) {
		t.Fatalf("conflict mutated DHCP: committed=%#v working=%#v calls=%#v", runner.committed, runner.working, runner.calls)
	}
}

func TestLeaseManagerReplaceRejectsDuplicateResultAddressBeforeMutation(t *testing.T) {
	left := secondDiscoveredModelDevice()
	right := thirdDiscoveredModelDevice()
	runner := newTransactionalLeaseRunner()
	manager := NewLeaseManager(runner, &staticDeviceSource{devices: []platform.DiscoveredDevice{secondDiscoveredPlatformDevice(), thirdDiscoveredPlatformDevice()}}, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))

	if err := manager.Replace(context.Background(), nil, []model.Device{left, right}, 4); err == nil {
		t.Fatal("Replace() error = nil")
	}
	if runner.commits != 0 || runner.reloads != 0 || len(runner.calls) != 0 {
		t.Fatalf("duplicate result reached DHCP: calls=%#v", runner.calls)
	}
}

func TestLeaseManagerReplaceRestoresTouchedSectionsAfterReloadFailure(t *testing.T) {
	before := discoveredModelDevice()
	after := secondDiscoveredModelDevice()
	runner := newTransactionalLeaseRunner(before)
	original := cloneUCIHosts(runner.committed)
	runner.failNextReload = true
	manager := NewLeaseManager(runner, &staticDeviceSource{devices: []platform.DiscoveredDevice{secondDiscoveredPlatformDevice()}}, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"))

	if err := manager.Replace(context.Background(), []model.Device{before}, []model.Device{after}, 4); err == nil {
		t.Fatal("Replace() error = nil")
	}
	if !reflect.DeepEqual(runner.committed, original) {
		t.Fatalf("reload rollback committed=%#v, want %#v", runner.committed, original)
	}
	if runner.commits != 2 || runner.reloads != 2 {
		t.Fatalf("rollback commits=%d reloads=%d, want 2/2", runner.commits, runner.reloads)
	}
}

type commandCall struct {
	name string
	args []string
}

type runnerResponse struct {
	output string
	err    error
}

type scriptedRunner struct {
	calls     []commandCall
	responses []runnerResponse
}

func (runner *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, commandCall{name: name, args: append([]string(nil), args...)})
	if len(runner.responses) == 0 {
		return nil, errors.New("unexpected command")
	}
	response := runner.responses[0]
	runner.responses = runner.responses[1:]
	return []byte(response.output), response.err
}

type staticDeviceSource struct {
	devices []platform.DiscoveredDevice
	err     error
	calls   int
}

func (source *staticDeviceSource) List(context.Context) ([]platform.DiscoveredDevice, error) {
	source.calls++
	return append([]platform.DiscoveredDevice(nil), source.devices...), source.err
}

type leaseRunner struct {
	calls      []commandCall
	show       string
	changes    string
	failReload bool
}

type transactionalLeaseRunner struct {
	calls          []commandCall
	committed      map[string]uciHost
	working        map[string]uciHost
	commits        int
	reloads        int
	failNextReload bool
}

func newTransactionalLeaseRunner(devices ...model.Device) *transactionalLeaseRunner {
	runner := &transactionalLeaseRunner{committed: make(map[string]uciHost)}
	for _, device := range devices {
		runner.committed["proxypool_"+device.ID] = uciHost{Type: "host", MAC: device.MAC, IP: device.FixedIPv4.String(), Name: device.Hostname}
	}
	runner.resetWorking()
	return runner
}

func (runner *transactionalLeaseRunner) resetWorking() {
	runner.working = cloneUCIHosts(runner.committed)
}

func (runner *transactionalLeaseRunner) hasCall(name string, args []string) bool {
	for _, call := range runner.calls {
		if call.name == name && reflect.DeepEqual(call.args, args) {
			return true
		}
	}
	return false
}

func cloneUCIHosts(input map[string]uciHost) map[string]uciHost {
	result := make(map[string]uciHost, len(input))
	for name, host := range input {
		result[name] = host
	}
	return result
}

func (runner *transactionalLeaseRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, commandCall{name: name, args: append([]string(nil), args...)})
	if name == "/etc/init.d/dnsmasq" && reflect.DeepEqual(args, []string{"reload"}) {
		runner.reloads++
		if runner.failNextReload {
			runner.failNextReload = false
			return nil, errors.New("injected reload failure")
		}
		return nil, nil
	}
	if name != "/sbin/uci" || len(args) < 2 || args[0] != "-q" {
		return nil, errors.New("unexpected command")
	}
	switch args[1] {
	case "changes":
		return nil, nil
	case "show":
		if len(args) == 3 && args[2] == "dhcp" {
			return []byte(renderUCIHosts(runner.working)), nil
		}
		if len(args) == 3 && strings.HasPrefix(args[2], "dhcp.") {
			section := strings.TrimPrefix(args[2], "dhcp.")
			host, exists := runner.working[section]
			if !exists {
				return nil, errors.New("section not found")
			}
			return []byte(renderUCIHosts(map[string]uciHost{section: host})), nil
		}
	case "set":
		if len(args) != 3 {
			break
		}
		assignment := args[2]
		left, value, found := strings.Cut(assignment, "=")
		if !found || !strings.HasPrefix(left, "dhcp.") {
			break
		}
		parts := strings.Split(strings.TrimPrefix(left, "dhcp."), ".")
		section := parts[0]
		host := runner.working[section]
		if len(parts) == 1 {
			host.Type = value
		} else {
			switch parts[1] {
			case "mac":
				host.MAC = value
			case "ip":
				host.IP = value
			case "name":
				host.Name = value
			}
		}
		runner.working[section] = host
		return nil, nil
	case "delete":
		if len(args) != 3 || !strings.HasPrefix(args[2], "dhcp.") {
			break
		}
		parts := strings.Split(strings.TrimPrefix(args[2], "dhcp."), ".")
		section := parts[0]
		if len(parts) == 1 {
			if _, exists := runner.working[section]; !exists {
				return nil, errors.New("section not found")
			}
			delete(runner.working, section)
		} else {
			host := runner.working[section]
			if parts[1] == "name" {
				host.Name = ""
			}
			runner.working[section] = host
		}
		return nil, nil
	case "commit":
		if len(args) == 3 && args[2] == "dhcp" {
			runner.committed = cloneUCIHosts(runner.working)
			runner.commits++
			return nil, nil
		}
	case "revert":
		if len(args) == 3 && args[2] == "dhcp" {
			runner.resetWorking()
			return nil, nil
		}
	}
	return nil, errors.New("unexpected uci command")
}

func renderUCIHosts(hosts map[string]uciHost) string {
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	var output strings.Builder
	for _, name := range names {
		host := hosts[name]
		output.WriteString("dhcp." + name + "=" + host.Type + "\n")
		if host.MAC != "" {
			output.WriteString("dhcp." + name + ".mac='" + host.MAC + "'\n")
		}
		if host.IP != "" {
			output.WriteString("dhcp." + name + ".ip='" + host.IP + "'\n")
		}
		if host.Name != "" {
			output.WriteString("dhcp." + name + ".name='" + host.Name + "'\n")
		}
	}
	return output.String()
}

func (runner *leaseRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := commandCall{name: name, args: append([]string(nil), args...)}
	runner.calls = append(runner.calls, call)
	if name == "/sbin/uci" && reflect.DeepEqual(args, []string{"-q", "changes", "dhcp"}) {
		return []byte(runner.changes), nil
	}
	if name == "/sbin/uci" && reflect.DeepEqual(args, []string{"-q", "show", "dhcp"}) {
		return []byte(runner.show), nil
	}
	if name == "/sbin/uci" && reflect.DeepEqual(args, []string{"-q", "show", "dhcp.proxypool_device_001122334455"}) {
		return []byte("dhcp.proxypool_device_001122334455=host\ndhcp.proxypool_device_001122334455.mac='00:11:22:33:44:55'\ndhcp.proxypool_device_001122334455.ip='192.168.9.10'\ndhcp.proxypool_device_001122334455.name='phone'\n"), nil
	}
	if name == "/etc/init.d/dnsmasq" && runner.failReload {
		runner.failReload = false
		return nil, errors.New("injected reload failure")
	}
	return nil, nil
}

func (runner *leaseRunner) hasCall(name string, args []string) bool {
	for _, call := range runner.calls {
		if call.name == name && reflect.DeepEqual(call.args, args) {
			return true
		}
	}
	return false
}

func writeLeaseFixture(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dhcp.leases")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func discoveredPlatformDevice() platform.DiscoveredDevice {
	return platform.DiscoveredDevice{ID: "device_001122334455", MAC: "00:11:22:33:44:55", IPv4: netip.MustParseAddr("192.168.9.10"), Hostname: "phone", Ingress: "lan1", LastSeen: time.Now(), Confirmed: true}
}

func discoveredModelDevice() model.Device {
	return model.Device{ID: "device_001122334455", MAC: "00:11:22:33:44:55", Hostname: "phone", FixedIPv4: netip.MustParseAddr("192.168.9.10"), NodeID: "node_a", Enabled: true}
}

func secondDiscoveredPlatformDevice() platform.DiscoveredDevice {
	return platform.DiscoveredDevice{ID: "device_001122334466", MAC: "00:11:22:33:44:66", IPv4: netip.MustParseAddr("192.168.9.11"), Hostname: "tablet", Ingress: "wlan0", LastSeen: time.Now(), Confirmed: true}
}

func secondDiscoveredModelDevice() model.Device {
	return model.Device{ID: "device_001122334466", MAC: "00:11:22:33:44:66", Hostname: "tablet", FixedIPv4: netip.MustParseAddr("192.168.9.11"), NodeID: "node_b", Enabled: true}
}

func thirdDiscoveredPlatformDevice() platform.DiscoveredDevice {
	return platform.DiscoveredDevice{ID: "device_001122334477", MAC: "00:11:22:33:44:77", IPv4: netip.MustParseAddr("192.168.9.11"), Hostname: "watch", Ingress: "wlan0", LastSeen: time.Now(), Confirmed: true}
}

func thirdDiscoveredModelDevice() model.Device {
	return model.Device{ID: "device_001122334477", MAC: "00:11:22:33:44:77", Hostname: "watch", FixedIPv4: netip.MustParseAddr("192.168.9.11"), NodeID: "node_b", Enabled: true}
}
