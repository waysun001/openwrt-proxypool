package openwrt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
	socksprotocol "proxypoold/internal/socks5"
)

const (
	defaultSOCKS5StateDir   = "/var/run/proxypool/nodes"
	defaultSOCKS5ProcRoot   = "/proc"
	defaultSOCKS5BootIDPath = "/proc/sys/kernel/random/boot_id"
	redsocksPath            = "/usr/sbin/redsocks"
	killPath                = "/bin/kill"
	socks5ManifestSchema    = 1
	socks5BasePort          = 12000
	socks5ProbeTarget       = "223.5.5.5:443"
	maxSOCKS5StateFileBytes = 128 << 10
)

var errSOCKS5OwnershipAbsent = errors.New("SOCKS5 ownership is absent")

type SOCKS5Probe func(context.Context, string, string, string, string) error

type SOCKS5Option func(*SOCKS5Adapter)

func WithSOCKS5ProcRoot(path string) SOCKS5Option {
	return func(adapter *SOCKS5Adapter) { adapter.procRoot = path }
}

func WithSOCKS5BootIDPath(path string) SOCKS5Option {
	return func(adapter *SOCKS5Adapter) { adapter.bootIDPath = path }
}

func WithSOCKS5PollInterval(interval time.Duration) SOCKS5Option {
	return func(adapter *SOCKS5Adapter) {
		if interval > 0 {
			adapter.pollInterval = interval
		}
	}
}

func WithSOCKS5TerminateGrace(grace time.Duration) SOCKS5Option {
	return func(adapter *SOCKS5Adapter) {
		if grace > 0 {
			adapter.terminateGrace = grace
		}
	}
}

func WithSOCKS5Probe(probe SOCKS5Probe) SOCKS5Option {
	return func(adapter *SOCKS5Adapter) {
		if probe != nil {
			adapter.probe = probe
		}
	}
}

func WithSOCKS5Clock(clock func() time.Time) SOCKS5Option {
	return func(adapter *SOCKS5Adapter) {
		if clock != nil {
			adapter.now = clock
		}
	}
}

type SOCKS5Adapter struct {
	runner         platform.InputCommandRunner
	resolver       L2TPEndpointResolver
	stateDir       string
	procRoot       string
	bootIDPath     string
	pollInterval   time.Duration
	terminateGrace time.Duration
	probe          SOCKS5Probe
	now            func() time.Time
}

func NewSOCKS5Adapter(runner platform.InputCommandRunner, resolver L2TPEndpointResolver, stateDir string, options ...SOCKS5Option) *SOCKS5Adapter {
	if stateDir == "" {
		stateDir = defaultSOCKS5StateDir
	}
	adapter := &SOCKS5Adapter{
		runner: runner, resolver: resolver, stateDir: stateDir, procRoot: defaultSOCKS5ProcRoot,
		bootIDPath: defaultSOCKS5BootIDPath, pollInterval: 100 * time.Millisecond,
		terminateGrace: 500 * time.Millisecond, probe: defaultSOCKS5Probe, now: time.Now,
	}
	for _, option := range options {
		if option != nil {
			option(adapter)
		}
	}
	return adapter
}

