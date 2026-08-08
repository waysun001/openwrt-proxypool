package openwrt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

const (
	maxLeaseFileBytes = 1 << 20
	maxLeaseDevices   = 512
)

type DeviceInventoryOption func(*DeviceInventory)

func WithDeviceInventoryClock(clock func() time.Time) DeviceInventoryOption {
	return func(inventory *DeviceInventory) {
		if clock != nil {
			inventory.now = clock
		}
	}
}

type DeviceInventory struct {
	leasesPath string
	runner     platform.CommandRunner
	now        func() time.Time
}

func NewDeviceInventory(leasesPath string, runner platform.CommandRunner, options ...DeviceInventoryOption) *DeviceInventory {
	inventory := &DeviceInventory{leasesPath: leasesPath, runner: runner, now: time.Now}
	for _, option := range options {
		if option != nil {
			option(inventory)
		}
	}
	return inventory
}

func (inventory *DeviceInventory) List(ctx context.Context) ([]platform.DiscoveredDevice, error) {
	if inventory == nil || inventory.runner == nil || inventory.leasesPath == "" {
		return nil, errors.New("device inventory is unavailable")
	}
	now := inventory.now().UTC().Round(0)
	leases, err := readDHCPLeases(inventory.leasesPath, now)
	if err != nil {
		return nil, errors.New("device lease inventory failed")
	}
	statusBytes, err := inventory.runner.Run(ctx, "/bin/ubus", "call", "network.interface.lan", "status")
	if err != nil || !validLANStatus(statusBytes) {
		return nil, errors.New("LAN inventory state is uncertain")
	}
	fdbBytes, err := inventory.runner.Run(ctx, "/usr/sbin/bridge", "-j", "fdb", "show", "br", "br-lan")
	if err != nil {
		return nil, errors.New("bridge inventory state is uncertain")
	}
	ingress, err := parseBridgeFDB(fdbBytes)
	if err != nil {
		return nil, errors.New("bridge inventory state is invalid")
	}
	devices := make([]platform.DiscoveredDevice, 0, len(leases))
	for _, lease := range leases {
		port := ingress[lease.MAC]
		if port == "" {
			port = "br-lan"
		}
		devices = append(devices, platform.DiscoveredDevice{
			ID: deviceIDForMAC(lease.MAC), MAC: lease.MAC, IPv4: lease.IPv4,
			Hostname: lease.Hostname, Ingress: port, LastSeen: now, Confirmed: true,
		})
	}
	sort.Slice(devices, func(left, right int) bool { return devices[left].ID < devices[right].ID })
	return devices, nil
}

type dhcpLease struct {
	MAC      string
	IPv4     netip.Addr
	Hostname string
}

