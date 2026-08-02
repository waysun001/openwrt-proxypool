package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

const maxLegacyConfigBytes = 1 << 20

type MigrationResult struct {
	Config          model.DesiredConfig
	SourceSHA256    string
	MigratedNodes   int
	LearnedDevices  int
	PendingBindings int
}

type legacyClient struct {
	section string
	node    model.Node
	bindIPs []netip.Addr
}

// MigrateV1 parses a bounded legacy UCI document without modifying it and
// merges its clients into a cloned V2 configuration. An IP is converted into
// a device only when DHCP discovery already confirms its MAC; otherwise it is
// retained as a non-authorizing pending binding.
func MigrateV1(source []byte, base model.DesiredConfig, discovered []platform.DiscoveredDevice, now time.Time) (MigrationResult, error) {
	if len(source) == 0 || len(source) > maxLegacyConfigBytes || now.IsZero() || model.Validate(base) != nil {
		return MigrationResult{}, errors.New("legacy migration input is invalid")
	}
	sections, err := parseUCI(bytes.NewReader(source))
	if err != nil {
		return MigrationResult{}, errors.New("legacy configuration is invalid")
	}
	clients, globalEnabled, err := parseLegacyClients(sections)
	if err != nil {
		return MigrationResult{}, err
	}
	if len(base.Nodes)+len(clients) > 60 {
		return MigrationResult{}, errors.New("legacy migration exceeds node capacity")
	}
	next := cloneConfig(base)
	if next.Nodes == nil {
		next.Nodes = make(map[string]model.Node)
	}
	if next.Devices == nil {
		next.Devices = make(map[string]model.Device)
	}
	if next.PendingBindings == nil {
		next.PendingBindings = make(map[string]model.PendingBinding)
	}
	if globalEnabled != nil {
		next.Global.Enabled = *globalEnabled
	}

	usedPolicies := make(map[uint16]struct{}, len(next.Nodes))
	usedNames := make(map[string]struct{}, len(next.Nodes))
	for _, node := range next.Nodes {
		usedPolicies[node.PolicyID] = struct{}{}
		usedNames[strings.ToLower(strings.TrimSpace(node.Name))] = struct{}{}
	}
	sectionToNode := make(map[string]string, len(clients))
	for _, client := range clients {
		nodeID, err := migrationNodeID(client.section, next.Nodes)
		if err != nil {
			return MigrationResult{}, err
		}
		node := client.node
		node.ID = nodeID
		node.Name = uniqueMigrationName(node.Name, client.section, usedNames)
		node.PolicyID = nextPolicyID(usedPolicies)
		if node.PolicyID == 0 {
			return MigrationResult{}, errors.New("legacy migration has no free policy ID")
		}
		node.Revision = base.Revision + 1
		next.Nodes[nodeID] = node
		sectionToNode[client.section] = nodeID
		usedPolicies[node.PolicyID] = struct{}{}
		usedNames[strings.ToLower(strings.TrimSpace(node.Name))] = struct{}{}
	}

	discoveryByIP := migrationDiscoveryByIP(discovered)
	usedAddresses := make(map[string]struct{}, len(next.Devices)+len(next.PendingBindings))
	usedMACs := make(map[string]struct{}, len(next.Devices))
	for _, device := range next.Devices {
		usedAddresses[device.FixedIPv4.String()] = struct{}{}
		usedMACs[strings.ToLower(device.MAC)] = struct{}{}
	}
	for _, pending := range next.PendingBindings {
		usedAddresses[pending.LegacyIPv4.String()] = struct{}{}
	}
	seenLegacyIP := make(map[string]struct{})
	learned, deferred := 0, 0
	for _, client := range clients {
		nodeID := sectionToNode[client.section]
		for _, address := range client.bindIPs {
			addressKey := address.String()
			if _, exists := seenLegacyIP[addressKey]; exists {
				return MigrationResult{}, errors.New("legacy bind IPv4 is duplicated")
			}
			seenLegacyIP[addressKey] = struct{}{}
			detected := discoveryByIP[addressKey]
			if detected.count == 1 && detected.device.Confirmed && validMigrationDevice(detected.device) {
				macKey := strings.ToLower(detected.device.MAC)
				_, addressConflict := usedAddresses[addressKey]
				_, macConflict := usedMACs[macKey]
				_, idConflict := next.Devices[detected.device.ID]
				if !addressConflict && !macConflict && !idConflict {
					next.Devices[detected.device.ID] = model.Device{
						ID: detected.device.ID, MAC: macKey, Hostname: detected.device.Hostname,
						FixedIPv4: address, NodeID: nodeID, Enabled: true,
					}
					usedAddresses[addressKey] = struct{}{}
					usedMACs[macKey] = struct{}{}
					learned++
					continue
				}
			}
			pendingID := migrationPendingID(address)
			if _, exists := next.PendingBindings[pendingID]; exists {
				return MigrationResult{}, errors.New("legacy pending binding identity is duplicated")
			}
			errorCode := ""
			if detected.count > 1 {
				errorCode = "duplicate"
			}
			if _, conflict := usedAddresses[addressKey]; conflict {
				errorCode = "duplicate"
			}
			next.PendingBindings[pendingID] = model.PendingBinding{
				ID: pendingID, LegacyIPv4: address, NodeID: nodeID, CreatedAt: now.UTC().Round(0), ErrorCode: errorCode,
			}
			usedAddresses[addressKey] = struct{}{}
			deferred++
		}
	}
	if err := model.Validate(next); err != nil {
		return MigrationResult{}, errors.New("legacy migration result is invalid")
	}
	digest := sha256.Sum256(source)
	return MigrationResult{
		Config: next, SourceSHA256: hex.EncodeToString(digest[:]), MigratedNodes: len(clients),
		LearnedDevices: learned, PendingBindings: deferred,
	}, nil
}