func (adapter *SOCKS5Adapter) Start(ctx context.Context, request platform.NodeRequest) (platform.Session, error) {
	identity, err := adapter.identity(request, true)
	if err != nil {
		return platform.Session{}, err
	}
	endpoint, err := adapter.resolveEndpoint(ctx, request.Node.Server)
	if err != nil {
		return platform.Session{}, err
	}
	identity.endpoint = endpoint
	identity.proxyAddress = net.JoinHostPort(endpoint.String(), strconv.Itoa(int(request.Node.Port)))
	configuration, err := renderSOCKS5Config(identity, request.Node.Username, request.Node.Password)
	if err != nil {
		return platform.Session{}, &model.CodeError{Code: "invalid_config", Message: "SOCKS5 configuration is invalid"}
	}
	identity.digest = socks5OwnershipDigest(request, identity)
	session := platform.Session{
		NodeID: request.Node.ID, Generation: request.Generation, Protocol: model.ProtocolSOCKS5,
		Interface: identity.interfaceName, LocalPort: identity.localPort,
		StartedAt: adapter.now().UTC().Round(0), OwnershipDigest: identity.digest,
	}

	if entry, loadErr := adapter.loadOwnership(identity); loadErr == nil {
		if !entry.matches(request, identity) {
			return session, errors.New("SOCKS5 process ownership conflicts")
		}
		if err := adapter.proveActive(entry, identity, configuration, true); err != nil {
			return session, err
		}
		session.StartedAt = entry.StartedAt
		return session, nil
	} else if !errors.Is(loadErr, errSOCKS5OwnershipAbsent) {
		return session, loadErr
	}

	if err := adapter.prepareState(identity); err != nil {
		return session, err
	}
	if err := adapter.rejectLiveUnownedPID(identity); err != nil {
		return session, err
	}
	if err := writePrivateFile(identity.configPath, configuration); err != nil {
		return session, errors.New("SOCKS5 configuration write failed")
	}
	bootID, err := readBootID(adapter.bootIDPath)
	if err != nil {
		return session, errors.New("SOCKS5 boot identity is unavailable")
	}
	entry := socks5Ownership{
		SchemaVersion: socks5ManifestSchema, State: "reserved", BootID: bootID,
		NodeID: request.Node.ID, PolicyID: request.Node.PolicyID, ConfigRevision: request.Node.Revision,
		Generation: request.Generation, ConfigPath: identity.configPath, PIDPath: identity.pidPath,
		Listener: identity.listener.String(), ProxyEndpoint: identity.proxyAddress,
		OwnershipDigest: identity.digest, StartedAt: session.StartedAt,
	}
	if err := adapter.saveOwnership(identity, entry); err != nil {
		return session, err
	}
	_ = os.Remove(identity.pidPath)
	if _, err := adapter.runner.Run(ctx, redsocksPath, "-c", identity.configPath, "-p", identity.pidPath); err != nil {
		return session, errors.New("SOCKS5 process start failed")
	}

	for {
		pid, pidErr := readPID(identity.pidPath)
		if pidErr == nil {
			if chmodErr := os.Chmod(identity.pidPath, 0o600); chmodErr != nil {
				return session, errors.New("SOCKS5 PID file permission failed")
			}
			evidence, exists, inspectErr := adapter.inspectProcess(pid, identity)
			if inspectErr != nil {
				return session, inspectErr
			}
			if exists {
				if !evidence.commandMatches || !evidence.executableMatches {
					return session, errors.New("SOCKS5 process ownership conflicts")
				}
				if evidence.listenerOwned {
					entry.State = "active"
					entry.PID = pid
					entry.ProcessStartTime = evidence.startTime
					if err := adapter.saveOwnership(identity, entry); err != nil {
						return session, err
					}
					return session, nil
				}
			}
		} else if !errors.Is(pidErr, os.ErrNotExist) {
			return session, pidErr
		}
		select {
		case <-ctx.Done():
			return session, ctx.Err()
		case <-time.After(adapter.pollInterval):
		}
	}
}

func (adapter *SOCKS5Adapter) Probe(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	identity, err := adapter.identity(request, true)
	if err != nil {
		return err
	}
	if session.NodeID != request.Node.ID || session.Generation != request.Generation || session.Protocol != model.ProtocolSOCKS5 ||
		session.Interface != identity.interfaceName || session.LocalPort != identity.localPort || session.OwnershipDigest == "" {
		return errors.New("SOCKS5 session ownership is stale")
	}
	entry, err := adapter.loadOwnership(identity)
	if err != nil {
		return errors.New("SOCKS5 process ownership verification failed")
	}
	host, port, splitErr := net.SplitHostPort(entry.ProxyEndpoint)
	parsedEndpoint, parseErr := netip.ParseAddr(host)
	if splitErr != nil || parseErr != nil || !validSOCKS5Endpoint(parsedEndpoint) || port != strconv.Itoa(int(request.Node.Port)) {
		return errors.New("SOCKS5 process ownership verification failed")
	}
	identity.endpoint = parsedEndpoint
	identity.proxyAddress = entry.ProxyEndpoint
	identity.digest = socks5OwnershipDigest(request, identity)
	if !entry.matches(request, identity) || entry.OwnershipDigest != session.OwnershipDigest {
		return errors.New("SOCKS5 process ownership verification failed")
	}
	configuration, renderErr := renderSOCKS5Config(identity, request.Node.Username, request.Node.Password)
	if renderErr != nil || adapter.proveActive(entry, identity, configuration, true) != nil {
		return errors.New("SOCKS5 process ownership verification failed")
	}
	if err := adapter.probe(ctx, entry.ProxyEndpoint, request.Node.Username, request.Node.Password, socks5ProbeTarget); err != nil {
		return classifySOCKS5ProbeError(err)
	}
	return nil
}