func readDHCPLeases(path string, now time.Time) ([]dhcpLease, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return []dhcpLease{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxLeaseFileBytes {
		return nil, errors.New("lease path is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, maxLeaseFileBytes+1))
	scanner.Buffer(make([]byte, 1024), 64*1024)
	leases := make([]dhcpLease, 0)
	seenMAC := make(map[string]struct{})
	seenIPv4 := make(map[netip.Addr]struct{})
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			return nil, errors.New("lease line is malformed")
		}
		expires, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || expires < 0 {
			return nil, errors.New("lease expiry is malformed")
		}
		if expires != 0 && expires <= now.Unix() {
			continue
		}
		mac, err := normalizeMAC(fields[1])
		if err != nil {
			return nil, err
		}
		address, err := netip.ParseAddr(fields[2])
		if err != nil || !address.Is4() {
			return nil, errors.New("lease IPv4 is malformed")
		}
		if _, exists := seenMAC[mac]; exists {
			return nil, errors.New("lease MAC is ambiguous")
		}
		if _, exists := seenIPv4[address]; exists {
			return nil, errors.New("lease IPv4 is ambiguous")
		}
		seenMAC[mac] = struct{}{}
		seenIPv4[address] = struct{}{}
		leases = append(leases, dhcpLease{MAC: mac, IPv4: address, Hostname: sanitizeHostname(fields[3])})
		if len(leases) > maxLeaseDevices {
			return nil, errors.New("lease capacity exceeded")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return leases, nil
}

func validLANStatus(contents []byte) bool {
	var status struct {
		Up       bool   `json:"up"`
		Device   string `json:"device"`
		L3Device string `json:"l3_device"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if decoder.Decode(&status) != nil {
		return false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return false
	}
	return status.Up && status.Device == "br-lan" && status.L3Device == "br-lan"
}

func parseBridgeFDB(contents []byte) (map[string]string, error) {
	var entries []struct {
		MAC    string `json:"mac"`
		Dev    string `json:"dev"`
		Master string `json:"master"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(&entries); err != nil {
		return nil, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("trailing FDB data")
	}
	ingress := make(map[string]string)
	for _, entry := range entries {
		if entry.Master != "br-lan" || entry.Dev == "" || entry.Dev == "br-lan" {
			continue
		}
		mac, err := normalizeMAC(entry.MAC)
		if err != nil {
			continue
		}
		if current, exists := ingress[mac]; exists && current != entry.Dev {
			return nil, errors.New("device ingress is ambiguous")
		}
		ingress[mac] = entry.Dev
	}
	return ingress, nil
}

func normalizeMAC(value string) (string, error) {
	parsed, err := net.ParseMAC(value)
	if err != nil || len(parsed) != 6 {
		return "", errors.New("device MAC is invalid")
	}
	return strings.ToLower(parsed.String()), nil
}

func deviceIDForMAC(mac string) string {
	return "device_" + strings.NewReplacer(":", "", "-", "").Replace(strings.ToLower(mac))
}

func sanitizeHostname(value string) string {
	if value == "*" || value == "" || len(value) > 63 || !utf8.ValidString(value) {
		return ""
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return ""
		}
	}
	return value
}

type LeaseManager struct {
	runner platform.CommandRunner
	source platform.DeviceSource
	lan    netip.Prefix
	router netip.Addr
}

func NewLeaseManager(runner platform.CommandRunner, source platform.DeviceSource, lan netip.Prefix, router netip.Addr) *LeaseManager {
	return &LeaseManager{runner: runner, source: source, lan: lan.Masked(), router: router}
}

func (manager *LeaseManager) Apply(ctx context.Context, device model.Device, revision uint64) error {
	if err := manager.validateDevice(device, revision); err != nil {
		return err
	}
	if err := manager.ensureNoPendingDHCP(ctx); err != nil {
		return err
	}
	if err := manager.confirmDevice(ctx, device); err != nil {
		return err
	}
	contents, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "show", "dhcp")
	if err != nil {
		return errors.New("DHCP reservation inventory failed")
	}
	section := "proxypool_" + device.ID
	hosts, err := parseUCIHosts(contents)
	if err != nil {
		return errors.New("DHCP reservation inventory is invalid")
	}
	for name, host := range hosts {
		if name == section {
			continue
		}
		if host.MAC == device.MAC || host.IP == device.FixedIPv4.String() {
			return errors.New("DHCP reservation conflicts with another host")
		}
	}
	_, existed := hosts[section]
	commands := [][]string{
		{"-q", "set", "dhcp." + section + "=host"},
		{"-q", "set", "dhcp." + section + ".mac=" + device.MAC},
		{"-q", "set", "dhcp." + section + ".ip=" + device.FixedIPv4.String()},
	}
	if hostname := sanitizeHostname(device.Hostname); hostname != "" {
		commands = append(commands, []string{"-q", "set", "dhcp." + section + ".name=" + hostname})
	} else {
		commands = append(commands, []string{"-q", "delete", "dhcp." + section + ".name"})
	}
	for _, args := range commands {
		if _, err := manager.runner.Run(ctx, "/sbin/uci", args...); err != nil {
			_, _ = manager.runner.Run(context.Background(), "/sbin/uci", "-q", "revert", "dhcp")
			return errors.New("DHCP reservation staging failed")
		}
	}
	projection, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "show", "dhcp."+section)
	if err != nil || !exactOwnedProjection(projection, section, device) {
		_, _ = manager.runner.Run(context.Background(), "/sbin/uci", "-q", "revert", "dhcp")
		return errors.New("DHCP reservation verification failed")
	}
	if _, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "commit", "dhcp"); err != nil {
		_, _ = manager.runner.Run(context.Background(), "/sbin/uci", "-q", "revert", "dhcp")
		return errors.New("DHCP reservation commit failed")
	}
	if _, err := manager.runner.Run(ctx, "/etc/init.d/dnsmasq", "reload"); err != nil {
		if !existed {
			manager.cleanupNewReservation(section)
		}
		return errors.New("DHCP reservation reload failed")
	}
	if err := manager.confirmDevice(ctx, device); err != nil {
		if !existed {
			manager.cleanupNewReservation(section)
		}
		return errors.New("DHCP reservation live lease verification failed")
	}
	return nil
}

