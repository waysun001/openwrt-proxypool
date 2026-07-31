package config

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"proxypoold/internal/model"
)

func TestDecodeEncodeRoundTripPreservesEveryField(t *testing.T) {
	input, err := os.ReadFile("testdata/v2-valid.uci")
	if err != nil {
		t.Fatalf("read valid fixture: %v", err)
	}
	cfg, err := Decode(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("Decode(valid fixture): %v", err)
	}

	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode(valid fixture): %v", err)
	}
	roundTripped, err := Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("Decode(encoded config): %v", err)
	}

	if roundTripped.SchemaVersion != 2 || roundTripped.Revision != 9 {
		t.Fatalf("round-tripped config metadata = schema %d revision %d", roundTripped.SchemaVersion, roundTripped.Revision)
	}
	if roundTripped.Global.Enabled != cfg.Global.Enabled || roundTripped.Global.RuntimeBackend != cfg.Global.RuntimeBackend || roundTripped.Global.MaxNodes != cfg.Global.MaxNodes || roundTripped.Global.LANDevice != cfg.Global.LANDevice || roundTripped.Global.L2TPConcurrency != cfg.Global.L2TPConcurrency || roundTripped.Global.ProxyConcurrency != cfg.Global.ProxyConcurrency || roundTripped.Global.ConnectTimeout != cfg.Global.ConnectTimeout || roundTripped.Global.StopTimeout != cfg.Global.StopTimeout {
		t.Fatalf("round-tripped global differs")
	}
	if !sameUint16s(roundTripped.Global.ManagementPorts, cfg.Global.ManagementPorts) || !sameDoH(roundTripped.Global.DoHEndpoints, cfg.Global.DoHEndpoints) {
		t.Fatalf("round-tripped global lists differ")
	}
	if !configsEqual(roundTripped, cfg) {
		t.Fatal("complete semantic round trip differs")
	}
	node, ok := roundTripped.Nodes["node_b"]
	if !ok || node.Name != "Bob's node" || node.Protocol != model.ProtocolSLP || node.ExpiresAt == nil {
		t.Fatalf("round-tripped node metadata differs")
	}
	if node.Username != "slp-user" || node.SLPTransport != "quic" || !node.SLPObfs || !node.SLPInsecure || node.PolicyID != 2 || node.Revision != 9 {
		t.Fatalf("round-tripped node fields differ")
	}
	if !node.ExpiresAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 123456789, time.UTC)) {
		t.Fatalf("round-tripped expiry differs")
	}
	if node.Password != "fixture-password-not-real" || node.SLPToken != "fixture-token-not-real" || node.SLPObfsKey != "fixture-obfs-key-not-real" {
		t.Fatal("round-tripped secret field was not preserved")
	}
	device, ok := roundTripped.Devices["device_b"]
	if !ok || device.MAC != "00:11:22:33:44:66" || device.Hostname != "Bob's phone" || device.FixedIPv4.String() != "192.0.2.11" || device.NodeID != "node_b" || device.Enabled {
		t.Fatalf("round-tripped device differs")
	}
}

func TestEncodeOrdersSectionsAndQuotesApostrophes(t *testing.T) {
	cfg := validConfig()
	first := cfg.Nodes["node_a"]
	first.Name = "Alice's node"
	cfg.Nodes["node_a"] = first

	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode(): %v", err)
	}
	text := encoded.String()
	globalAt := strings.Index(text, "config global 'global'")
	nodeAAt := strings.Index(text, "config node 'node_a'")
	nodeBAt := strings.Index(text, "config node 'node_b'")
	deviceAAt := strings.Index(text, "config device 'device_a'")
	deviceBAt := strings.Index(text, "config device 'device_b'")
	if globalAt < 0 || nodeAAt < globalAt || nodeBAt < nodeAAt || deviceAAt < nodeBAt || deviceBAt < deviceAAt {
		t.Fatal("sections are not in global, sorted node, sorted device order")
	}
	decoded, err := Decode(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Decode(encoded apostrophe): %v", err)
	}
	if decoded.Nodes["node_a"].Name != "Alice's node" {
		t.Fatal("apostrophe was not preserved")
	}
}