func (adapter *SOCKS5Adapter) Stop(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	identity, err := adapter.identity(request, false)
	if err != nil {
		return err
	}
	entry, err := adapter.loadOwnership(identity)
	if errors.Is(err, errSOCKS5OwnershipAbsent) {
		pid, pidErr := readPID(identity.pidPath)
		if os.IsNotExist(pidErr) {
			return adapter.cleanupState(identity)
		}
		if pidErr != nil {
			return errors.New("SOCKS5 process ownership verification failed")
		}
		_, exists, inspectErr := adapter.inspectProcess(pid, identity)
		if inspectErr != nil || exists {
			return errors.New("SOCKS5 process ownership verification failed")
		}
		return adapter.cleanupState(identity)
	}
	if err != nil {
		return err
	}
	if entry.NodeID != request.Node.ID || entry.PolicyID != request.Node.PolicyID || entry.Generation > request.Generation ||
		(session != (platform.Session{}) && (session.NodeID != entry.NodeID || session.Generation != entry.Generation ||
			session.Protocol != model.ProtocolSOCKS5 || session.LocalPort != identity.localPort || session.OwnershipDigest != entry.OwnershipDigest)) {
		return errors.New("SOCKS5 process ownership verification failed")
	}
	pid := entry.PID
	if pid == 0 {
		pid, err = readPID(identity.pidPath)
		if errors.Is(err, os.ErrNotExist) {
			return adapter.cleanupState(identity)
		}
		if err != nil {
			return errors.New("SOCKS5 process ownership verification failed")
		}
	}
	evidence, exists, err := adapter.inspectProcess(pid, identity)
	if err != nil {
		return err
	}
	if !exists {
		return adapter.cleanupState(identity)
	}
	if !evidence.executableMatches || !evidence.commandMatches || (entry.State == "active" && evidence.startTime != entry.ProcessStartTime) {
		return errors.New("SOCKS5 process ownership verification failed")
	}
	if _, err := adapter.runner.Run(ctx, killPath, "-TERM", strconv.Itoa(pid)); err != nil {
		return errors.New("SOCKS5 process termination failed")
	}
	terminateTimer := time.NewTimer(adapter.terminateGrace)
	defer terminateTimer.Stop()
	escalated := false
	for {
		evidence, exists, inspectErr := adapter.inspectProcess(pid, identity)
		if inspectErr != nil {
			return inspectErr
		}
		if !exists {
			return adapter.cleanupState(identity)
		}
		if !evidence.executableMatches || !evidence.commandMatches || (entry.State == "active" && evidence.startTime != entry.ProcessStartTime) {
			return errors.New("SOCKS5 process ownership verification failed")
		}
		if escalated {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(adapter.pollInterval):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-terminateTimer.C:
			if _, err := adapter.runner.Run(ctx, killPath, "-KILL", strconv.Itoa(pid)); err != nil {
				return errors.New("SOCKS5 process kill failed")
			}
			escalated = true
		case <-time.After(adapter.pollInterval):
		}
	}
}

type socks5Identity struct {
	nodeDir       string
	configPath    string
	pidPath       string
	manifestPath  string
	interfaceName string
	localPort     uint16
	listener      netip.AddrPort
	endpoint      netip.Addr
	proxyAddress  string
	digest        string
}

func (adapter *SOCKS5Adapter) identity(request platform.NodeRequest, requireEnabled bool) (socks5Identity, error) {
	if adapter == nil || adapter.runner == nil || adapter.resolver == nil || adapter.probe == nil || adapter.now == nil ||
		!filepath.IsAbs(adapter.stateDir) || !filepath.IsAbs(adapter.procRoot) || !filepath.IsAbs(adapter.bootIDPath) ||
		adapter.pollInterval <= 0 || adapter.terminateGrace <= 0 ||
		!safeOwnershipID.MatchString(request.Node.ID) || request.Node.Protocol != model.ProtocolSOCKS5 ||
		(requireEnabled && !request.Node.Enabled) || request.Node.PolicyID == 0 || request.Node.PolicyID > 60 || request.Node.Revision == 0 ||
		request.Generation == 0 || request.Node.Port == 0 || !validL2TPServer(request.Node.Server) ||
		(request.Node.Username == "") != (request.Node.Password == "") || !validSOCKS5Credential(request.Node.Username) ||
		!validSOCKS5Credential(request.Node.Password) {
		return socks5Identity{}, errors.New("SOCKS5 request is invalid")
	}
	nodeDir := filepath.Join(adapter.stateDir, request.Node.ID)
	localPort := uint16(socks5BasePort + int(request.Node.PolicyID))
	return socks5Identity{
		nodeDir: nodeDir, configPath: filepath.Join(nodeDir, "redsocks.conf"), pidPath: filepath.Join(nodeDir, "redsocks.pid"),
		manifestPath: filepath.Join(nodeDir, "ownership.json"), interfaceName: fmt.Sprintf("psx%04d", request.Node.PolicyID),
		localPort: localPort, listener: netip.AddrPortFrom(netip.MustParseAddr("192.168.9.1"), localPort),
	}, nil
}

