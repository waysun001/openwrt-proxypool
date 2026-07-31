package config

import (
	"bufio"
	"errors"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"proxypoold/internal/model"
)

// Decode reads the strict, named-section UCI subset used by ProxyPool V2.
func Decode(r io.Reader) (model.DesiredConfig, error) {
	sections, err := parseUCI(r)
	if err != nil {
		return model.DesiredConfig{}, invalidConfig()
	}

	cfg := model.DesiredConfig{Nodes: make(map[string]model.Node), Devices: make(map[string]model.Device)}
	var global *uciSection
	for _, section := range sections {
		switch section.kind {
		case "global":
			if section.name != "global" || global != nil {
				return model.DesiredConfig{}, invalidConfig()
			}
			global = section
		case "node":
			if section.name == "" {
				return model.DesiredConfig{}, invalidConfig()
			}
			if _, exists := cfg.Nodes[section.name]; exists {
				return model.DesiredConfig{}, invalidConfig()
			}
			node, err := decodeNode(section)
			if err != nil {
				return model.DesiredConfig{}, invalidConfig()
			}
			cfg.Nodes[section.name] = node
		case "device":
			if section.name == "" {
				return model.DesiredConfig{}, invalidConfig()
			}
			if _, exists := cfg.Devices[section.name]; exists {
				return model.DesiredConfig{}, invalidConfig()
			}
			device, err := decodeDevice(section)
			if err != nil {
				return model.DesiredConfig{}, invalidConfig()
			}
			cfg.Devices[section.name] = device
		default:
			return model.DesiredConfig{}, invalidConfig()
		}
	}
	if global == nil {
		return model.DesiredConfig{}, invalidConfig()
	}
	var errGlobal error
	cfg.SchemaVersion, cfg.Revision, cfg.Global, errGlobal = decodeGlobal(global)
	if errGlobal != nil || validateCodecConfig(cfg) != nil {
		return model.DesiredConfig{}, invalidConfig()
	}
	return cfg, nil
}

// Encode writes a canonical, deterministic UCI representation.
func Encode(w io.Writer, cfg model.DesiredConfig) error {
	if validateCodecConfig(cfg) != nil {
		return invalidConfig()
	}
	var output strings.Builder
	writeSection(&output, "global", "global")
	writeOption(&output, "schema_version", strconv.Itoa(cfg.SchemaVersion))
	writeOption(&output, "revision", strconv.FormatUint(cfg.Revision, 10))
	writeOption(&output, "enabled", boolText(cfg.Global.Enabled))
	writeOption(&output, "runtime_backend", cfg.Global.RuntimeBackend)
	writeOption(&output, "max_nodes", strconv.Itoa(cfg.Global.MaxNodes))
	writeOption(&output, "lan_device", cfg.Global.LANDevice)
	for _, port := range cfg.Global.ManagementPorts {
		writeList(&output, "management_port", strconv.FormatUint(uint64(port), 10))
	}
	writeOption(&output, "l2tp_concurrency", strconv.Itoa(cfg.Global.L2TPConcurrency))
	writeOption(&output, "proxy_concurrency", strconv.Itoa(cfg.Global.ProxyConcurrency))
	writeOption(&output, "connect_timeout", cfg.Global.ConnectTimeout.String())
	writeOption(&output, "stop_timeout", cfg.Global.StopTimeout.String())
	for _, endpoint := range cfg.Global.DoHEndpoints {
		writeList(&output, "doh_url", endpoint.URL)
	}
	for _, endpoint := range cfg.Global.DoHEndpoints {
		writeList(&output, "doh_bootstrap_ip", endpoint.BootstrapIP)
	}
	for _, endpoint := range cfg.Global.DoHEndpoints {
		writeList(&output, "doh_server_name", endpoint.ServerName)
	}

	for _, id := range sortedNodeIDs(cfg.Nodes) {
		node := cfg.Nodes[id]
		output.WriteByte('\n')
		writeSection(&output, "node", id)
		writeOption(&output, "name", node.Name)
		writeOption(&output, "protocol", string(node.Protocol))
		writeOption(&output, "enabled", boolText(node.Enabled))
		writeOption(&output, "server", node.Server)
		writeOption(&output, "port", strconv.FormatUint(uint64(node.Port), 10))
		writeOption(&output, "username", node.Username)
		writeOption(&output, "password", node.Password)
		writeOption(&output, "slp_token", node.SLPToken)
		writeOption(&output, "slp_transport", node.SLPTransport)
		writeOption(&output, "slp_obfs", boolText(node.SLPObfs))
		writeOption(&output, "slp_obfs_key", node.SLPObfsKey)
		writeOption(&output, "slp_insecure", boolText(node.SLPInsecure))
		if node.ExpiresAt == nil {
			writeOption(&output, "expires_at", "")
		} else {
			writeOption(&output, "expires_at", node.ExpiresAt.UTC().Format(time.RFC3339Nano))
		}
		writeOption(&output, "policy_id", strconv.FormatUint(uint64(node.PolicyID), 10))
		writeOption(&output, "revision", strconv.FormatUint(node.Revision, 10))
	}
	for _, id := range sortedDeviceIDs(cfg.Devices) {
		device := cfg.Devices[id]
		output.WriteByte('\n')
		writeSection(&output, "device", id)
		writeOption(&output, "mac", device.MAC)
		writeOption(&output, "hostname", device.Hostname)
		writeOption(&output, "fixed_ipv4", device.FixedIPv4.String())
		writeOption(&output, "node_id", device.NodeID)
		writeOption(&output, "enabled", boolText(device.Enabled))
	}
	if _, err := io.WriteString(w, output.String()); err != nil {
		return errors.New("configuration write failed")
	}
	return nil
}

