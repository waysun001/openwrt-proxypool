package openwrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

const (
	defaultL2TPManifestPath       = "/var/run/proxypool/l2tp-ownership.json"
	defaultL2TPStatusDir          = "/var/run/proxypool/l2tp-netifd"
	defaultBootIDPath             = "/proc/sys/kernel/random/boot_id"
	l2tpUbusHelperPath            = "/usr/lib/proxypool/ubus-call-stdin.uc"
	maxL2TPManifestBytes          = 128 << 10
	l2tpManifestSchema            = 1
	defaultL2TPNegotiationTimeout = 20 * time.Second
	maxL2TPStatusLogBytes         = 128 << 10
)

var errL2TPOwnershipAbsent = errors.New("L2TP ownership is absent")

// L2TPEndpointResolver converts a configured hostname to the exact IPv4
// endpoint handed to netifd. Implementations must not silently return an IPv6
// address or use a proxy node that has not been admitted yet.
type L2TPEndpointResolver interface {
	ResolveIPv4(context.Context, string) (netip.Addr, error)
}

type L2TPOption func(*L2TPAdapter)

func WithL2TPBootIDPath(path string) L2TPOption {
	return func(adapter *L2TPAdapter) { adapter.bootIDPath = path }
}

func WithL2TPPollInterval(interval time.Duration) L2TPOption {
	return func(adapter *L2TPAdapter) {
		if interval > 0 {
			adapter.pollInterval = interval
		}
	}
}

func WithL2TPNegotiationTimeout(timeout time.Duration) L2TPOption {
	return func(adapter *L2TPAdapter) {
		if timeout > 0 {
			adapter.negotiationTimeout = timeout
		}
	}
}

func WithL2TPClock(clock func() time.Time) L2TPOption {
	return func(adapter *L2TPAdapter) {
		if clock != nil {
			adapter.now = clock
		}
	}
}

type L2TPAdapter struct {
	runner             platform.InputCommandRunner
	resolver           L2TPEndpointResolver
	manifestPath       string
	bootIDPath         string
	pollInterval       time.Duration
	negotiationTimeout time.Duration
	now                func() time.Time
	manifestMu         sync.Mutex
}

func NewL2TPAdapter(runner platform.InputCommandRunner, resolver L2TPEndpointResolver, manifestPath string, options ...L2TPOption) *L2TPAdapter {
	if manifestPath == "" {
		manifestPath = defaultL2TPManifestPath
	}
	adapter := &L2TPAdapter{
		runner: runner, resolver: resolver, manifestPath: manifestPath,
		bootIDPath: defaultBootIDPath, pollInterval: 200 * time.Millisecond,
		negotiationTimeout: defaultL2TPNegotiationTimeout, now: time.Now,
	}
	for _, option := range options {
		if option != nil {
			option(adapter)
		}
	}
	return adapter
}