func TestDecodeRejectsMalformedInputWithoutSecretLeak(t *testing.T) {
	input, err := os.ReadFile("testdata/v2-invalid.uci")
	if err != nil {
		t.Fatalf("read invalid fixture: %v", err)
	}
	_, err = Decode(bytes.NewReader(input))
	assertCode(t, err, "invalid_config")
	if strings.Contains(err.Error(), "fixture-password-not-real") {
		t.Fatal("decode error leaked a secret")
	}
}

func TestDecodeRejectsDuplicateGlobalUnknownOptionAndBadListShape(t *testing.T) {
	for name, input := range map[string]string{
		"duplicate global": "config global 'global'\n" + minimalGlobal() + "\nconfig global 'other'\n" + minimalGlobal(),
		"unknown option":   "config global 'global'\n" + minimalGlobal() + "\noption unknown 'x'",
		"bad list shape":   "config global 'global'\n" + strings.Replace(minimalGlobal(), "list management_port '80'", "list management_port '80'\nlist doh_url 'https://dns.example/dns-query'", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(input))
			assertCode(t, err, "invalid_config")
		})
	}
}

func TestCodecRejectsInvalidGlobalScalarValues(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"negative max nodes": func(input string) string {
			return strings.Replace(input, "option max_nodes '60'", "option max_nodes '-1'", 1)
		},
		"negative duration": func(input string) string {
			return strings.Replace(input, "option connect_timeout '30s'", "option connect_timeout '-1s'", 1)
		},
		"invalid runtime": func(input string) string {
			return strings.Replace(input, "option runtime_backend 'v2_shadow'", "option runtime_backend 'other'", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(strings.NewReader("config global 'global'\n" + mutate(minimalGlobal())))
			assertCode(t, err, "invalid_config")
		})
	}

	invalid := validConfig()
	invalid.Global.ConnectTimeout = -time.Second
	var encoded bytes.Buffer
	assertCode(t, Encode(&encoded, invalid), "invalid_config")
}

func TestEncodeDecodeRoundTripSpecialUCIValues(t *testing.T) {
	for name, value := range map[string]string{
		"apostrophe":         "apostrophe's value",
		"backslash":          `backslash\value`,
		"double quote":       `double " quote`,
		"comment marker":     "value # is data",
		"empty string":       "",
		"adjacent fragments": `left'right\\tail`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := specialValueConfig(value)
			var encoded bytes.Buffer
			if err := Encode(&encoded, cfg); err != nil {
				t.Fatalf("Encode(): %v", err)
			}
			decoded, err := Decode(bytes.NewReader(encoded.Bytes()))
			if err != nil {
				t.Fatalf("Decode(encoded config): %v", err)
			}
			if !safeConfigsEqual(decoded, cfg) {
				t.Fatal("semantic round trip differs")
			}
		})
	}
}

func TestDecodeSupportsQuotedFragmentsAndTrailingComment(t *testing.T) {
	input, err := os.ReadFile("testdata/v2-valid.uci")
	if err != nil {
		t.Fatalf("read valid fixture: %v", err)
	}
	input = bytes.Replace(input, []byte("option runtime_backend 'v2_shadow'"), []byte("option runtime_backend 'v2_'\"shadow\" # trailing comment"), 1)
	input = bytes.Replace(input, []byte("option password 'fixture-password-not-real'"), []byte("option password 'left'\\''right\\tail' # trailing comment"), 1)
	decoded, err := Decode(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if decoded.Global.RuntimeBackend != "v2_shadow" || decoded.Nodes["node_a"].Password != "left'right\\tail" {
		t.Fatal("quoted fragments or comment were not parsed correctly")
	}
}

func TestCodecRejectsUnsafeStringsWithoutLeakingThem(t *testing.T) {
	unsafe := []string{"line\nfeed", "carriage\rreturn", "nul\x00byte", "tab\tbyte", string([]byte{0xff})}
	for _, value := range unsafe {
		cfg := specialValueConfig(value)
		var encoded bytes.Buffer
		err := Encode(&encoded, cfg)
		assertCode(t, err, "invalid_config")
		if err != nil && strings.Contains(err.Error(), value) {
			t.Fatal("encode error leaked unsafe value")
		}
	}
	for _, input := range [][]byte{[]byte("config global 'global'\n\x00"), {0xff}} {
		_, err := Decode(bytes.NewReader(input))
		assertCode(t, err, "invalid_config")
	}
}

func TestCodecRequiresGlobalCapacityDoHAndManagementPort(t *testing.T) {
	for name, mutate := range map[string]func(*model.DesiredConfig){
		"capacity": func(cfg *model.DesiredConfig) { cfg.Global.MaxNodes = 1 },
		"doh":      func(cfg *model.DesiredConfig) { cfg.Global.DoHEndpoints = nil },
		"port":     func(cfg *model.DesiredConfig) { cfg.Global.ManagementPorts = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)
			var encoded bytes.Buffer
			assertCode(t, Encode(&encoded, cfg), "invalid_config")
		})
	}
}