func (manager *LeaseManager) Remove(ctx context.Context, device model.Device, revision uint64) error {
	if err := manager.validateDevice(device, revision); err != nil {
		return err
	}
	if err := manager.ensureNoPendingDHCP(ctx); err != nil {
		return err
	}
	section := "proxypool_" + device.ID
	contents, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "show", "dhcp")
	if err != nil {
		return errors.New("DHCP reservation inventory failed")
	}
	hosts, err := parseUCIHosts(contents)
	if err != nil {
		return errors.New("DHCP reservation inventory is invalid")
	}
	host, exists := hosts[section]
	if !exists {
		return nil
	}
	if host.MAC != device.MAC || host.IP != device.FixedIPv4.String() {
		return errors.New("DHCP reservation ownership mismatch")
	}
	if _, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "delete", "dhcp."+section); err != nil {
		return errors.New("DHCP reservation removal failed")
	}
	if _, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "commit", "dhcp"); err != nil {
		return errors.New("DHCP reservation removal commit failed")
	}
	if _, err := manager.runner.Run(ctx, "/etc/init.d/dnsmasq", "reload"); err != nil {
		return errors.New("DHCP reservation removal reload failed")
	}
	return nil
}

// Replace changes the complete ProxyPool-owned DHCP reservation projection in
// one UCI commit and one dnsmasq reload. The before projection is used both for
// ownership proof and for exact rollback after a post-commit failure.
func (manager *LeaseManager) Replace(ctx context.Context, before, after []model.Device, revision uint64) error {
	if manager == nil || manager.runner == nil || manager.source == nil || revision == 0 {
		return errors.New("DHCP reservation replacement is invalid")
	}
	beforeByID, err := manager.leaseDeviceMap(before, revision)
	if err != nil {
		return err
	}
	afterByID, err := manager.leaseDeviceMap(after, revision)
	if err != nil {
		return err
	}
	touched := changedLeaseDeviceIDs(beforeByID, afterByID)
	if len(touched) == 0 {
		return nil
	}
	for _, id := range touched {
		if device, exists := afterByID[id]; exists {
			if err := manager.confirmDevice(ctx, device); err != nil {
				return err
			}
		}
	}
	if err := manager.ensureNoPendingDHCP(ctx); err != nil {
		return err
	}
	contents, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "show", "dhcp")
	if err != nil {
		return errors.New("DHCP reservation inventory failed")
	}
	hosts, err := parseUCIHosts(contents)
	if err != nil {
		return errors.New("DHCP reservation inventory is invalid")
	}
	snapshot := make(map[string]uciHost, len(touched))
	snapshotExists := make(map[string]bool, len(touched))
	touchedSections := make(map[string]struct{}, len(touched))
	for _, id := range touched {
		section := "proxypool_" + id
		touchedSections[section] = struct{}{}
		if host, exists := hosts[section]; exists {
			snapshot[section], snapshotExists[section] = host, true
		}
		if device, exists := beforeByID[id]; exists {
			if host, present := hosts[section]; present && !ownedLeaseHostMatches(host, device) {
				return errors.New("DHCP reservation ownership mismatch")
			}
		}
	}
	for _, id := range touched {
		device, exists := afterByID[id]
		if !exists {
			continue
		}
		for section, host := range hosts {
			if _, isTouched := touchedSections[section]; isTouched {
				continue
			}
			if host.MAC == device.MAC || host.IP == device.FixedIPv4.String() {
				return errors.New("DHCP reservation conflicts with another host")
			}
		}
	}
	if err := manager.stageLeaseProjection(ctx, touched, afterByID, hosts); err != nil {
		_, _ = manager.runner.Run(context.Background(), "/sbin/uci", "-q", "revert", "dhcp")
		return err
	}
	projection, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "show", "dhcp")
	if err != nil || !leaseProjectionMatches(projection, touched, afterByID) {
		_, _ = manager.runner.Run(context.Background(), "/sbin/uci", "-q", "revert", "dhcp")
		return errors.New("DHCP reservation replacement verification failed")
	}
	if _, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "commit", "dhcp"); err != nil {
		_, _ = manager.runner.Run(context.Background(), "/sbin/uci", "-q", "revert", "dhcp")
		return errors.New("DHCP reservation replacement commit failed")
	}
	if _, err := manager.runner.Run(ctx, "/etc/init.d/dnsmasq", "reload"); err != nil {
		manager.restoreLeaseProjection(touched, snapshot, snapshotExists)
		return errors.New("DHCP reservation replacement reload failed")
	}
	for _, id := range touched {
		device, exists := afterByID[id]
		if !exists {
			continue
		}
		if err := manager.confirmDevice(ctx, device); err != nil {
			manager.restoreLeaseProjection(touched, snapshot, snapshotExists)
			return errors.New("DHCP reservation replacement live lease verification failed")
		}
	}
	return nil
}

