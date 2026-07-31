package config

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
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
	node, ok := roundTripped.Nodes["node-b"]
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
	device, ok := roundTripped.Devices["device-b"]
	if !ok || device.MAC != "00:11:22:33:44:66" || device.Hostname != "Bob's phone" || device.FixedIPv4.String() != "192.0.2.11" || device.NodeID != "node-b" || device.Enabled {
		t.Fatalf("round-tripped device differs")
	}
}

func TestEncodeOrdersSectionsAndQuotesApostrophes(t *testing.T) {
	cfg := validConfig()
	first := cfg.Nodes["node-a"]
	first.Name = "Alice's node"
	cfg.Nodes["node-a"] = first

	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode(): %v", err)
	}
	text := encoded.String()
	globalAt := strings.Index(text, "config global 'global'")
	nodeAAt := strings.Index(text, "config node 'node-a'")
	nodeBAt := strings.Index(text, "config node 'node-b'")
	deviceAAt := strings.Index(text, "config device 'device-a'")
	deviceBAt := strings.Index(text, "config device 'device-b'")
	if globalAt < 0 || nodeAAt < globalAt || nodeBAt < nodeAAt || deviceAAt < nodeBAt || deviceBAt < deviceAAt {
		t.Fatal("sections are not in global, sorted node, sorted device order")
	}
	decoded, err := Decode(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Decode(encoded apostrophe): %v", err)
	}
	if decoded.Nodes["node-a"].Name != "Alice's node" {
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
			"node-b": {ID: "node-b", Name: "Node B", Protocol: model.ProtocolSOCKS5, Enabled: true, Server: "b.example", Port: 1080, PolicyID: 2},
			"node-a": {ID: "node-a", Name: "Node A", Protocol: model.ProtocolSOCKS5, Enabled: true, Server: "a.example", Port: 1080, PolicyID: 1},
		},
		Devices: map[string]model.Device{
			"device-b": {ID: "device-b", MAC: "00:11:22:33:44:66", Hostname: "B", FixedIPv4: mustAddr("192.0.2.11"), NodeID: "node-b", Enabled: true},
			"device-a": {ID: "device-a", MAC: "00:11:22:33:44:55", Hostname: "A", FixedIPv4: mustAddr("192.0.2.10"), NodeID: "node-a", Enabled: true},
		},
	}
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