func TestDecodeRequiresGlobalCapacityDoHAndManagementPort(t *testing.T) {
	fixture, err := os.ReadFile("testdata/v2-valid.uci")
	if err != nil {
		t.Fatalf("read valid fixture: %v", err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"capacity": func(input []byte) []byte {
			return bytes.Replace(input, []byte("option max_nodes '60'"), []byte("option max_nodes '1'"), 1)
		},
		"doh": func(input []byte) []byte {
			input = bytes.Replace(input, []byte("\tlist doh_url 'https://dns.example/dns-query'\n"), nil, 1)
			input = bytes.Replace(input, []byte("\tlist doh_bootstrap_ip '192.0.2.53'\n"), nil, 1)
			return bytes.Replace(input, []byte("\tlist doh_server_name 'dns.example'\n"), nil, 1)
		},
		"port": func(input []byte) []byte {
			input = bytes.Replace(input, []byte("\tlist management_port '80'\n"), nil, 1)
			return bytes.Replace(input, []byte("\tlist management_port '443'\n"), nil, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(bytes.NewReader(mutate(append([]byte(nil), fixture...))))
			assertCode(t, err, "invalid_config")
		})
	}
}

func TestEncodeRejectsUnsafeStringInEveryConfigArea(t *testing.T) {
	unsafe := "unsafe\x00value"
	for name, mutate := range map[string]func(*model.DesiredConfig){
		"global LAN":    func(cfg *model.DesiredConfig) { cfg.Global.LANDevice = unsafe },
		"DoH URL":       func(cfg *model.DesiredConfig) { cfg.Global.DoHEndpoints[0].URL = unsafe },
		"DoH bootstrap": func(cfg *model.DesiredConfig) { cfg.Global.DoHEndpoints[0].BootstrapIP = unsafe },
		"DoH server":    func(cfg *model.DesiredConfig) { cfg.Global.DoHEndpoints[0].ServerName = unsafe },
		"node ID": func(cfg *model.DesiredConfig) {
			node := cfg.Nodes["node_a"]
			node.ID = unsafe
			delete(cfg.Nodes, "node_a")
			cfg.Nodes[unsafe] = node
		},
		"node name": func(cfg *model.DesiredConfig) {
			node := cfg.Nodes["node_a"]
			node.Name = unsafe
			cfg.Nodes["node_a"] = node
		},
		"node server": func(cfg *model.DesiredConfig) {
			node := cfg.Nodes["node_a"]
			node.Server = unsafe
			cfg.Nodes["node_a"] = node
		},
		"node username": func(cfg *model.DesiredConfig) {
			node := cfg.Nodes["node_a"]
			node.Username = unsafe
			cfg.Nodes["node_a"] = node
		},
		"node password": func(cfg *model.DesiredConfig) {
			node := cfg.Nodes["node_a"]
			node.Password = unsafe
			cfg.Nodes["node_a"] = node
		},
		"node token": func(cfg *model.DesiredConfig) {
			node := cfg.Nodes["node_a"]
			node.Protocol = model.ProtocolSLP
			node.SLPTransport = "quic"
			node.SLPToken = unsafe
			cfg.Nodes["node_a"] = node
		},
		"node transport": func(cfg *model.DesiredConfig) {
			node := cfg.Nodes["node_a"]
			node.Protocol = model.ProtocolSLP
			node.SLPToken = "fixture-token"
			node.SLPTransport = unsafe
			cfg.Nodes["node_a"] = node
		},
		"node obfs key": func(cfg *model.DesiredConfig) {
			node := cfg.Nodes["node_a"]
			node.SLPObfsKey = unsafe
			cfg.Nodes["node_a"] = node
		},
		"device ID": func(cfg *model.DesiredConfig) {
			device := cfg.Devices["device_a"]
			device.ID = unsafe
			delete(cfg.Devices, "device_a")
			cfg.Devices[unsafe] = device
		},
		"device MAC": func(cfg *model.DesiredConfig) {
			device := cfg.Devices["device_a"]
			device.MAC = unsafe
			cfg.Devices["device_a"] = device
		},
		"device hostname": func(cfg *model.DesiredConfig) {
			device := cfg.Devices["device_a"]
			device.Hostname = unsafe
			cfg.Devices["device_a"] = device
		},
		"device node ID": func(cfg *model.DesiredConfig) {
			device := cfg.Devices["device_a"]
			device.NodeID = unsafe
			cfg.Devices["device_a"] = device
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)
			var encoded bytes.Buffer
			assertCode(t, Encode(&encoded, cfg), "invalid_config")
		})
	}
}