func (manager *LeaseManager) leaseDeviceMap(devices []model.Device, revision uint64) (map[string]model.Device, error) {
	result := make(map[string]model.Device, len(devices))
	macs := make(map[string]struct{}, len(devices))
	addresses := make(map[string]struct{}, len(devices))
	for _, device := range devices {
		if _, exists := result[device.ID]; exists {
			return nil, errors.New("DHCP reservation replacement contains duplicate devices")
		}
		if err := manager.validateDevice(device, revision); err != nil {
			return nil, err
		}
		address := device.FixedIPv4.String()
		if _, exists := macs[device.MAC]; exists {
			return nil, errors.New("DHCP reservation replacement contains duplicate MAC addresses")
		}
		if _, exists := addresses[address]; exists {
			return nil, errors.New("DHCP reservation replacement contains duplicate IPv4 addresses")
		}
		macs[device.MAC] = struct{}{}
		addresses[address] = struct{}{}
		result[device.ID] = device
	}
	return result, nil
}

func changedLeaseDeviceIDs(before, after map[string]model.Device) []string {
	set := make(map[string]struct{}, len(before)+len(after))
	for id, device := range before {
		candidate, exists := after[id]
		if !exists || !sameLeaseDevice(device, candidate) {
			set[id] = struct{}{}
		}
	}
	for id, device := range after {
		candidate, exists := before[id]
		if !exists || !sameLeaseDevice(device, candidate) {
			set[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sameLeaseDevice(left, right model.Device) bool {
	return left.ID == right.ID && left.MAC == right.MAC && left.FixedIPv4 == right.FixedIPv4 && sanitizeHostname(left.Hostname) == sanitizeHostname(right.Hostname)
}

func ownedLeaseHostMatches(host uciHost, device model.Device) bool {
	return host.Type == "host" && host.MAC == device.MAC && host.IP == device.FixedIPv4.String()
}

func (manager *LeaseManager) stageLeaseProjection(ctx context.Context, touched []string, after map[string]model.Device, existing map[string]uciHost) error {
	for _, id := range touched {
		section := "proxypool_" + id
		if _, exists := existing[section]; exists {
			if _, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "delete", "dhcp."+section); err != nil {
				return errors.New("DHCP reservation replacement staging failed")
			}
		}
		device, exists := after[id]
		if !exists {
			continue
		}
		commands := [][]string{
			{"-q", "set", "dhcp." + section + "=host"},
			{"-q", "set", "dhcp." + section + ".mac=" + device.MAC},
			{"-q", "set", "dhcp." + section + ".ip=" + device.FixedIPv4.String()},
		}
		if hostname := sanitizeHostname(device.Hostname); hostname != "" {
			commands = append(commands, []string{"-q", "set", "dhcp." + section + ".name=" + hostname})
		}
		for _, args := range commands {
			if _, err := manager.runner.Run(ctx, "/sbin/uci", args...); err != nil {
				return errors.New("DHCP reservation replacement staging failed")
			}
		}
	}
	return nil
}

func leaseProjectionMatches(contents []byte, touched []string, after map[string]model.Device) bool {
	hosts, err := parseUCIHosts(contents)
	if err != nil {
		return false
	}
	for _, id := range touched {
		host, exists := hosts["proxypool_"+id]
		device, wanted := after[id]
		if exists != wanted {
			return false
		}
		if wanted && (!ownedLeaseHostMatches(host, device) || host.Name != sanitizeHostname(device.Hostname)) {
			return false
		}
	}
	return true
}

func (manager *LeaseManager) restoreLeaseProjection(touched []string, snapshot map[string]uciHost, snapshotExists map[string]bool) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCommandTimeout)
	defer cancel()
	for _, id := range touched {
		section := "proxypool_" + id
		_, _ = manager.runner.Run(ctx, "/sbin/uci", "-q", "delete", "dhcp."+section)
		host, exists := snapshot[section]
		if !snapshotExists[section] || !exists {
			continue
		}
		commands := [][]string{
			{"-q", "set", "dhcp." + section + "=" + host.Type},
			{"-q", "set", "dhcp." + section + ".mac=" + host.MAC},
			{"-q", "set", "dhcp." + section + ".ip=" + host.IP},
		}
		if host.Name != "" {
			commands = append(commands, []string{"-q", "set", "dhcp." + section + ".name=" + host.Name})
		}
		for _, args := range commands {
			_, _ = manager.runner.Run(ctx, "/sbin/uci", args...)
		}
	}
	_, _ = manager.runner.Run(ctx, "/sbin/uci", "-q", "commit", "dhcp")
	_, _ = manager.runner.Run(ctx, "/etc/init.d/dnsmasq", "reload")
}

func (manager *LeaseManager) ensureNoPendingDHCP(ctx context.Context) error {
	changes, err := manager.runner.Run(ctx, "/sbin/uci", "-q", "changes", "dhcp")
	if err != nil || len(bytes.TrimSpace(changes)) != 0 {
		return errors.New("pending DHCP configuration prevents reservation mutation")
	}
	return nil
}

func (manager *LeaseManager) validateDevice(device model.Device, revision uint64) error {
	if manager == nil || manager.runner == nil || manager.source == nil || revision == 0 || !manager.lan.IsValid() || !manager.router.Is4() {
		return errors.New("DHCP reservation request is invalid")
	}
	mac, err := normalizeMAC(device.MAC)
	if err != nil || mac != device.MAC || device.ID != deviceIDForMAC(mac) || !device.FixedIPv4.Is4() || !manager.lan.Contains(device.FixedIPv4) || device.FixedIPv4 == manager.router || isPrefixBoundary(manager.lan, device.FixedIPv4) {
		return errors.New("DHCP reservation request is invalid")
	}
	return nil
}

func (manager *LeaseManager) confirmDevice(ctx context.Context, device model.Device) error {
	devices, err := manager.source.List(ctx)
	if err != nil {
		return errors.New("discovered device verification failed")
	}
	for _, discovered := range devices {
		if discovered.ID == device.ID && discovered.Confirmed && discovered.MAC == device.MAC && discovered.IPv4 == device.FixedIPv4 {
			return nil
		}
	}
	return errors.New("device is not confirmed by DHCP")
}

func (manager *LeaseManager) cleanupNewReservation(section string) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCommandTimeout)
	defer cancel()
	_, _ = manager.runner.Run(ctx, "/sbin/uci", "-q", "delete", "dhcp."+section)
	_, _ = manager.runner.Run(ctx, "/sbin/uci", "-q", "commit", "dhcp")
	_, _ = manager.runner.Run(ctx, "/etc/init.d/dnsmasq", "reload")
}

