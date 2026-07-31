package model

import (
	"net"
	"strings"
)

const maxNodes = 60

func Validate(cfg DesiredConfig) error {
	if len(cfg.Nodes) > maxNodes {
		return invalid("capacity_exceeded", "node capacity exceeds 60")
	}

	for _, node := range cfg.Nodes {
		if !validProtocol(node.Protocol) {
			return invalid("invalid_config", "node protocol is invalid")
		}
		if node.Port == 0 {
			return invalid("invalid_config", "node port is invalid")
		}
		if !validServer(node.Server) {
			return invalid("invalid_config", "node server is invalid")
		}
		if node.Protocol == ProtocolL2TP && (node.Username == "" || node.Password == "") {
			return invalid("invalid_config", "L2TP credentials are required")
		}
	}

	macs := make(map[string]struct{}, len(cfg.Devices))
	addresses := make(map[string]struct{}, len(cfg.Devices))
	for _, device := range cfg.Devices {
		mac, err := net.ParseMAC(device.MAC)
		if err != nil {
			return invalid("invalid_config", "device MAC is invalid")
		}
		macKey := mac.String()
		if _, exists := macs[macKey]; exists {
			return invalid("duplicate", "device MAC is duplicated")
		}
		macs[macKey] = struct{}{}

		ipv4 := net.ParseIP(device.FixedIPv4.String()).To4()
		if ipv4 == nil {
			return invalid("invalid_config", "device fixed IPv4 is invalid")
		}
		addressKey := ipv4.String()
		if _, exists := addresses[addressKey]; exists {
			return invalid("duplicate", "device fixed IPv4 is duplicated")
		}
		addresses[addressKey] = struct{}{}

		if _, exists := cfg.Nodes[device.NodeID]; !exists {
			return invalid("not_found", "device node does not exist")
		}
	}

	return nil
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