func (adapter *L2TPAdapter) Start(ctx context.Context, request platform.NodeRequest) (platform.Session, error) {
	logical, l3Device, err := adapter.validateRequest(request, true)
	if err != nil {
		return platform.Session{}, err
	}
	endpoint, err := adapter.resolveEndpoint(ctx, request.Node.Server)
	if err != nil {
		return platform.Session{}, err
	}
	digest := l2tpOwnershipDigest(request, logical, endpoint)
	startedAt := adapter.now().UTC().Round(0)
	session := platform.Session{
		NodeID: request.Node.ID, Generation: request.Generation, Protocol: model.ProtocolL2TP,
		Interface: l3Device, StartedAt: startedAt, OwnershipDigest: digest,
	}
	entry := l2tpOwnership{
		NodeID: request.Node.ID, PolicyID: request.Node.PolicyID, ConfigRevision: request.Node.Revision, Generation: request.Generation,
		LogicalInterface: logical, L3Device: l3Device, Endpoint: endpoint.String(),
		OwnershipDigest: digest, StartedAt: startedAt,
	}

	status, err := adapter.inspectStatus(ctx, logical)
	if err != nil {
		return session, err
	}
	if status.exists {
		ownedEntry, ownershipErr := adapter.startOwnership(entry)
		if ownershipErr != nil {
			return session, errors.New("L2TP interface ownership conflicts")
		}
		session.StartedAt = ownedEntry.StartedAt
		session.OwnershipDigest = ownedEntry.OwnershipDigest
		if err := adapter.waitReady(ctx, logical, l3Device); err != nil {
			return session, err
		}
		return session, nil
	}
	if err := adapter.reserveDormantOwnership(entry); err != nil {
		return session, err
	}
	configuration := l2tpDynamicConfig{
		Name: logical, Proto: "l2tp", Server: endpoint.String() + fmt.Sprintf(":%d", request.Node.Port),
		Username: request.Node.Username, Password: request.Node.Password, IPv6: false,
		Keepalive: "3 5", MTU: 1400, CheckupInterval: 5, PPPDOptions: "noauth",
	}
	input, err := json.Marshal(configuration)
	if err != nil {
		return session, errors.New("L2TP dynamic configuration failed")
	}
	if _, err := adapter.runner.RunInput(ctx, input, "/usr/bin/ucode", l2tpUbusHelperPath, "network", "add_dynamic"); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return session, fmt.Errorf("L2TP dynamic interface creation failed: %w", contextErr)
		}
		return session, l2tpFailure("l2tp_interface_failed", "L2TP dynamic interface creation failed")
	}
	if err := adapter.waitReady(ctx, logical, l3Device); err != nil {
		return session, err
	}
	return session, nil
}

func (adapter *L2TPAdapter) Probe(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	logical, l3Device, err := adapter.validateRequest(request, true)
	if err != nil {
		return err
	}
	if !exactL2TPSession(request, session, l3Device) {
		return errors.New("L2TP session ownership is stale")
	}
	if _, err := adapter.sessionOwnership(request, session, logical, l3Device); err != nil {
		return errors.New("L2TP ownership verification failed")
	}
	status, err := adapter.inspectStatus(ctx, logical)
	if err != nil || !status.ready(l3Device) {
		return errors.New("L2TP interface is not ready")
	}
	if _, err := adapter.runner.Run(ctx, "/etc/init.d/xl2tpd", "status"); err != nil {
		return errors.New("shared xl2tpd is unavailable")
	}
	return nil
}

func (adapter *L2TPAdapter) Stop(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	logical, l3Device, err := adapter.validateRequest(request, false)
	if err != nil {
		return err
	}
	entry, err := adapter.stopOwnership(request, session, logical, l3Device)
	if err != nil {
		if !errors.Is(err, errL2TPOwnershipAbsent) {
			return errors.New("L2TP ownership verification failed")
		}
		status, statusErr := adapter.inspectStatus(ctx, logical)
		if statusErr != nil {
			return statusErr
		}
		if !status.exists {
			return nil
		}
		return errors.New("L2TP ownership verification failed")
	}
	status, err := adapter.inspectStatus(ctx, logical)
	if err != nil {
		return err
	}
	if status.exists {
		if !status.dynamic || status.protocol != "l2tp" || (status.l3Device != "" && status.l3Device != l3Device) {
			return errors.New("L2TP interface ownership conflicts")
		}
		if _, err := adapter.runner.Run(ctx, "/bin/ubus", "call", "network.interface."+logical, "remove"); err != nil {
			return errors.New("L2TP dynamic interface removal failed")
		}
		if err := adapter.waitRemoved(ctx, logical); err != nil {
			return err
		}
	}
	return adapter.deleteOwnership(entry)
}