func parseLegacyClients(sections []*uciSection) ([]legacyClient, *bool, error) {
	clients := make([]legacyClient, 0)
	seenSections := make(map[string]struct{})
	var globalEnabled *bool
	for _, section := range sections {
		switch section.kind {
		case "global":
			if section.name != "global" || globalEnabled != nil {
				return nil, nil, errors.New("legacy global section is invalid")
			}
			value := true
			if enabled, exists := section.options["enabled"]; exists {
				parsed, err := parseBool(enabled)
				if err != nil {
					return nil, nil, errors.New("legacy global enabled value is invalid")
				}
				value = parsed
			}
			globalEnabled = &value
		case "client":
			if _, exists := seenSections[section.name]; exists {
				return nil, nil, errors.New("legacy client section is duplicated")
			}
			seenSections[section.name] = struct{}{}
			client, err := parseLegacyClient(section)
			if err != nil {
				return nil, nil, err
			}
			clients = append(clients, client)
			if len(clients) > 60 {
				return nil, nil, errors.New("legacy client capacity exceeds 60")
			}
		default:
			return nil, nil, errors.New("legacy section type is unsupported")
		}
	}
	if globalEnabled == nil {
		return nil, nil, errors.New("legacy global section is missing")
	}
	return clients, globalEnabled, nil
}