func validSOCKS5Credential(value string) bool {
	if len(value) > 255 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (adapter *SOCKS5Adapter) resolveEndpoint(ctx context.Context, server string) (netip.Addr, error) {
	if address, err := netip.ParseAddr(server); err == nil {
		if validSOCKS5Endpoint(address) {
			return address.Unmap(), nil
		}
		return netip.Addr{}, &model.CodeError{Code: "resolve_failed", Message: "SOCKS5 endpoint is invalid"}
	}
	address, err := adapter.resolver.ResolveIPv4(ctx, server)
	if err != nil || !validSOCKS5Endpoint(address) {
		return netip.Addr{}, &model.CodeError{Code: "resolve_failed", Message: "SOCKS5 endpoint resolution failed"}
	}
	return address.Unmap(), nil
}

func validSOCKS5Endpoint(address netip.Addr) bool {
	return address.Is4() && address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast()
}

func renderSOCKS5Config(identity socks5Identity, username, password string) ([]byte, error) {
	if !identity.endpoint.Is4() || !identity.listener.IsValid() || identity.localPort == 0 || !validSOCKS5Credential(username) ||
		!validSOCKS5Credential(password) || (username == "") != (password == "") {
		return nil, errors.New("invalid SOCKS5 config")
	}
	var auth strings.Builder
	if username != "" {
		_, _ = fmt.Fprintf(&auth, "    login = \"%s\";\n    password = \"%s\";\n", escapeRedsocksString(username), escapeRedsocksString(password))
	}
	return []byte(fmt.Sprintf(`base {
    log_debug = off;
    log_info = off;
    log = "syslog:daemon";
    daemon = on;
    redirector = iptables;
}

redsocks {
    local_ip = %s;
    local_port = %d;
    ip = %s;
    port = %s;
    type = socks5;
%s}
`, identity.listener.Addr(), identity.localPort, identity.endpoint, proxyPort(identity.proxyAddress), auth.String())), nil
}

func proxyPort(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "0"
	}
	return port
}

func escapeRedsocksString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

func (adapter *SOCKS5Adapter) prepareState(identity socks5Identity) error {
	if err := ensurePrivateDirectory(adapter.stateDir); err != nil {
		return errors.New("SOCKS5 state directory is unsafe")
	}
	if err := ensurePrivateDirectory(identity.nodeDir); err != nil {
		return errors.New("SOCKS5 node directory is unsafe")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("unsafe directory")
	}
	return nil
}

func writePrivateFile(path string, contents []byte) error {
	if len(contents) > maxSOCKS5StateFileBytes {
		return errors.New("state file exceeds limit")
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".proxypool-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("state file is unsafe")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(temporary, path)
}

func readPrivateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > maxSOCKS5StateFileBytes {
		return nil, errors.New("state file is unsafe")
	}
	return os.ReadFile(path)
}

type socks5Ownership struct {
	SchemaVersion    int       `json:"schema_version"`
	State            string    `json:"state"`
	BootID           string    `json:"boot_id"`
	NodeID           string    `json:"node_id"`
	PolicyID         uint16    `json:"policy_id"`
	ConfigRevision   uint64    `json:"config_revision"`
	Generation       uint64    `json:"generation"`
	PID              int       `json:"pid,omitempty"`
	ProcessStartTime uint64    `json:"process_start_time,omitempty"`
	ConfigPath       string    `json:"config_path"`
	PIDPath          string    `json:"pid_path"`
	Listener         string    `json:"listener"`
	ProxyEndpoint    string    `json:"proxy_endpoint"`
	OwnershipDigest  string    `json:"ownership_digest"`
	StartedAt        time.Time `json:"started_at"`
}

