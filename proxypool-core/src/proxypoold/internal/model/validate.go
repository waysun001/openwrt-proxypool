package model

import (
	"net"
	"sort"
	"strings"
)

const maxNodes = 60

func Validate(cfg DesiredConfig) error {
	if cfg.SchemaVersion != 2 {
		return invalid("invalid_config", "schema version must be 2")
	}
	if len(cfg.Nodes) > maxNodes {
		return invalid("capacity_exceeded", "node capacity exceeds 60")
	}

	nodeKeys := sortedKeys(cfg.Nodes)
	if err := validateNodeIdentity(cfg.Nodes, nodeKeys); err != nil {
		return err
	}
	if err := validateNodeProtocol(cfg.Nodes, nodeKeys); err != nil {
		return err
	}

	deviceKeys := sortedKeys(cfg.Devices)
	if err := validateDeviceIdentity(cfg.Devices, deviceKeys); err != nil {
		return err
	}
	if err := validateDeviceMACs(cfg.Devices, deviceKeys); err != nil {
		return err
	}
	if err := validateDeviceAddresses(cfg.Devices, deviceKeys); err != nil {
		return err
	}
	if err := validateDeviceReferences(cfg.Nodes, cfg.Devices, deviceKeys); err != nil {
		return err
	}
	if err := validatePendingBindings(cfg.Nodes, cfg.Devices, cfg.PendingBindings); err != nil {
		return err
	}

	return nil
}

func validatePendingBindings(nodes map[string]Node, devices map[string]Device, pending map[string]PendingBinding) error {
	addresses := make(map[string]struct{}, len(devices)+len(pending))
	for _, device := range devices {
		addresses[device.FixedIPv4.String()] = struct{}{}
	}
	for _, key := range sortedKeys(pending) {
		binding := pending[key]
		if key == "" || binding.ID != key || !binding.LegacyIPv4.Is4() || binding.CreatedAt.IsZero() {
			return invalid("invalid_config", "pending binding is invalid")
		}
		if _, exists := nodes[binding.NodeID]; !exists {
			return invalid("not_found", "pending binding node does not exist")
		}
		if binding.ErrorCode != "" && binding.ErrorCode != "duplicate" {
			return invalid("invalid_config", "pending binding error is invalid")
		}
		address := binding.LegacyIPv4.String()
		if _, exists := addresses[address]; exists {
			if binding.ErrorCode != "duplicate" {
				return invalid("duplicate", "pending binding IPv4 is duplicated")
			}
		}
		addresses[address] = struct{}{}
	}
	return nil
}

func validateNodeIdentity(nodes map[string]Node, nodeKeys []string) error {
	names := make(map[string]struct{}, len(nodes))
	policyIDs := make(map[uint16]struct{}, len(nodes))
	for _, key := range nodeKeys {
		node := nodes[key]
		if key == "" || node.ID != key {
			return invalid("invalid_config", "node identity is invalid")
		}
		name := strings.ToLower(strings.TrimSpace(node.Name))
		if name == "" {
			return invalid("invalid_config", "node name is required")
		}
		if _, exists := names[name]; exists {
			return invalid("duplicate", "node name is duplicated")
		}
		names[name] = struct{}{}
		if node.PolicyID == 0 || node.PolicyID > maxNodes {
			return invalid("invalid_config", "node policy ID is invalid")
		}
		if _, exists := policyIDs[node.PolicyID]; exists {
			return invalid("duplicate", "node policy ID is duplicated")
		}
		policyIDs[node.PolicyID] = struct{}{}
	}
	return nil
}

func validateNodeProtocol(nodes map[string]Node, nodeKeys []string) error {
	for _, key := range nodeKeys {
		node := nodes[key]
		if !validProtocol(node.Protocol) {
			return invalid("invalid_config", "node protocol is invalid")
		}
		if node.Port == 0 {
			return invalid("invalid_config", "node port is invalid")
		}
		if !validServer(node.Server) {
			return invalid("invalid_config", "node server is invalid")
		}
		switch node.Protocol {
		case ProtocolL2TP:
			if node.Username == "" || node.Password == "" {
				return invalid("invalid_config", "L2TP credentials are required")
			}
		case ProtocolSOCKS5:
			if (node.Username == "") != (node.Password == "") {
				return invalid("invalid_config", "SOCKS5 credentials must be paired")
			}
		case ProtocolSLP:
			if node.SLPToken == "" {
				return invalid("invalid_config", "SLP token is required")
			}
			if node.SLPTransport != "quic" {
				return invalid("invalid_config", "SLP transport is unsupported")
			}
		}
	}
	return nil
}

func validateDeviceIdentity(devices map[string]Device, deviceKeys []string) error {
	for _, key := range deviceKeys {
		if key == "" || devices[key].ID != key {
			return invalid("invalid_config", "device identity is invalid")
		}
	}
	return nil
}

func validateDeviceMACs(devices map[string]Device, deviceKeys []string) error {
	macs := make(map[string]struct{}, len(devices))
	for _, key := range deviceKeys {
		mac, err := net.ParseMAC(devices[key].MAC)
		if err != nil || len(mac) != 6 {
			return invalid("invalid_config", "device MAC is invalid")
		}
		macKey := mac.String()
		if _, exists := macs[macKey]; exists {
			return invalid("duplicate", "device MAC is duplicated")
		}
		macs[macKey] = struct{}{}
	}
	return nil
}

func validateDeviceAddresses(devices map[string]Device, deviceKeys []string) error {
	addresses := make(map[string]struct{}, len(devices))
	for _, key := range deviceKeys {
		address := devices[key].FixedIPv4
		if !address.Is4() {
			return invalid("invalid_config", "device fixed IPv4 is invalid")
		}
		addressKey := address.String()
		if _, exists := addresses[addressKey]; exists {
			return invalid("duplicate", "device fixed IPv4 is duplicated")
		}
		addresses[addressKey] = struct{}{}
	}
	return nil
}

func validateDeviceReferences(nodes map[string]Node, devices map[string]Device, deviceKeys []string) error {
	for _, key := range deviceKeys {
		nodeID := devices[key].NodeID
		if nodeID == "" {
			continue
		}
		if _, exists := nodes[nodeID]; !exists {
			return invalid("not_found", "device node does not exist")
		}
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func invalid(code, message string) error {
	return &CodeError{Code: code, Message: message}
}

func validProtocol(protocol Protocol) bool {
	switch protocol {
	case ProtocolL2TP, ProtocolSOCKS5, ProtocolSLP:
		return true
	default:
		return false
	}
}

func validServer(server string) bool {
	if net.ParseIP(server) != nil {
		return true
	}
	return validHostname(server)
}

func validHostname(hostname string) bool {
	if len(hostname) == 0 || len(hostname) > 253 {
		return false
	}
	if strings.HasSuffix(hostname, ".") {
		hostname = hostname[:len(hostname)-1]
	}
	if hostname == "" {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') &&
				character != '-' {
				return false
			}
		}
	}
	return true
}
