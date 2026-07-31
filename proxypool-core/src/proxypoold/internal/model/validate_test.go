package model_test

import (
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	"proxypoold/internal/model"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  model.DesiredConfig
		code string
	}{
		{
			name: "accepts sixty nodes",
			cfg:  validConfigWithNodes(60),
		},
		{
			name: "rejects sixty-one nodes",
			cfg:  validConfigWithNodes(61),
			code: "capacity_exceeded",
		},
		{
			name: "rejects duplicate normalized MAC addresses",
			cfg: withDevices(validConfig(), map[string]model.Device{
				"device-a": validDevice("device-a", "00:11:22:33:44:55", "192.0.2.10"),
				"device-b": validDevice("device-b", "00-11-22-33-44-55", "192.0.2.11"),
			}),
			code: "duplicate",
		},
		{
			name: "rejects duplicate fixed IPv4 addresses",
			cfg: withDevices(validConfig(), map[string]model.Device{
				"device-a": validDevice("device-a", "00:11:22:33:44:55", "192.0.2.10"),
				"device-b": validDevice("device-b", "00:11:22:33:44:56", "192.0.2.10"),
			}),
			code: "duplicate",
		},
		{
			name: "rejects device with missing node reference",
			cfg: withDevices(validConfig(), map[string]model.Device{
				"device-a": {
					ID:        "device-a",
					MAC:       "00:11:22:33:44:55",
					FixedIPv4: netip.MustParseAddr("192.0.2.10"),
					NodeID:    "missing-node",
					Enabled:   true,
				},
			}),
			code: "not_found",
		},
		{
			name: "rejects invalid protocol",
			cfg: withNode(validConfig(), model.Node{
				ID:       "node-01",
				Name:     "Node 01",
				Protocol: "wireguard",
				Server:   "no-dns-needed.invalid",
				Port:     1080,
				PolicyID: 1,
			}),
			code: "invalid_config",
		},
		{
			name: "rejects server that is neither an IP nor hostname",
			cfg: withNode(validConfig(), model.Node{
				ID:       "node-01",
				Name:     "Node 01",
				Protocol: model.ProtocolSOCKS5,
				Server:   "bad host!",
				Port:     1080,
				PolicyID: 1,
			}),
			code: "invalid_config",
		},
		{
			name: "rejects invalid port",
			cfg: withNode(validConfig(), model.Node{
				ID:       "node-01",
				Name:     "Node 01",
				Protocol: model.ProtocolSOCKS5,
				Server:   "no-dns-needed.invalid",
				PolicyID: 1,
			}),
			code: "invalid_config",
		},
		{
			name: "rejects L2TP node with empty credentials",
			cfg: withNode(validConfig(), model.Node{
				ID:       "node-01",
				Name:     "Node 01",
				Protocol: model.ProtocolL2TP,
				Server:   "no-dns-needed.invalid",
				Port:     1701,
				Password: "secret",
				PolicyID: 1,
			}),
			code: "invalid_config",
		},
		{
			name: "rejects invalid MAC address",
			cfg: withDevices(validConfig(), map[string]model.Device{
				"device-a": validDevice("device-a", "not-a-mac", "192.0.2.10"),
			}),
			code: "invalid_config",
		},
		{
			name: "rejects IPv6 fixed address",
			cfg: withDevices(validConfig(), map[string]model.Device{
				"device-a": validDevice("device-a", "00:11:22:33:44:55", "2001:db8::1"),
			}),
			code: "invalid_config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := model.Validate(tt.cfg)
			if tt.code == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			assertCode(t, err, tt.code)
		})
	}
}

func TestSecretFieldsAreRedactedFromJSON(t *testing.T) {
	node := model.Node{
		Password:   "password-secret",
		SLPToken:   "token-secret",
		SLPObfsKey: "obfs-key-secret",
	}

	b, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var encoded map[string]any
	if err := json.Unmarshal(b, &encoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	for _, field := range []string{"Password", "SLPToken", "SLPObfsKey"} {
		if _, ok := encoded[field]; ok {
			t.Errorf("JSON contains secret field %q: %s", field, b)
		}
	}
}

func validConfig() model.DesiredConfig {
	return validConfigWithNodes(1)
}

func validConfigWithNodes(count int) model.DesiredConfig {
	nodes := make(map[string]model.Node, count)
	for i := 1; i <= count; i++ {
		id := nodeID(i)
		nodes[id] = model.Node{
			ID:       id,
			Name:     id,
			Protocol: model.ProtocolSOCKS5,
			Server:   "no-dns-needed.invalid",
			Port:     1080,
			PolicyID: uint16(i),
		}
	}
	return model.DesiredConfig{
		SchemaVersion: 1,
		Global: model.GlobalConfig{
			MaxNodes: 60,
		},
		Nodes: nodes,
	}
}

func withNode(cfg model.DesiredConfig, node model.Node) model.DesiredConfig {
	cfg.Nodes = map[string]model.Node{node.ID: node}
	return cfg
}

func withDevices(cfg model.DesiredConfig, devices map[string]model.Device) model.DesiredConfig {
	cfg.Devices = devices
	return cfg
}

func validDevice(id, mac, fixedIPv4 string) model.Device {
	return model.Device{
		ID:        id,
		MAC:       mac,
		FixedIPv4: netip.MustParseAddr(fixedIPv4),
		NodeID:    "node-01",
		Enabled:   true,
	}
}

func nodeID(i int) string {
	if i < 10 {
		return "node-0" + string(rune('0'+i))
	}
	return "node-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	var codeErr *model.CodeError
	if !errors.As(err, &codeErr) {
		t.Fatalf("error = %v, want CodeError with code %q", err, want)
	}
	if codeErr.Code != want {
		t.Fatalf("error code = %q, want %q", codeErr.Code, want)
	}
}