type uciHost struct {
	Type string
	MAC  string
	IP   string
	Name string
}

func parseUCIHosts(contents []byte) (map[string]uciHost, error) {
	hosts := make(map[string]uciHost)
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		left, right, found := strings.Cut(line, "=")
		if !found || !strings.HasPrefix(left, "dhcp.") {
			return nil, errors.New("invalid UCI projection")
		}
		path := strings.TrimPrefix(left, "dhcp.")
		parts := strings.Split(path, ".")
		if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
			return nil, errors.New("invalid UCI host path")
		}
		value := strings.Trim(right, "'")
		host := hosts[parts[0]]
		if len(parts) == 1 {
			host.Type = value
		} else {
			switch parts[1] {
			case "mac":
				host.MAC = strings.ToLower(value)
			case "ip":
				host.IP = value
			case "name":
				host.Name = value
			}
		}
		hosts[parts[0]] = host
	}
	return hosts, nil
}

func exactOwnedProjection(contents []byte, section string, device model.Device) bool {
	hosts, err := parseUCIHosts(contents)
	if err != nil || len(hosts) != 1 {
		return false
	}
	host, exists := hosts[section]
	if !exists || host.Type != "host" || host.MAC != device.MAC || host.IP != device.FixedIPv4.String() {
		return false
	}
	wantName := sanitizeHostname(device.Hostname)
	return host.Name == wantName
}

func isPrefixBoundary(prefix netip.Prefix, address netip.Addr) bool {
	if address == prefix.Addr() {
		return true
	}
	value := address.As4()
	network := prefix.Addr().As4()
	hostBits := 32 - prefix.Bits()
	if hostBits <= 0 {
		return true
	}
	mask := uint32(1<<hostBits) - 1
	addressValue := uint32(value[0])<<24 | uint32(value[1])<<16 | uint32(value[2])<<8 | uint32(value[3])
	networkValue := uint32(network[0])<<24 | uint32(network[1])<<16 | uint32(network[2])<<8 | uint32(network[3])
	return addressValue == networkValue|mask
}

var _ platform.DeviceSource = (*DeviceInventory)(nil)
var _ platform.LeaseManager = (*LeaseManager)(nil)