func (entry socks5Ownership) matches(request platform.NodeRequest, identity socks5Identity) bool {
	return entry.SchemaVersion == socks5ManifestSchema && entry.State == "active" && entry.NodeID == request.Node.ID &&
		entry.PolicyID == request.Node.PolicyID && entry.ConfigRevision == request.Node.Revision && entry.Generation == request.Generation &&
		entry.PID > 0 && entry.ProcessStartTime > 0 && entry.ConfigPath == identity.configPath && entry.PIDPath == identity.pidPath &&
		entry.Listener == identity.listener.String() && entry.OwnershipDigest == identity.digest && !entry.StartedAt.IsZero()
}

func (adapter *SOCKS5Adapter) loadOwnership(identity socks5Identity) (socks5Ownership, error) {
	contents, err := readPrivateFile(identity.manifestPath)
	if os.IsNotExist(err) {
		return socks5Ownership{}, errSOCKS5OwnershipAbsent
	}
	if err != nil {
		return socks5Ownership{}, errors.New("SOCKS5 ownership manifest is unsafe")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var entry socks5Ownership
	if err := decoder.Decode(&entry); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return socks5Ownership{}, errors.New("SOCKS5 ownership manifest is invalid")
	}
	bootID, err := readBootID(adapter.bootIDPath)
	if err != nil || entry.SchemaVersion != socks5ManifestSchema || entry.BootID != bootID ||
		(entry.State != "reserved" && entry.State != "active") || !safeOwnershipID.MatchString(entry.NodeID) ||
		entry.PolicyID == 0 || entry.PolicyID > 60 || entry.ConfigRevision == 0 || entry.Generation == 0 ||
		entry.ConfigPath != identity.configPath || entry.PIDPath != identity.pidPath || entry.Listener != identity.listener.String() ||
		len(entry.OwnershipDigest) != sha256.Size*2 || entry.StartedAt.IsZero() ||
		(entry.State == "active" && (entry.PID <= 0 || entry.ProcessStartTime == 0)) {
		return socks5Ownership{}, errors.New("SOCKS5 ownership manifest is invalid")
	}
	return entry, nil
}

func (adapter *SOCKS5Adapter) saveOwnership(identity socks5Identity, entry socks5Ownership) error {
	contents, err := json.Marshal(entry)
	if err != nil {
		return errors.New("SOCKS5 ownership manifest encode failed")
	}
	if err := writePrivateFile(identity.manifestPath, contents); err != nil {
		return errors.New("SOCKS5 ownership manifest write failed")
	}
	return nil
}

func readBootID(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil || len(contents) > 128 {
		return "", errors.New("boot identity unavailable")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" || strings.ContainsAny(value, "\x00\r\n\t ") {
		return "", errors.New("boot identity invalid")
	}
	return value, nil
}

func socks5OwnershipDigest(request platform.NodeRequest, identity socks5Identity) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "proxypool-socks5-v1\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%d",
		request.Node.ID, request.Node.PolicyID, request.Node.Revision, request.Generation,
		identity.listener, identity.endpoint, request.Node.Port)
	return hex.EncodeToString(hash.Sum(nil))
}

type socks5ProcessEvidence struct {
	startTime         uint64
	executableMatches bool
	commandMatches    bool
	listenerOwned     bool
}

func (adapter *SOCKS5Adapter) inspectProcess(pid int, identity socks5Identity) (socks5ProcessEvidence, bool, error) {
	if pid <= 0 {
		return socks5ProcessEvidence{}, false, errors.New("SOCKS5 PID is invalid")
	}
	directory := filepath.Join(adapter.procRoot, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(directory, "stat"))
	if os.IsNotExist(err) {
		return socks5ProcessEvidence{}, false, nil
	}
	if err != nil || len(stat) > 16<<10 {
		return socks5ProcessEvidence{}, false, errors.New("SOCKS5 process inspection failed")
	}
	startTime, err := parseProcessStartTime(string(stat))
	if err != nil {
		return socks5ProcessEvidence{}, false, errors.New("SOCKS5 process inspection failed")
	}
	executable, exeErr := os.Readlink(filepath.Join(directory, "exe"))
	cmdline, cmdErr := os.ReadFile(filepath.Join(directory, "cmdline"))
	if exeErr != nil || cmdErr != nil || len(cmdline) > 16<<10 {
		return socks5ProcessEvidence{}, false, errors.New("SOCKS5 process inspection failed")
	}
	wantCommand := strings.Join([]string{redsocksPath, "-c", identity.configPath, "-p", identity.pidPath, ""}, "\x00")
	owned, err := adapter.processOwnsListener(directory, identity.listener)
	if err != nil {
		return socks5ProcessEvidence{}, false, err
	}
	return socks5ProcessEvidence{
		startTime: startTime, executableMatches: executable == redsocksPath,
		commandMatches: string(cmdline) == wantCommand, listenerOwned: owned,
	}, true, nil
}