func (adapter *L2TPAdapter) validateRequest(request platform.NodeRequest, requireEnabled bool) (string, string, error) {
	if adapter == nil || adapter.runner == nil || !filepath.IsAbs(adapter.manifestPath) || !filepath.IsAbs(adapter.bootIDPath) ||
		adapter.pollInterval <= 0 || adapter.negotiationTimeout <= 0 || adapter.now == nil || !safeOwnershipID.MatchString(request.Node.ID) ||
		request.Node.Protocol != model.ProtocolL2TP || (requireEnabled && !request.Node.Enabled) || request.Node.PolicyID == 0 || request.Node.PolicyID > 60 ||
		request.Node.Port == 0 || request.Node.Revision == 0 || request.Generation == 0 || !validL2TPCredential(request.Node.Username, 256) ||
		!validL2TPCredential(request.Node.Password, 1024) || !validL2TPServer(request.Node.Server) {
		return "", "", errors.New("L2TP request is invalid")
	}
	logical := fmt.Sprintf("ppv2%04d", request.Node.PolicyID)
	l3Device := "l2tp-" + logical
	if !safeInterface.MatchString(logical) || !safeInterface.MatchString(l3Device) {
		return "", "", errors.New("L2TP interface identity is invalid")
	}
	return logical, l3Device, nil
}