type uciSection struct {
	kind    string
	name    string
	options map[string]string
	lists   map[string][]string
}

func parseUCI(r io.Reader) ([]*uciSection, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	var sections []*uciSection
	var current *uciSection
	for scanner.Scan() {
		fields, err := uciTokens(scanner.Text())
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return nil, errors.New("invalid uci statement")
		}
		switch fields[0] {
		case "config":
			current = &uciSection{kind: fields[1], name: fields[2], options: make(map[string]string), lists: make(map[string][]string)}
			sections = append(sections, current)
		case "option":
			if current == nil || fields[1] == "" {
				return nil, errors.New("option without section")
			}
			if _, exists := current.options[fields[1]]; exists {
				return nil, errors.New("duplicate option")
			}
			if _, isList := current.lists[fields[1]]; isList {
				return nil, errors.New("option/list collision")
			}
			current.options[fields[1]] = fields[2]
		case "list":
			if current == nil || fields[1] == "" {
				return nil, errors.New("list without section")
			}
			if _, isOption := current.options[fields[1]]; isOption {
				return nil, errors.New("option/list collision")
			}
			current.lists[fields[1]] = append(current.lists[fields[1]], fields[2])
		default:
			return nil, errors.New("unsupported uci keyword")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return sections, nil
}

func uciTokens(line string) ([]string, error) {
	var fields []string
	var token strings.Builder
	inToken := false
	var quote rune
	escaped := false
	flush := func() { fields = append(fields, token.String()); token.Reset(); inToken = false }
	for _, character := range line {
		if escaped {
			token.WriteRune(character)
			inToken = true
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			inToken = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				token.WriteRune(character)
			}
			inToken = true
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
			inToken = true
		case '#':
			if inToken {
				token.WriteRune(character)
			} else {
				return fields, nil
			}
		case ' ', '\t', '\r':
			if inToken {
				flush()
			}
		default:
			token.WriteRune(character)
			inToken = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated uci value")
	}
	if inToken {
		flush()
	}
	return fields, nil
}

func decodeGlobal(section *uciSection) (int, uint64, model.GlobalConfig, error) {
	if !onlyKeys(section.options, globalOptions) || !onlyKeys(section.lists, globalLists) || !hasAll(section.options, globalOptions) {
		return 0, 0, model.GlobalConfig{}, errors.New("invalid global options")
	}
	schemaVersion, err := parseInt(section.options["schema_version"])
	if err != nil {
		return 0, 0, model.GlobalConfig{}, err
	}
	revision, err := strconv.ParseUint(section.options["revision"], 10, 64)
	if err != nil {
		return 0, 0, model.GlobalConfig{}, err
	}
	enabled, err := parseBool(section.options["enabled"])
	if err != nil {
		return 0, 0, model.GlobalConfig{}, err
	}
	maxNodes, err := parseInt(section.options["max_nodes"])
	if err != nil {
		return 0, 0, model.GlobalConfig{}, err
	}
	l2tp, err := parseInt(section.options["l2tp_concurrency"])
	if err != nil {
		return 0, 0, model.GlobalConfig{}, err
	}
	proxy, err := parseInt(section.options["proxy_concurrency"])
	if err != nil {
		return 0, 0, model.GlobalConfig{}, err
	}
	connectTimeout, err := time.ParseDuration(section.options["connect_timeout"])
	if err != nil {
		return 0, 0, model.GlobalConfig{}, err
	}
	stopTimeout, err := time.ParseDuration(section.options["stop_timeout"])
	if err != nil {
		return 0, 0, model.GlobalConfig{}, err
	}
	ports := make([]uint16, len(section.lists["management_port"]))
	for i, value := range section.lists["management_port"] {
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil || port == 0 {
			return 0, 0, model.GlobalConfig{}, errors.New("invalid management port")
		}
		ports[i] = uint16(port)
	}
	urls, addresses, names := section.lists["doh_url"], section.lists["doh_bootstrap_ip"], section.lists["doh_server_name"]
	if len(urls) != len(addresses) || len(urls) != len(names) {
		return 0, 0, model.GlobalConfig{}, errors.New("invalid doh lists")
	}
	doh := make([]model.DoHEndpoint, len(urls))
	for i := range urls {
		if urls[i] == "" || names[i] == "" {
			return 0, 0, model.GlobalConfig{}, errors.New("invalid doh endpoint")
		}
		if _, err := netip.ParseAddr(addresses[i]); err != nil {
			return 0, 0, model.GlobalConfig{}, err
		}
		doh[i] = model.DoHEndpoint{URL: urls[i], BootstrapIP: addresses[i], ServerName: names[i]}
	}
	return schemaVersion, revision, model.GlobalConfig{Enabled: enabled, RuntimeBackend: section.options["runtime_backend"], MaxNodes: maxNodes, LANDevice: section.options["lan_device"], ManagementPorts: ports, L2TPConcurrency: l2tp, ProxyConcurrency: proxy, ConnectTimeout: connectTimeout, StopTimeout: stopTimeout, DoHEndpoints: doh}, nil
}

func decodeNode(section *uciSection) (model.Node, error) {
	if !onlyKeys(section.options, nodeOptions) || len(section.lists) != 0 || !hasAll(section.options, nodeOptions) {
		return model.Node{}, errors.New("invalid node options")
	}
	enabled, err := parseBool(section.options["enabled"])
	if err != nil {
		return model.Node{}, err
	}
	port, err := strconv.ParseUint(section.options["port"], 10, 16)
	if err != nil || port == 0 {
		return model.Node{}, errors.New("invalid port")
	}
	slpObfs, err := parseBool(section.options["slp_obfs"])
	if err != nil {
		return model.Node{}, err
	}
	slpInsecure, err := parseBool(section.options["slp_insecure"])
	if err != nil {
		return model.Node{}, err
	}
	policyID, err := strconv.ParseUint(section.options["policy_id"], 10, 16)
	if err != nil || policyID == 0 {
		return model.Node{}, errors.New("invalid policy id")
	}
	revision, err := strconv.ParseUint(section.options["revision"], 10, 64)
	if err != nil {
		return model.Node{}, err
	}
	var expiresAt *time.Time
	if value := section.options["expires_at"]; value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return model.Node{}, err
		}
		expiresAt = &parsed
	}
	return model.Node{ID: section.name, Name: section.options["name"], Protocol: model.Protocol(section.options["protocol"]), Enabled: enabled, Server: section.options["server"], Port: uint16(port), Username: section.options["username"], Password: section.options["password"], SLPToken: section.options["slp_token"], SLPTransport: section.options["slp_transport"], SLPObfs: slpObfs, SLPObfsKey: section.options["slp_obfs_key"], SLPInsecure: slpInsecure, ExpiresAt: expiresAt, PolicyID: uint16(policyID), Revision: revision}, nil
}