func TestCodecRequiresOpenWrtSafeNamedSectionIDs(t *testing.T) {
	for _, invalidID := range []string{"node-a", "node a", "节点", "node.a"} {
		t.Run("encode "+invalidID, func(t *testing.T) {
			cfg := uciSafeConfig()
			replaceNodeID(&cfg, "node_a", invalidID)
			var encoded bytes.Buffer
			assertCode(t, Encode(&encoded, cfg), "invalid_config")
		})
		t.Run("decode "+invalidID, func(t *testing.T) {
			cfg := uciSafeConfig()
			var encoded bytes.Buffer
			if err := Encode(&encoded, cfg); err != nil {
				t.Fatalf("Encode(safe config): %v", err)
			}
			input := bytes.Replace(encoded.Bytes(), []byte("config node 'node_a'"), []byte("config node '"+invalidID+"'"), 1)
			_, err := Decode(bytes.NewReader(input))
			assertCode(t, err, "invalid_config")
		})
		t.Run("encode device "+invalidID, func(t *testing.T) {
			cfg := uciSafeConfig()
			replaceDeviceID(&cfg, "device_a", invalidID)
			var encoded bytes.Buffer
			assertCode(t, Encode(&encoded, cfg), "invalid_config")
		})
		t.Run("decode device "+invalidID, func(t *testing.T) {
			cfg := uciSafeConfig()
			var encoded bytes.Buffer
			if err := Encode(&encoded, cfg); err != nil {
				t.Fatalf("Encode(safe config): %v", err)
			}
			input := bytes.Replace(encoded.Bytes(), []byte("config device 'device_a'"), []byte("config device '"+invalidID+"'"), 1)
			_, err := Decode(bytes.NewReader(input))
			assertCode(t, err, "invalid_config")
		})
	}

	cfg := uciSafeConfig()
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode(safe config): %v", err)
	}
	if !bytes.Contains(encoded.Bytes(), []byte("config node 'node_a'")) || bytes.Contains(encoded.Bytes(), []byte("config node 'node-a'")) {
		t.Fatal("canonical output did not use an OpenWrt-safe section name")
	}
	if _, err := Decode(bytes.NewReader(encoded.Bytes())); err != nil {
		t.Fatalf("Decode(safe config): %v", err)
	}
}

func TestUCITokenizerMatchesOpenWrtCommentRules(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "unquoted comment", line: "option value foo#comment", want: "foo"},
		{name: "single quote", line: "option value 'foo#bar'", want: "foo#bar"},
		{name: "double quote", line: "option value \"foo#bar\"", want: "foo#bar"},
		{name: "escaped hash", line: "option value foo\\#bar", want: "foo#bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields, err := uciTokens(tt.line)
			if err != nil {
				t.Fatalf("uciTokens(): %v", err)
			}
			if len(fields) != 3 || fields[2] != tt.want {
				t.Fatal("tokenizer did not produce the expected comment-compatible value")
			}
		})
	}
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	var codeErr *model.CodeError
	if !errors.As(err, &codeErr) {
		t.Fatalf("error = %v, want CodeError %q", err, want)
	}
	if codeErr.Code != want {
		t.Fatalf("code = %q, want %q", codeErr.Code, want)
	}
}

