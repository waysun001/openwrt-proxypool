package model_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

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
			t.Errorf("JSON contains secret field %q", field)
		}
	}
}

func TestNodeSecretsAreRedactedFromFormatOutput(t *testing.T) {
	node := model.Node{
		ID:         "node-01",
		Name:       "Node 01",
		Protocol:   model.ProtocolSLP,
		Password:   "password-secret",
		SLPToken:   "token-secret",
		SLPObfsKey: "obfs-key-secret",
	}

	tests := []struct {
		name  string
		value any
	}{
		{
			name:  "direct node",
			value: node,
		},
		{
			name: "node map",
			value: map[string]model.Node{
				"node-01": node,
			},
		},
		{
			name: "desired config",
			value: model.DesiredConfig{
				Nodes: map[string]model.Node{
					"node-01": node,
				},
			},
		},
	}

	for _, tt := range tests {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			t.Run(tt.name+" "+format, func(t *testing.T) {
				output := fmt.Sprintf(format, tt.value)
				for _, secretName := range []string{"password", "token", "obfs key"} {
					if strings.Contains(output, secretValue(secretName)) {
						t.Errorf("format %q leaked the %s", format, secretName)
					}
				}
			})
		}
	}
}

func TestValidateRejectsUnsupportedSchemaVersions(t *testing.T) {
	for _, version := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("version %d", version), func(t *testing.T) {
			cfg := validConfig()
			cfg.SchemaVersion = version
			assertCode(t, model.Validate(cfg), "invalid_config")
		})
	}
}

func TestValidateRejectsInvalidOrDuplicatePolicyIDs(t *testing.T) {
	tests := []struct {
		name string
		cfg  model.DesiredConfig
		code string
	}{
		{
			name: "zero policy ID",
			cfg: func() model.DesiredConfig {
				cfg := validConfig()
				node := cfg.Nodes["node-01"]
				node.PolicyID = 0
				cfg.Nodes["node-01"] = node
				return cfg
			}(),
			code: "invalid_config",
		},
		{
			name: "policy ID above capacity",
			cfg: func() model.DesiredConfig {
				cfg := validConfig()
				node := cfg.Nodes["node-01"]
				node.PolicyID = 61
				cfg.Nodes["node-01"] = node
				return cfg
			}(),
			code: "invalid_config",
		},
		{
			name: "duplicate policy ID",
			cfg: func() model.DesiredConfig {
				cfg := validConfigWithNodes(2)
				node := cfg.Nodes["node-02"]
				node.PolicyID = 1
				cfg.Nodes["node-02"] = node
				return cfg
			}(),
			code: "duplicate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCode(t, model.Validate(tt.cfg), tt.code)
		})
	}
}

func TestValidateRequiresCanonicalNodeAndDeviceIdentity(t *testing.T) {
	tests := []struct {
		name string
		cfg  model.DesiredConfig
		code string
	}{
		{
			name: "empty node map key",
			cfg: func() model.DesiredConfig {
				cfg := validConfig()
				node := cfg.Nodes["node-01"]
				cfg.Nodes = map[string]model.Node{"": node}
				return cfg
			}(),
			code: "invalid_config",
		},
		{
			name: "node map key differs from node ID",
			cfg: func() model.DesiredConfig {
				cfg := validConfig()
				node := cfg.Nodes["node-01"]
				cfg.Nodes = map[string]model.Node{"other-node": node}
				return cfg
			}(),
			code: "invalid_config",
		},
		{
			name: "blank normalized node name",
			cfg: func() model.DesiredConfig {
				cfg := validConfig()
				node := cfg.Nodes["node-01"]
				node.Name = " \t "
				cfg.Nodes["node-01"] = node
				return cfg
			}(),
			code: "invalid_config",
		},
		{
			name: "duplicate normalized node name",
			cfg: func() model.DesiredConfig {
				cfg := validConfigWithNodes(2)
				first := cfg.Nodes["node-01"]
				first.Name = " Example Node "
				cfg.Nodes["node-01"] = first
				second := cfg.Nodes["node-02"]
				second.Name = "example node"
				cfg.Nodes["node-02"] = second
				return cfg
			}(),
			code: "duplicate",
		},
		{
			name: "empty device map key",
			cfg: func() model.DesiredConfig {
				cfg := validConfig()
				cfg.Devices = map[string]model.Device{
					"": validDevice("device-01", "00:11:22:33:44:55", "192.0.2.10"),
				}
				return cfg
			}(),
			code: "invalid_config",
		},
		{
			name: "device map key differs from device ID",
			cfg: func() model.DesiredConfig {
				cfg := validConfig()
				cfg.Devices = map[string]model.Device{
					"other-device": validDevice("device-01", "00:11:22:33:44:55", "192.0.2.10"),
				}
				return cfg
			}(),
			code: "invalid_config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCode(t, model.Validate(tt.cfg), tt.code)
		})
	}
}