func decodeDevice(section *uciSection) (model.Device, error) {
	if !onlyKeys(section.options, deviceOptions) || len(section.lists) != 0 || !hasAll(section.options, deviceOptions) {
		return model.Device{}, errors.New("invalid device options")
	}
	enabled, err := parseBool(section.options["enabled"])
	if err != nil {
		return model.Device{}, err
	}
	address, err := netip.ParseAddr(section.options["fixed_ipv4"])
	if err != nil || !address.Is4() {
		return model.Device{}, errors.New("invalid fixed ipv4")
	}
	return model.Device{ID: section.name, MAC: section.options["mac"], Hostname: section.options["hostname"], FixedIPv4: address, NodeID: section.options["node_id"], Enabled: enabled}, nil
}

var globalOptions = []string{"schema_version", "revision", "enabled", "runtime_backend", "max_nodes", "lan_device", "l2tp_concurrency", "proxy_concurrency", "connect_timeout", "stop_timeout"}
var globalLists = []string{"management_port", "doh_url", "doh_bootstrap_ip", "doh_server_name"}
var nodeOptions = []string{"name", "protocol", "enabled", "server", "port", "username", "password", "slp_token", "slp_transport", "slp_obfs", "slp_obfs_key", "slp_insecure", "expires_at", "policy_id", "revision"}
var deviceOptions = []string{"mac", "hostname", "fixed_ipv4", "node_id", "enabled"}