func validL2TPCredential(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.ContainsAny(value, "\"\\\r\n") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validL2TPServer(value string) bool {
	if address, err := netip.ParseAddr(value); err == nil {
		return validL2TPEndpoint(address)
	}
	if len(value) == 0 || len(value) > 253 || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func validL2TPEndpoint(address netip.Addr) bool {
	return address.Is4() && address.IsGlobalUnicast() && !address.IsLoopback() && !address.IsLinkLocalUnicast()
}

func (adapter *L2TPAdapter) resolveEndpoint(ctx context.Context, server string) (netip.Addr, error) {
	if address, err := netip.ParseAddr(server); err == nil {
		if validL2TPEndpoint(address) {
			return address, nil
		}
		return netip.Addr{}, errors.New("L2TP endpoint is invalid")
	}
	if adapter.resolver == nil {
		return netip.Addr{}, errors.New("L2TP endpoint resolver is unavailable")
	}
	address, err := adapter.resolver.ResolveIPv4(ctx, server)
	if err != nil || !validL2TPEndpoint(address) {
		if contextErr := ctx.Err(); contextErr != nil {
			return netip.Addr{}, fmt.Errorf("L2TP endpoint resolution failed: %w", contextErr)
		}
		return netip.Addr{}, l2tpFailure("resolve_failed", "L2TP endpoint resolution failed")
	}
	return address, nil
}

type l2tpDynamicConfig struct {
	Name            string `json:"name"`
	Proto           string `json:"proto"`
	Server          string `json:"server"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	IPv6            bool   `json:"ipv6"`
	Keepalive       string `json:"keepalive"`
	MTU             int    `json:"mtu"`
	CheckupInterval int    `json:"checkup_interval"`
	PPPDOptions     string `json:"pppd_options"`
}

type l2tpStatus struct {
	exists, up, pending, available, dynamic bool
	protocol, l3Device                      string
	addresses                               []netip.Addr
	failureCode                             string
}

func (status l2tpStatus) ready(l3Device string) bool {
	return status.exists && status.up && !status.pending && status.available && status.dynamic &&
		status.protocol == "l2tp" && status.l3Device == l3Device && len(status.addresses) > 0
}

func (adapter *L2TPAdapter) inspectStatus(ctx context.Context, logical string) (l2tpStatus, error) {
	object := "network.interface." + logical
	listed, err := adapter.runner.Run(ctx, "/bin/ubus", "-S", "list")
	if err != nil {
		return l2tpStatus{}, l2tpContextError(ctx, "L2TP interface inventory failed")
	}
	found := false
	for _, listedObject := range strings.Split(string(listed), "\n") {
		if listedObject != object {
			continue
		}
		if found {
			return l2tpStatus{}, errors.New("L2TP interface inventory is invalid")
		}
		found = true
	}
	if !found {
		return l2tpStatus{}, nil
	}
	contents, err := adapter.runner.Run(ctx, "/bin/ubus", "call", object, "status")
	if err != nil {
		return l2tpStatus{}, l2tpContextError(ctx, "L2TP interface status failed")
	}
	var document struct {
		Up        bool   `json:"up"`
		Pending   bool   `json:"pending"`
		Available bool   `json:"available"`
		Dynamic   bool   `json:"dynamic"`
		Proto     string `json:"proto"`
		L3Device  string `json:"l3_device"`
		IPv4      []struct {
			Address string `json:"address"`
		} `json:"ipv4-address"`
		Errors []struct {
			Subsystem string `json:"subsystem"`
			Code      string `json:"code"`
		} `json:"errors"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(&document); err != nil || len(document.IPv4) > 8 || len(document.Errors) > 16 {
		return l2tpStatus{}, errors.New("L2TP interface status is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return l2tpStatus{}, errors.New("L2TP interface status is invalid")
	}
	status := l2tpStatus{exists: true, up: document.Up, pending: document.Pending, available: document.Available, dynamic: document.Dynamic, protocol: document.Proto, l3Device: document.L3Device}
	for _, item := range document.Errors {
		if code := classifyL2TPStatusError(item.Code); code != "" {
			status.failureCode = code
			break
		}
	}
	for _, item := range document.IPv4 {
		address, err := netip.ParseAddr(item.Address)
		if err != nil || !address.Is4() || !address.IsGlobalUnicast() {
			return l2tpStatus{}, errors.New("L2TP PPP address is invalid")
		}
		status.addresses = append(status.addresses, address)
	}
	return status, nil
}

func (adapter *L2TPAdapter) waitReady(ctx context.Context, logical, l3Device string) error {
	deadline := time.NewTimer(adapter.negotiationTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(adapter.pollInterval)
	defer poll.Stop()
	for {
		status, err := adapter.inspectStatus(ctx, logical)
		if err != nil {
			return err
		}
		if status.ready(l3Device) {
			if _, err := adapter.runner.Run(ctx, "/etc/init.d/xl2tpd", "status"); err != nil {
				return errors.New("shared xl2tpd is unavailable")
			}
			return nil
		}
		if status.failureCode != "" {
			return l2tpFailure(status.failureCode, "L2TP interface reported a connection failure")
		}
		if failureCode := adapter.inspectPPPFailure(ctx, logical); failureCode != "" {
			return l2tpFailure(failureCode, "L2TP PPP negotiation reported a connection failure")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("L2TP connection deadline exceeded: %w", ctx.Err())
		case <-deadline.C:
			return l2tpFailure("l2tp_server_no_response", "L2TP server did not respond")
		case <-poll.C:
		}
	}
}

func (adapter *L2TPAdapter) inspectPPPFailure(ctx context.Context, logical string) string {
	contents, err := adapter.runner.Run(
		ctx,
		"/usr/bin/head",
		"-c",
		strconv.Itoa(maxL2TPStatusLogBytes+1),
		defaultL2TPStatusDir+"/status."+logical+".log",
	)
	if err != nil || len(contents) == 0 {
		return ""
	}
	return classifyPPPFailure(contents)
}

func classifyPPPFailure(contents []byte) string {
	if len(contents) > maxL2TPStatusLogBytes {
		return "l2tp_negotiation_failed"
	}
	lower := bytes.ToLower(contents)
	for _, marker := range [][]byte{
		[]byte("ms-chap authentication failed"),
		[]byte("chap authentication failed"),
		[]byte("authentication failed"),
	} {
		if bytes.Contains(lower, marker) {
			return "auth_failed"
		}
	}
	for _, marker := range [][]byte{
		[]byte("could not determine remote ip address"),
		[]byte("ipcp: timeout sending config-requests"),
	} {
		if bytes.Contains(lower, marker) {
			return "l2tp_no_address"
		}
	}
	return ""
}

func (adapter *L2TPAdapter) waitRemoved(ctx context.Context, logical string) error {
	for {
		status, err := adapter.inspectStatus(ctx, logical)
		if err != nil {
			return err
		}
		if !status.exists {
			return nil
		}
		if err := waitL2TPPoll(ctx, adapter.pollInterval); err != nil {
			return fmt.Errorf("L2TP stop deadline exceeded: %w", err)
		}
	}
}

func waitL2TPPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func l2tpContextError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	return errors.New(message)
}

func l2tpFailure(code, message string) error {
	return &model.CodeError{Code: code, Message: message}
}

func classifyL2TPStatusError(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "AUTH_FAILED", "PEER_REFUSED", "AUTHENTICATION_FAILED":
		return "auth_failed"
	case "NO_ADDRESS", "IP_CONFIG_FAILED", "ADDRESS_FAILED":
		return "l2tp_no_address"
	case "RESOLVE_FAILED", "DNS_FAILED":
		return "resolve_failed"
	case "XL2TPD_FAILED", "DAEMON_FAILED":
		return "l2tp_daemon_failed"
	case "INTERFACE_FAILED":
		return "l2tp_interface_failed"
	case "CONNECT_FAILED", "NEGOTIATION_FAILED":
		return "l2tp_negotiation_failed"
	default:
		if strings.TrimSpace(value) != "" {
			return "l2tp_negotiation_failed"
		}
		return ""
	}
}

func exactL2TPSession(request platform.NodeRequest, session platform.Session, l3Device string) bool {
	decodedDigest, digestErr := hex.DecodeString(session.OwnershipDigest)
	return session.NodeID == request.Node.ID && session.Generation == request.Generation && session.Protocol == model.ProtocolL2TP &&
		session.Interface == l3Device && session.LocalPort == 0 && digestErr == nil && len(decodedDigest) == sha256.Size
}

func l2tpOwnershipDigest(request platform.NodeRequest, logical string, endpoint netip.Addr) string {
	hash := sha256.New()
	// Config revision is the secret-free identity of the complete node config.
	// Never persist even a fast hash derived from the L2TP password: a copied
	// manifest must not become an offline password oracle.
	_, _ = fmt.Fprintf(hash, "proxypool-l2tp-v1\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%d",
		request.Node.ID, request.Node.PolicyID, request.Node.Revision, request.Generation, logical, endpoint.String(), request.Node.Port)
	return hex.EncodeToString(hash.Sum(nil))
}

type l2tpOwnership struct {
	NodeID           string    `json:"node_id"`
	PolicyID         uint16    `json:"policy_id"`
	ConfigRevision   uint64    `json:"config_revision"`
	Generation       uint64    `json:"generation"`
	LogicalInterface string    `json:"logical_interface"`
	L3Device         string    `json:"l3_device"`
	Endpoint         string    `json:"endpoint"`
	OwnershipDigest  string    `json:"ownership_digest"`
	StartedAt        time.Time `json:"started_at"`
}

type l2tpManifest struct {
	SchemaVersion int             `json:"schema_version"`
	BootID        string          `json:"boot_id"`
	Entries       []l2tpOwnership `json:"entries"`
}

func (adapter *L2TPAdapter) lookupOwnership(want l2tpOwnership) (bool, error) {
	adapter.manifestMu.Lock()
	defer adapter.manifestMu.Unlock()
	manifest, err := adapter.loadManifest()
	if err != nil {
		return false, err
	}
	for _, entry := range manifest.Entries {
		if entry.LogicalInterface != want.LogicalInterface {
			continue
		}
		return sameL2TPOwnership(entry, want), nil
	}
	return false, nil
}

func (adapter *L2TPAdapter) startOwnership(want l2tpOwnership) (l2tpOwnership, error) {
	adapter.manifestMu.Lock()
	defer adapter.manifestMu.Unlock()
	manifest, err := adapter.loadManifest()
	if err != nil {
		return l2tpOwnership{}, err
	}
	for _, entry := range manifest.Entries {
		if entry.LogicalInterface != want.LogicalInterface {
			continue
		}
		if !sameL2TPStartIdentity(entry, want) {
			return l2tpOwnership{}, errors.New("L2TP ownership generation conflicts")
		}
		return entry, nil
	}
	return l2tpOwnership{}, errors.New("L2TP ownership is absent")
}

func (adapter *L2TPAdapter) sessionOwnership(request platform.NodeRequest, session platform.Session, logical, l3Device string) (l2tpOwnership, error) {
	adapter.manifestMu.Lock()
	defer adapter.manifestMu.Unlock()
	manifest, err := adapter.loadManifest()
	if err != nil {
		return l2tpOwnership{}, err
	}
	for _, entry := range manifest.Entries {
		if entry.LogicalInterface != logical {
			continue
		}
		if entry.NodeID != request.Node.ID || entry.PolicyID != request.Node.PolicyID || entry.ConfigRevision != request.Node.Revision ||
			entry.Generation != request.Generation || entry.L3Device != l3Device || entry.OwnershipDigest != session.OwnershipDigest {
			return l2tpOwnership{}, errors.New("L2TP ownership generation conflicts")
		}
		return entry, nil
	}
	return l2tpOwnership{}, errors.New("L2TP ownership is absent")
}

func (adapter *L2TPAdapter) stopOwnership(request platform.NodeRequest, session platform.Session, logical, l3Device string) (l2tpOwnership, error) {
	adapter.manifestMu.Lock()
	defer adapter.manifestMu.Unlock()
	manifest, err := adapter.loadManifest()
	if err != nil {
		return l2tpOwnership{}, err
	}
	for _, entry := range manifest.Entries {
		if entry.LogicalInterface != logical {
			continue
		}
		if entry.NodeID != request.Node.ID || entry.PolicyID != request.Node.PolicyID || entry.Generation != request.Generation || entry.L3Device != l3Device {
			return l2tpOwnership{}, errors.New("L2TP ownership generation conflicts")
		}
		if session != (platform.Session{}) && (!exactL2TPSession(request, session, l3Device) || entry.OwnershipDigest != session.OwnershipDigest) {
			return l2tpOwnership{}, errors.New("L2TP session ownership is stale")
		}
		return entry, nil
	}
	return l2tpOwnership{}, errL2TPOwnershipAbsent
}

func (adapter *L2TPAdapter) reserveDormantOwnership(entry l2tpOwnership) error {
	adapter.manifestMu.Lock()
	defer adapter.manifestMu.Unlock()
	manifest, err := adapter.loadManifest()
	if err != nil {
		return err
	}
	for index, current := range manifest.Entries {
		if current.LogicalInterface != entry.LogicalInterface {
			continue
		}
		if !sameL2TPStartIdentity(current, entry) {
			return errors.New("L2TP ownership generation conflicts")
		}
		if sameL2TPOwnership(current, entry) {
			entry.StartedAt = current.StartedAt
		}
		manifest.Entries[index] = entry
		return adapter.saveManifest(manifest)
	}
	manifest.Entries = append(manifest.Entries, entry)
	return adapter.saveManifest(manifest)
}

func sameL2TPStartIdentity(left, right l2tpOwnership) bool {
	return left.NodeID == right.NodeID && left.PolicyID == right.PolicyID && left.ConfigRevision == right.ConfigRevision &&
		left.Generation == right.Generation && left.LogicalInterface == right.LogicalInterface && left.L3Device == right.L3Device
}

func (adapter *L2TPAdapter) deleteOwnership(want l2tpOwnership) error {
	adapter.manifestMu.Lock()
	defer adapter.manifestMu.Unlock()
	manifest, err := adapter.loadManifest()
	if err != nil {
		return err
	}
	for index, entry := range manifest.Entries {
		if entry.LogicalInterface != want.LogicalInterface {
			continue
		}
		if !sameL2TPOwnership(entry, want) {
			return errors.New("L2TP ownership generation conflicts")
		}
		manifest.Entries = append(manifest.Entries[:index], manifest.Entries[index+1:]...)
		return adapter.saveManifest(manifest)
	}
	return errors.New("L2TP ownership verification failed")
}

func sameL2TPOwnership(left, right l2tpOwnership) bool {
	return left.NodeID == right.NodeID && left.PolicyID == right.PolicyID && left.ConfigRevision == right.ConfigRevision && left.Generation == right.Generation &&
		left.LogicalInterface == right.LogicalInterface && left.L3Device == right.L3Device && left.Endpoint == right.Endpoint &&
		left.OwnershipDigest == right.OwnershipDigest
}

func (adapter *L2TPAdapter) loadManifest() (l2tpManifest, error) {
	bootID, err := readL2TPBootID(adapter.bootIDPath)
	if err != nil {
		return l2tpManifest{}, err
	}
	empty := l2tpManifest{SchemaVersion: l2tpManifestSchema, BootID: bootID, Entries: []l2tpOwnership{}}
	info, err := os.Lstat(adapter.manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxL2TPManifestBytes {
		return l2tpManifest{}, errors.New("L2TP ownership manifest is unsafe")
	}
	contents, err := os.ReadFile(adapter.manifestPath)
	if err != nil {
		return l2tpManifest{}, errors.New("L2TP ownership manifest read failed")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var manifest l2tpManifest
	if err := decoder.Decode(&manifest); err != nil {
		return l2tpManifest{}, errors.New("L2TP ownership manifest is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return l2tpManifest{}, errors.New("L2TP ownership manifest is invalid")
	}
	if manifest.SchemaVersion != l2tpManifestSchema || len(manifest.Entries) > 60 {
		return l2tpManifest{}, errors.New("L2TP ownership manifest is invalid")
	}
	if manifest.BootID != bootID {
		return empty, nil
	}
	seen := make(map[string]struct{}, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		address, addressErr := netip.ParseAddr(entry.Endpoint)
		if !safeOwnershipID.MatchString(entry.NodeID) || entry.PolicyID == 0 || entry.PolicyID > 60 || entry.ConfigRevision == 0 || entry.Generation == 0 ||
			entry.LogicalInterface != fmt.Sprintf("ppv2%04d", entry.PolicyID) || entry.L3Device != "l2tp-"+entry.LogicalInterface ||
			!validL2TPEndpoint(address) || addressErr != nil || len(entry.OwnershipDigest) != sha256.Size*2 || entry.StartedAt.IsZero() {
			return l2tpManifest{}, errors.New("L2TP ownership manifest is invalid")
		}
		if _, duplicate := seen[entry.LogicalInterface]; duplicate {
			return l2tpManifest{}, errors.New("L2TP ownership manifest is invalid")
		}
		seen[entry.LogicalInterface] = struct{}{}
	}
	return manifest, nil
}

func (adapter *L2TPAdapter) saveManifest(manifest l2tpManifest) error {
	sort.Slice(manifest.Entries, func(i, j int) bool {
		return manifest.Entries[i].LogicalInterface < manifest.Entries[j].LogicalInterface
	})
	contents, err := json.Marshal(manifest)
	if err != nil {
		return errors.New("L2TP ownership manifest encode failed")
	}
	directory := filepath.Dir(adapter.manifestPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("L2TP ownership manifest directory failed")
	}
	temporary, err := os.CreateTemp(directory, ".proxypool-l2tp-*")
	if err != nil {
		return errors.New("L2TP ownership manifest create failed")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return errors.New("L2TP ownership manifest permission failed")
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return errors.New("L2TP ownership manifest write failed")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("L2TP ownership manifest write failed")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("L2TP ownership manifest write failed")
	}
	if info, err := os.Lstat(adapter.manifestPath); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("L2TP ownership manifest is unsafe")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("L2TP ownership manifest is unsafe")
	}
	if err := os.Rename(temporaryPath, adapter.manifestPath); err != nil {
		return errors.New("L2TP ownership manifest publish failed")
	}
	if err := os.Chmod(adapter.manifestPath, 0o600); err != nil {
		return errors.New("L2TP ownership manifest permission failed")
	}
	if runtime.GOOS != "windows" {
		directoryHandle, err := os.Open(directory)
		if err != nil {
			return errors.New("L2TP ownership manifest directory sync failed")
		}
		err = directoryHandle.Sync()
		_ = directoryHandle.Close()
		if err != nil {
			return errors.New("L2TP ownership manifest directory sync failed")
		}
	}
	return nil
}

func readL2TPBootID(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 256 {
		return "", errors.New("L2TP boot identity is unavailable")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("L2TP boot identity is unavailable")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" || len(value) > 128 {
		return "", errors.New("L2TP boot identity is invalid")
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' {
			return "", errors.New("L2TP boot identity is invalid")
		}
	}
	return value, nil
}

var _ platform.NodeAdapter = (*L2TPAdapter)(nil)