func TestValidateDeviceBindingAndAddressRules(t *testing.T) {
	t.Run("allows an unbound device", func(t *testing.T) {
		cfg := withDevices(validConfig(), map[string]model.Device{
			"device-01": validDevice("device-01", "00:11:22:33:44:55", "192.0.2.10"),
		})
		device := cfg.Devices["device-01"]
		device.NodeID = ""
		cfg.Devices["device-01"] = device
		if err := model.Validate(cfg); err != nil {
			t.Fatalf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("rejects IPv4-mapped IPv6 address", func(t *testing.T) {
		cfg := withDevices(validConfig(), map[string]model.Device{
			"device-01": validDevice("device-01", "00:11:22:33:44:55", "192.0.2.10"),
		})
		device := cfg.Devices["device-01"]
		device.FixedIPv4 = netip.MustParseAddr("::ffff:192.0.2.10")
		cfg.Devices["device-01"] = device
		assertCode(t, model.Validate(cfg), "invalid_config")
	})
}

func TestValidateRejectsNon48BitMACAddresses(t *testing.T) {
	for _, mac := range []string{
		"00:11:22:33:44:55:66:77",
		"00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00:11:22:33",
	} {
		t.Run(mac, func(t *testing.T) {
			cfg := withDevices(validConfig(), map[string]model.Device{
				"device-01": validDevice("device-01", mac, "192.0.2.10"),
			})
			assertCode(t, model.Validate(cfg), "invalid_config")
		})
	}
}

func TestValidateProtocolCredentialRules(t *testing.T) {
	tests := []struct {
		name string
		node model.Node
		code string
	}{
		{
			name: "allows SOCKS5 without authentication",
			node: model.Node{
				ID:       "node-01",
				Name:     "Node 01",
				Protocol: model.ProtocolSOCKS5,
				Server:   "no-dns-needed.invalid",
				Port:     1080,
				PolicyID: 1,
			},
		},
		{
			name: "rejects SOCKS5 username without password",
			node: model.Node{
				ID:       "node-01",
				Name:     "Node 01",
				Protocol: model.ProtocolSOCKS5,
				Server:   "no-dns-needed.invalid",
				Port:     1080,
				Username: "user",
				PolicyID: 1,
			},
			code: "invalid_config",
		},
		{
			name: "rejects SOCKS5 password without username",
			node: model.Node{
				ID:       "node-01",
				Name:     "Node 01",
				Protocol: model.ProtocolSOCKS5,
				Server:   "no-dns-needed.invalid",
				Port:     1080,
				Password: "secret",
				PolicyID: 1,
			},
			code: "invalid_config",
		},
		{
			name: "rejects SLP without token",
			node: model.Node{
				ID:           "node-01",
				Name:         "Node 01",
				Protocol:     model.ProtocolSLP,
				Server:       "no-dns-needed.invalid",
				Port:         443,
				SLPTransport: "quic",
				PolicyID:     1,
			},
			code: "invalid_config",
		},
		{
			name: "rejects SLP unsupported transport",
			node: model.Node{
				ID:           "node-01",
				Name:         "Node 01",
				Protocol:     model.ProtocolSLP,
				Server:       "no-dns-needed.invalid",
				Port:         443,
				SLPToken:     "token",
				SLPTransport: "tcp",
				PolicyID:     1,
			},
			code: "invalid_config",
		},
		{
			name: "allows SLP obfuscation with token fallback",
			node: model.Node{
				ID:           "node-01",
				Name:         "Node 01",
				Protocol:     model.ProtocolSLP,
				Server:       "no-dns-needed.invalid",
				Port:         443,
				SLPToken:     "token",
				SLPTransport: "quic",
				SLPObfs:      true,
				PolicyID:     1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := model.Validate(withNode(validConfig(), tt.node))
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

func TestValidateReportsMultiErrorConfigurationDeterministically(t *testing.T) {
	cfg := validConfigWithNodes(2)
	first := cfg.Nodes["node-01"]
	first.Protocol = "invalid"
	cfg.Nodes["node-01"] = first
	second := cfg.Nodes["node-02"]
	second.Port = 0
	cfg.Nodes["node-02"] = second

	var want string
	for i := 0; i < 100; i++ {
		err := model.Validate(cfg)
		if err == nil {
			t.Fatal("Validate() error = nil, want invalid configuration")
		}
		if i == 0 {
			want = err.Error()
			continue
		}
		if got := err.Error(); got != want {
			t.Fatalf("Validate() error changed between runs: got %q, want %q", got, want)
		}
	}
}

func secretValue(name string) string {
	switch name {
	case "password":
		return "password-secret"
	case "token":
		return "token-secret"
	default:
		return "obfs-key-secret"
	}
}

func TestValidatePendingBindingsRequiresUniqueIPv4AndExistingNode(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	cfg := validConfig()
	cfg.PendingBindings = map[string]model.PendingBinding{
		"pending_a": {ID: "pending_a", LegacyIPv4: netip.MustParseAddr("192.168.9.20"), NodeID: "node-01", CreatedAt: now},
	}
	if err := model.Validate(cfg); err != nil {
		t.Fatalf("valid pending binding: %v", err)
	}
	duplicate := cfg
	duplicate.PendingBindings = map[string]model.PendingBinding{"pending_a": cfg.PendingBindings["pending_a"]}
	duplicate.Devices = map[string]model.Device{"device-01": validDevice("device-01", "00:11:22:33:44:55", "192.168.9.20")}
	value := duplicate.PendingBindings["pending_a"]
	value.ErrorCode = "duplicate"
	duplicate.PendingBindings["pending_a"] = value
	if err := model.Validate(duplicate); err != nil {
		t.Fatalf("explicit duplicate pending state: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*model.DesiredConfig)
	}{
		{name: "identity", mutate: func(cfg *model.DesiredConfig) {
			value := cfg.PendingBindings["pending_a"]
			value.ID = "wrong"
			cfg.PendingBindings["pending_a"] = value
		}},
		{name: "ipv6", mutate: func(cfg *model.DesiredConfig) {
			value := cfg.PendingBindings["pending_a"]
			value.LegacyIPv4 = netip.MustParseAddr("2001:db8::1")
			cfg.PendingBindings["pending_a"] = value
		}},
		{name: "missing node", mutate: func(cfg *model.DesiredConfig) {
			value := cfg.PendingBindings["pending_a"]
			value.NodeID = "missing"
			cfg.PendingBindings["pending_a"] = value
		}},
		{name: "missing timestamp", mutate: func(cfg *model.DesiredConfig) {
			value := cfg.PendingBindings["pending_a"]
			value.CreatedAt = time.Time{}
			cfg.PendingBindings["pending_a"] = value
		}},
		{name: "bad error", mutate: func(cfg *model.DesiredConfig) {
			value := cfg.PendingBindings["pending_a"]
			value.ErrorCode = "secret-detail"
			cfg.PendingBindings["pending_a"] = value
		}},
		{name: "device address collision", mutate: func(cfg *model.DesiredConfig) {
			cfg.Devices = map[string]model.Device{"device-01": validDevice("device-01", "00:11:22:33:44:55", "192.168.9.20")}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cfg
			candidate.PendingBindings = map[string]model.PendingBinding{"pending_a": cfg.PendingBindings["pending_a"]}
			test.mutate(&candidate)
			if err := model.Validate(candidate); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func validConfig() model.DesiredConfig {
	return validConfigWithNodes(1)
}

func TestValidateDeletePendingNodeMustBeDisabledAndUnreferenced(t *testing.T) {
	cfg := validConfig()
	node := cfg.Nodes["node-01"]
	node.DeletePending = true
	node.Enabled = false
	cfg.Nodes[node.ID] = node
	if err := model.Validate(cfg); err != nil {
		t.Fatalf("unreferenced delete-pending node rejected: %v", err)
	}

	node.Enabled = true
	cfg.Nodes[node.ID] = node
	assertCode(t, model.Validate(cfg), "invalid_config")

	node.Enabled = false
	cfg.Nodes[node.ID] = node
	cfg.Devices = map[string]model.Device{"device-01": validDevice("device-01", "00:11:22:33:44:55", "192.168.9.20")}
	assertCode(t, model.Validate(cfg), "invalid_config")
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
		SchemaVersion: 2,
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