func onlyKeys[V any](values map[string]V, allowed []string) bool {
	for key := range values {
		if !contains(allowed, key) {
			return false
		}
	}
	return true
}
func hasAll(values map[string]string, required []string) bool {
	for _, key := range required {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func parseInt(value string) (int, error) { return strconv.Atoi(value) }
func parseBool(value string) (bool, error) {
	if value == "1" {
		return true, nil
	}
	if value == "0" {
		return false, nil
	}
	return false, errors.New("invalid boolean")
}
func boolText(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
func invalidConfig() error {
	return &model.CodeError{Code: "invalid_config", Message: "configuration is invalid"}
}

func validateCodecConfig(cfg model.DesiredConfig) error {
	if model.Validate(cfg) != nil {
		return invalidConfig()
	}
	global := cfg.Global
	if global.RuntimeBackend != "v1" && global.RuntimeBackend != "v2_shadow" {
		return invalidConfig()
	}
	if global.MaxNodes < 1 || global.MaxNodes > 60 || global.LANDevice == "" || global.L2TPConcurrency < 1 || global.ProxyConcurrency < 1 || global.ConnectTimeout <= 0 || global.StopTimeout <= 0 {
		return invalidConfig()
	}
	ports := make(map[uint16]struct{}, len(global.ManagementPorts))
	for _, port := range global.ManagementPorts {
		if port == 0 {
			return invalidConfig()
		}
		if _, exists := ports[port]; exists {
			return invalidConfig()
		}
		ports[port] = struct{}{}
	}
	for _, endpoint := range global.DoHEndpoints {
		if endpoint.URL == "" || endpoint.ServerName == "" {
			return invalidConfig()
		}
		if _, err := netip.ParseAddr(endpoint.BootstrapIP); err != nil {
			return invalidConfig()
		}
	}
	return nil
}
func writeSection(out *strings.Builder, kind, name string) {
	out.WriteString("config ")
	out.WriteString(kind)
	out.WriteByte(' ')
	out.WriteString(quoteUCI(name))
	out.WriteByte('\n')
}
func writeOption(out *strings.Builder, key, value string) {
	out.WriteString("\toption ")
	out.WriteString(key)
	out.WriteByte(' ')
	out.WriteString(quoteUCI(value))
	out.WriteByte('\n')
}
func writeList(out *strings.Builder, key, value string) {
	out.WriteString("\tlist ")
	out.WriteString(key)
	out.WriteByte(' ')
	out.WriteString(quoteUCI(value))
	out.WriteByte('\n')
}
func quoteUCI(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func sortedNodeIDs(nodes map[string]model.Node) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func sortedDeviceIDs(devices map[string]model.Device) []string {
	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