func parseLegacyClient(section *uciSection) (legacyClient, error) {
	allowedOptions := []string{"enabled", "name", "type", "server", "port", "username", "password", "expiry", "slp_token", "slp_transport", "slp_obfs", "slp_obfs_key", "slp_insecure"}
	if section.name == "" || !onlyKeys(section.options, allowedOptions) || !onlyKeys(section.lists, []string{"bind_ip"}) {
		return legacyClient{}, errors.New("legacy client options are invalid")
	}
	protocol := model.Protocol(section.options["type"])
	if protocol != model.ProtocolL2TP && protocol != model.ProtocolSOCKS5 && protocol != model.ProtocolSLP {
		return legacyClient{}, errors.New("legacy client protocol is invalid")
	}
	enabled, err := parseBool(section.options["enabled"])
	if err != nil {
		return legacyClient{}, errors.New("legacy client enabled value is invalid")
	}
	port := defaultLegacyPort(protocol)
	if value := section.options["port"]; value != "" {
		parsed, err := strconv.ParseUint(value, 10, 16)
		if err != nil || parsed == 0 {
			return legacyClient{}, errors.New("legacy client port is invalid")
		}
		port = uint16(parsed)
	}
	node := model.Node{
		Name: section.options["name"], Protocol: protocol, Enabled: enabled,
		Server: strings.ToLower(strings.TrimSuffix(strings.TrimSpace(section.options["server"]), ".")), Port: port,
		Username: section.options["username"], Password: section.options["password"],
		SLPToken: section.options["slp_token"], SLPTransport: section.options["slp_transport"],
		SLPObfsKey: section.options["slp_obfs_key"],
	}
	if node.Name == "" {
		node.Name = section.name
	}
	if node.Protocol == model.ProtocolSLP && node.SLPTransport == "" {
		node.SLPTransport = "quic"
	}
	if value := section.options["slp_obfs"]; value != "" {
		node.SLPObfs, err = parseBool(value)
		if err != nil {
			return legacyClient{}, errors.New("legacy SLP obfs value is invalid")
		}
	}
	if value := section.options["slp_insecure"]; value != "" {
		node.SLPInsecure, err = parseBool(value)
		if err != nil {
			return legacyClient{}, errors.New("legacy SLP insecure value is invalid")
		}
	}
	if value := section.options["expiry"]; value != "" {
		expires, err := parseLegacyExpiry(value)
		if err != nil {
			return legacyClient{}, errors.New("legacy client expiry is invalid")
		}
		node.ExpiresAt = &expires
	}
	bindIPs := make([]netip.Addr, 0, len(section.lists["bind_ip"]))
	for _, value := range section.lists["bind_ip"] {
		address, err := netip.ParseAddr(value)
		if err != nil || !address.Is4() {
			return legacyClient{}, errors.New("legacy bind IPv4 is invalid")
		}
		bindIPs = append(bindIPs, address.Unmap())
	}
	return legacyClient{section: section.name, node: node, bindIPs: bindIPs}, nil
}

func defaultLegacyPort(protocol model.Protocol) uint16 {
	switch protocol {
	case model.ProtocolL2TP:
		return 1701
	case model.ProtocolSOCKS5:
		return 1080
	default:
		return 443
	}
}

func parseLegacyExpiry(value string) (time.Time, error) {
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed.UTC(), err
}

func migrationNodeID(section string, existing map[string]model.Node) (string, error) {
	var normalized strings.Builder
	for _, character := range strings.ToLower(section) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' {
			normalized.WriteRune(character)
		} else {
			normalized.WriteByte('_')
		}
	}
	base := strings.Trim(normalized.String(), "_")
	if base == "" {
		base = "legacy"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	candidate := "node_" + base
	if _, exists := existing[candidate]; !exists {
		return candidate, nil
	}
	digest := sha256.Sum256([]byte(section))
	candidate = "node_" + base + "_" + hex.EncodeToString(digest[:4])
	if _, exists := existing[candidate]; exists {
		return "", errors.New("legacy node identity collides with existing configuration")
	}
	return candidate, nil
}

func uniqueMigrationName(name, section string, used map[string]struct{}) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = section
	}
	if _, exists := used[strings.ToLower(name)]; !exists {
		return name
	}
	digest := sha256.Sum256([]byte(section))
	return fmt.Sprintf("%s (%s)", name, hex.EncodeToString(digest[:3]))
}

func nextPolicyID(used map[uint16]struct{}) uint16 {
	for id := uint16(1); id <= 60; id++ {
		if _, exists := used[id]; !exists {
			return id
		}
	}
	return 0
}

type migrationDiscovery struct {
	device platform.DiscoveredDevice
	count  int
}

func migrationDiscoveryByIP(discovered []platform.DiscoveredDevice) map[string]migrationDiscovery {
	byIP := make(map[string]migrationDiscovery)
	for _, device := range discovered {
		if !device.IPv4.Is4() {
			continue
		}
		key := device.IPv4.Unmap().String()
		entry := byIP[key]
		entry.count++
		if entry.count == 1 {
			entry.device = device
		}
		byIP[key] = entry
	}
	return byIP
}

func validMigrationDevice(device platform.DiscoveredDevice) bool {
	parsed, err := net.ParseMAC(device.MAC)
	return err == nil && len(parsed) == 6 && safeUCISectionName(device.ID) && device.IPv4.Is4()
}

func migrationPendingID(address netip.Addr) string {
	return "pending_" + strings.ReplaceAll(address.String(), ".", "_")
}