func parseProcessStartTime(stat string) (uint64, error) {
	end := strings.LastIndex(stat, ")")
	if end < 0 || end+2 > len(stat) {
		return 0, errors.New("invalid process stat")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) <= 19 {
		return 0, errors.New("invalid process stat")
	}
	value, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || value == 0 {
		return 0, errors.New("invalid process start time")
	}
	return value, nil
}

func (adapter *SOCKS5Adapter) processOwnsListener(procDirectory string, listener netip.AddrPort) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(procDirectory, "fd"))
	if err != nil {
		return false, errors.New("SOCKS5 process descriptors are unavailable")
	}
	inodes := make(map[string]struct{})
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(procDirectory, "fd", entry.Name()))
		if err == nil && strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = struct{}{}
		}
	}
	file, err := os.Open(filepath.Join(adapter.procRoot, "net", "tcp"))
	if err != nil {
		return false, errors.New("SOCKS5 TCP inventory is unavailable")
	}
	defer file.Close()
	wantAddress := procIPv4(listener.Addr()) + ":" + fmt.Sprintf("%04X", listener.Port())
	scanner := bufio.NewScanner(io.LimitReader(file, maxSOCKS5StateFileBytes+1))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 9 && fields[1] == wantAddress && fields[3] == "0A" {
			if _, exists := inodes[fields[9]]; exists {
				return true, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false, errors.New("SOCKS5 TCP inventory read failed")
	}
	return false, nil
}

func procIPv4(address netip.Addr) string {
	bytes := address.As4()
	return fmt.Sprintf("%02X%02X%02X%02X", bytes[3], bytes[2], bytes[1], bytes[0])
}

func (adapter *SOCKS5Adapter) proveActive(entry socks5Ownership, identity socks5Identity, configuration []byte, requireListener bool) error {
	contents, err := readPrivateFile(identity.configPath)
	if err != nil || !bytes.Equal(contents, configuration) {
		return errors.New("SOCKS5 process ownership verification failed")
	}
	evidence, exists, err := adapter.inspectProcess(entry.PID, identity)
	if err != nil || !exists || !evidence.executableMatches || !evidence.commandMatches || evidence.startTime != entry.ProcessStartTime ||
		(requireListener && !evidence.listenerOwned) {
		return errors.New("SOCKS5 process ownership verification failed")
	}
	return nil
}

func (adapter *SOCKS5Adapter) rejectLiveUnownedPID(identity socks5Identity) error {
	pid, err := readPID(identity.pidPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("SOCKS5 PID file is unsafe")
	}
	_, exists, err := adapter.inspectProcess(pid, identity)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("SOCKS5 unowned process conflicts")
	}
	return os.Remove(identity.pidPath)
}

func readPID(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 64 {
		return 0, errors.New("invalid PID file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(contents))
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 1 || strconv.Itoa(pid) != value {
		return 0, errors.New("invalid PID file")
	}
	return pid, nil
}

func (adapter *SOCKS5Adapter) cleanupState(identity socks5Identity) error {
	failed := false
	for _, path := range []string{identity.manifestPath, identity.pidPath, identity.configPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failed = true
		}
	}
	if err := os.Remove(identity.nodeDir); err != nil && !os.IsNotExist(err) {
		failed = true
	}
	if failed {
		return errors.New("SOCKS5 state cleanup failed")
	}
	return nil
}

func defaultSOCKS5Probe(ctx context.Context, proxyAddress, username, password, target string) error {
	conn, err := (socksprotocol.Dialer{ProxyAddress: proxyAddress, Username: username, Password: password}).DialContext(ctx, "tcp4", target)
	if err != nil {
		return err
	}
	return conn.Close()
}

func classifySOCKS5ProbeError(err error) error {
	code := "probe_failed"
	switch socksprotocol.ErrorCode(err) {
	case socksprotocol.CodeAuthentication:
		code = "auth_failed"
	case socksprotocol.CodeResolve:
		code = "resolve_failed"
	case socksprotocol.CodeTimeout:
		code = "connect_timeout"
	case socksprotocol.CodeInvalidConfig:
		code = "invalid_config"
	}
	return &model.CodeError{Code: code, Message: "SOCKS5 connectivity probe failed"}
}

var _ platform.NodeAdapter = (*SOCKS5Adapter)(nil)