func sameUint16s(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameDoH(a, b []model.DoHEndpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func safeConfigsEqual(a, b model.DesiredConfig) bool {
	return reflect.DeepEqual(a, b)
}

func validConfig() model.DesiredConfig {
	return model.DesiredConfig{
		SchemaVersion: 2,
		Global: model.GlobalConfig{
			Enabled:          true,
			RuntimeBackend:   "v2_shadow",
			MaxNodes:         60,
			LANDevice:        "br-lan",
			ManagementPorts:  []uint16{80, 443},
			L2TPConcurrency:  4,
			ProxyConcurrency: 5,
			ConnectTimeout:   30 * time.Second,
			StopTimeout:      20 * time.Second,
			DoHEndpoints: []model.DoHEndpoint{{
				URL: "https://dns.example/dns-query", BootstrapIP: "192.0.2.53", ServerName: "dns.example",
			}},
		},
		Nodes: map[string]model.Node{
			"node_b": {ID: "node_b", Name: "Node B", Protocol: model.ProtocolSOCKS5, Enabled: true, Server: "b.example", Port: 1080, PolicyID: 2},
			"node_a": {ID: "node_a", Name: "Node A", Protocol: model.ProtocolSOCKS5, Enabled: true, Server: "a.example", Port: 1080, PolicyID: 1},
		},
		Devices: map[string]model.Device{
			"device_b": {ID: "device_b", MAC: "00:11:22:33:44:66", Hostname: "B", FixedIPv4: mustAddr("192.0.2.11"), NodeID: "node_b", Enabled: true},
			"device_a": {ID: "device_a", MAC: "00:11:22:33:44:55", Hostname: "A", FixedIPv4: mustAddr("192.0.2.10"), NodeID: "node_a", Enabled: true},
		},
	}
}

func specialValueConfig(value string) model.DesiredConfig {
	cfg := validConfig()
	node := cfg.Nodes["node_a"]
	node.Name = "Special node"
	node.Username = value
	node.Password = value
	node.SLPToken = "fixture-token"
	if value != "" {
		node.SLPToken = value
	}
	node.SLPTransport = "quic"
	node.SLPObfs = true
	node.SLPObfsKey = value
	node.Protocol = model.ProtocolSLP
	cfg.Nodes["node_a"] = node
	device := cfg.Devices["device_a"]
	device.Hostname = value
	cfg.Devices["device_a"] = device
	global := cfg.Global
	global.LANDevice = "br-lan"
	cfg.Global = global
	return cfg
}

func uciSafeConfig() model.DesiredConfig {
	return validConfig()
}

func replaceNodeID(cfg *model.DesiredConfig, oldID, newID string) {
	node := cfg.Nodes[oldID]
	node.ID = newID
	delete(cfg.Nodes, oldID)
	cfg.Nodes[newID] = node
	for id, device := range cfg.Devices {
		if device.NodeID == oldID {
			device.NodeID = newID
			cfg.Devices[id] = device
		}
	}
}

func replaceDeviceID(cfg *model.DesiredConfig, oldID, newID string) {
	device := cfg.Devices[oldID]
	device.ID = newID
	delete(cfg.Devices, oldID)
	cfg.Devices[newID] = device
}

func mustAddr(text string) netip.Addr {
	return netip.MustParseAddr(text)
}

func minimalGlobal() string {
	return strings.Join([]string{
		"option schema_version '2'", "option revision '1'", "option enabled '1'", "option runtime_backend 'v2_shadow'",
		"option max_nodes '60'", "option lan_device 'br-lan'", "list management_port '80'", "option l2tp_concurrency '4'",
		"option proxy_concurrency '4'", "option connect_timeout '30s'", "option stop_timeout '20s'",
	}, "\n")
}
