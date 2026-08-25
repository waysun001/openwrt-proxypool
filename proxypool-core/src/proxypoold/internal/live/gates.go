// Package live composes the fail-closed session gates used by the V2 daemon.
package live

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"proxypoold/internal/dnsproxy"
	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

const (
	defaultAuthorizationRenewInterval = 8 * time.Second
	authorizationLeaseLifetime        = 20 * time.Second
	dnsChannelAttemptTimeout           = 5 * time.Second
)

type ConfigSource interface {
	Load() (model.DesiredConfig, error)
}

type DNSBindingServer interface {
	SetBindings(map[netip.Addr]dnsproxy.NodeChannel)
}

type DNSChannelFactory func(model.Node, platform.Session, model.DoHEndpoint) (dnsproxy.NodeChannel, error)

type RouteGate struct{ manager platform.RouteManager }

func NewRouteGate(manager platform.RouteManager) *RouteGate { return &RouteGate{manager: manager} }

func (gate *RouteGate) Open(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	lease, err := routeLease(request, session)
	if err != nil || gate == nil || gate.manager == nil {
		return errors.New("live route gate is invalid")
	}
	if err := gate.manager.Install(ctx, lease); err != nil {
		return &model.CodeError{Code: "route_failed", Message: "live route installation failed"}
	}
	if err := gate.manager.Verify(ctx, lease); err != nil {
		_ = gate.manager.Remove(context.Background(), lease)
		return &model.CodeError{Code: "route_failed", Message: "live route verification failed"}
	}
	return nil
}

func (gate *RouteGate) Close(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	lease, err := routeCleanupLease(request, session)
	if err != nil || gate == nil || gate.manager == nil {
		return errors.New("live route gate is invalid")
	}
	return gate.manager.Remove(ctx, lease)
}

func (gate *RouteGate) Verify(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	lease, err := routeLease(request, session)
	if err != nil || gate == nil || gate.manager == nil {
		return errors.New("live route gate is invalid")
	}
	if err := gate.manager.Verify(ctx, lease); err != nil {
		return errors.New("live route verification failed")
	}
	return nil
}

func routeCleanupLease(request platform.NodeRequest, session platform.Session) (platform.RouteLease, error) {
	if session != (platform.Session{}) {
		return routeLease(request, session)
	}
	if request.Node.ID == "" || request.Node.Protocol != model.ProtocolL2TP || request.Node.PolicyID == 0 || request.Node.PolicyID > 60 || request.Generation == 0 {
		return platform.RouteLease{}, errors.New("live route cleanup lease is invalid")
	}
	return platform.RouteLease{
		NodeID: request.Node.ID, PolicyID: request.Node.PolicyID, Generation: request.Generation,
		Interface: fmt.Sprintf("l2tp-ppv2%04d", request.Node.PolicyID),
	}, nil
}

func routeLease(request platform.NodeRequest, session platform.Session) (platform.RouteLease, error) {
	if !exactLiveSession(request, session) || request.Node.Protocol != model.ProtocolL2TP {
		return platform.RouteLease{}, errors.New("live route lease is invalid")
	}
	return platform.RouteLease{NodeID: request.Node.ID, PolicyID: request.Node.PolicyID, Generation: request.Generation, Interface: session.Interface}, nil
}

type dnsGateState struct {
	generation uint64
	channel    dnsproxy.NodeChannel
	addresses  []netip.Addr
}

type DNSGate struct {
	mu       sync.Mutex
	source   ConfigSource
	server   DNSBindingServer
	factory  DNSChannelFactory
	bindings map[string]dnsGateState
}

func NewDNSGate(source ConfigSource, server DNSBindingServer, factory DNSChannelFactory) *DNSGate {
	return &DNSGate{source: source, server: server, factory: factory, bindings: make(map[string]dnsGateState)}
}

func (gate *DNSGate) Open(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	if gate == nil || gate.source == nil || gate.server == nil || gate.factory == nil || !exactLiveSession(request, session) {
		return &model.CodeError{Code: "dns_failed", Message: "live DNS gate is invalid"}
	}
	desired, err := gate.source.Load()
	if err != nil {
		return &model.CodeError{Code: "dns_failed", Message: "live DNS configuration is unavailable"}
	}
	node, devices, err := exactDesiredNode(desired, request)
	if err != nil || len(desired.Global.DoHEndpoints) == 0 {
		return &model.CodeError{Code: "dns_failed", Message: "live DNS configuration is stale"}
	}
	channels := make([]dnsproxy.NodeChannel, 0, len(desired.Global.DoHEndpoints))
	for _, endpoint := range desired.Global.DoHEndpoints {
		channel, err := gate.factory(node, session, endpoint)
		if err != nil || channel == nil {
			continue
		}
		channels = append(channels, channel)
	}
	if len(channels) == 0 {
		return &model.CodeError{Code: "dns_failed", Message: "live DNS channel is unavailable"}
	}
	channel := dnsFailover(channels)
	if _, err := channel.Resolve(ctx, dnsPreflightQuery()); err != nil {
		return &model.CodeError{Code: "dns_failed", Message: "live DNS preflight failed"}
	}
	addresses := make([]netip.Addr, 0, len(devices))
	for _, device := range devices {
		addresses = append(addresses, device.FixedIPv4.Unmap())
	}

	gate.mu.Lock()
	defer gate.mu.Unlock()
	previous, existed := gate.bindings[node.ID]
	gate.bindings[node.ID] = dnsGateState{generation: request.Generation, channel: channel, addresses: addresses}
	bindings, err := gate.renderBindingsLocked()
	if err != nil {
		if existed {
			gate.bindings[node.ID] = previous
		} else {
			delete(gate.bindings, node.ID)
		}
		return err
	}
	gate.server.SetBindings(bindings)
	return nil
}

func (gate *DNSGate) Close(_ context.Context, request platform.NodeRequest, _ platform.Session) error {
	if gate == nil || gate.server == nil || request.Node.ID == "" || request.Generation == 0 {
		return errors.New("live DNS cleanup is invalid")
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	state, exists := gate.bindings[request.Node.ID]
	if exists && state.generation <= request.Generation {
		delete(gate.bindings, request.Node.ID)
	}
	bindings, err := gate.renderBindingsLocked()
	if err != nil {
		return err
	}
	gate.server.SetBindings(bindings)
	return nil
}

func (gate *DNSGate) Verify(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	if gate == nil || gate.source == nil || !exactLiveSession(request, session) {
		return errors.New("live DNS gate is invalid")
	}
	desired, err := gate.source.Load()
	if err != nil {
		return errors.New("live DNS configuration is unavailable")
	}
	if _, _, err := exactDesiredNode(desired, request); err != nil {
		return errors.New("live DNS configuration is stale")
	}
	gate.mu.Lock()
	state, exists := gate.bindings[request.Node.ID]
	gate.mu.Unlock()
	if !exists || state.generation != request.Generation || state.channel == nil {
		return errors.New("live DNS channel is unavailable")
	}
	if _, err := state.channel.Resolve(ctx, dnsPreflightQuery()); err != nil {
		return errors.New("live DNS health check failed")
	}
	return nil
}

func (gate *DNSGate) renderBindingsLocked() (map[netip.Addr]dnsproxy.NodeChannel, error) {
	bindings := make(map[netip.Addr]dnsproxy.NodeChannel)
	for _, state := range gate.bindings {
		for _, address := range state.addresses {
			if !address.Is4() {
				return nil, errors.New("live DNS client address is invalid")
			}
			if _, exists := bindings[address]; exists {
				return nil, errors.New("live DNS client address conflicts")
			}
			bindings[address] = state.channel
		}
	}
	return bindings, nil
}

type dnsFailover []dnsproxy.NodeChannel

func (channels dnsFailover) Resolve(ctx context.Context, query []byte) ([]byte, error) {
	for index, channel := range channels {
		attemptTimeout := dnsChannelAttemptTimeout
		if deadline, ok := ctx.Deadline(); ok {
			fairShare := time.Until(deadline) / time.Duration(len(channels)-index)
			if fairShare < attemptTimeout {
				attemptTimeout = fairShare
			}
		}
		if attemptTimeout <= 0 {
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		response, err := channel.Resolve(attemptCtx, query)
		cancel()
		if err == nil {
			return response, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New("all node DNS channels failed")
}

func dnsPreflightQuery() []byte {
	return []byte{0x50, 0x50, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01}
}

type AuthorizationOption func(*AuthorizationGate)

func WithAuthorizationRenewInterval(interval time.Duration) AuthorizationOption {
	return func(gate *AuthorizationGate) {
		if interval > 0 {
			gate.renewInterval = interval
		}
	}
}

type authorizationGateState struct {
	generation uint64
	cancel     context.CancelFunc
	leases     []platform.AuthorizationLease
	request    platform.NodeRequest
}

type AuthorizationGate struct {
	mu            sync.Mutex
	source        ConfigSource
	authorizer    platform.Authorizer
	renewInterval time.Duration
	now           func() time.Time
	states        map[string]authorizationGateState
}

func NewAuthorizationGate(source ConfigSource, authorizer platform.Authorizer, options ...AuthorizationOption) *AuthorizationGate {
	gate := &AuthorizationGate{
		source: source, authorizer: authorizer, renewInterval: defaultAuthorizationRenewInterval,
		now: time.Now, states: make(map[string]authorizationGateState),
	}
	for _, option := range options {
		if option != nil {
			option(gate)
		}
	}
	return gate
}

func (gate *AuthorizationGate) Open(ctx context.Context, request platform.NodeRequest, session platform.Session) error {
	if gate == nil || gate.source == nil || gate.authorizer == nil || gate.renewInterval <= 0 || gate.now == nil || !exactLiveSession(request, session) {
		return errors.New("live authorization gate is invalid")
	}
	desired, err := gate.source.Load()
	if err != nil {
		return errors.New("live authorization configuration is unavailable")
	}
	node, devices, err := exactDesiredNode(desired, request)
	if err != nil {
		return errors.New("live authorization configuration is stale")
	}
	leases := make([]platform.AuthorizationLease, 0, len(devices))
	for _, device := range devices {
		leases = append(leases, platform.AuthorizationLease{
			NodeID: node.ID, MAC: device.MAC, IPv4: device.FixedIPv4, PolicyID: node.PolicyID,
			Generation: request.Generation, Protocol: node.Protocol, Interface: session.Interface,
			RedirectPort: session.LocalPort,
		})
	}

	gate.mu.Lock()
	defer gate.mu.Unlock()
	if current, exists := gate.states[node.ID]; exists {
		if current.generation > request.Generation {
			return errors.New("live authorization generation is stale")
		}
		current.cancel()
		delete(gate.states, node.ID)
	}
	if err := gate.authorizer.RevokeNode(ctx, node.ID, request.Generation); err != nil {
		return errors.New("live authorization reset failed")
	}
	if err := gate.publishLeases(ctx, leases); err != nil {
		_ = gate.authorizer.RevokeNode(context.Background(), node.ID, request.Generation)
		return err
	}
	keepCtx, cancel := context.WithCancel(context.Background())
	state := authorizationGateState{generation: request.Generation, cancel: cancel, leases: leases, request: request}
	gate.states[node.ID] = state
	go gate.renew(keepCtx, node.ID, state)
	return nil
}

func (gate *AuthorizationGate) Close(ctx context.Context, request platform.NodeRequest, _ platform.Session) error {
	if gate == nil || gate.authorizer == nil || request.Node.ID == "" || request.Generation == 0 {
		return errors.New("live authorization cleanup is invalid")
	}
	gate.mu.Lock()
	if current, exists := gate.states[request.Node.ID]; exists && current.generation <= request.Generation {
		current.cancel()
		delete(gate.states, request.Node.ID)
	}
	gate.mu.Unlock()
	return gate.authorizer.RevokeNode(ctx, request.Node.ID, request.Generation)
}

// StopRenewals prevents any keeper from republishing authorization after the
// daemon has entered its fail-closed shutdown sequence.
func (gate *AuthorizationGate) StopRenewals() {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for nodeID, state := range gate.states {
		state.cancel()
		delete(gate.states, nodeID)
	}
}

func (gate *AuthorizationGate) publishLeases(ctx context.Context, leases []platform.AuthorizationLease) error {
	for _, lease := range leases {
		lease.ExpiresAt = gate.now().UTC().Add(authorizationLeaseLifetime)
		if err := gate.authorizer.Publish(ctx, lease); err != nil {
			return errors.New("live authorization publication failed")
		}
	}
	return nil
}

func (gate *AuthorizationGate) renew(ctx context.Context, nodeID string, state authorizationGateState) {
	ticker := time.NewTicker(gate.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		gate.mu.Lock()
		current, exists := gate.states[nodeID]
		if !exists || current.generation != state.generation {
			gate.mu.Unlock()
			return
		}
		leases := append([]platform.AuthorizationLease(nil), current.leases...)
		gate.mu.Unlock()
		stillDesired, err := gate.authorizationStateStillDesired(current)
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), authorizationLeaseLifetime/2)
			_ = gate.authorizer.RevokeNode(cleanupCtx, nodeID, state.generation)
			cancel()
			continue
		}
		if !stillDesired {
			gate.mu.Lock()
			if latest, exists := gate.states[nodeID]; exists && latest.generation == state.generation {
				latest.cancel()
				delete(gate.states, nodeID)
			}
			gate.mu.Unlock()
			cleanupCtx, cancel := context.WithTimeout(context.Background(), authorizationLeaseLifetime/2)
			_ = gate.authorizer.RevokeNode(cleanupCtx, nodeID, state.generation)
			cancel()
			return
		}
		if err := gate.publishLeases(ctx, leases); err != nil {
			cleanupCtx, cancel := context.WithTimeout(ctx, authorizationLeaseLifetime/2)
			_ = gate.authorizer.RevokeNode(cleanupCtx, nodeID, state.generation)
			cancel()
			continue
		}
	}
}

func (gate *AuthorizationGate) authorizationStateStillDesired(state authorizationGateState) (bool, error) {
	desired, err := gate.source.Load()
	if err != nil {
		return false, errors.New("live authorization configuration is unavailable")
	}
	node, devices, err := exactDesiredNode(desired, state.request)
	if err != nil || len(devices) != len(state.leases) {
		return false, nil
	}
	for index, device := range devices {
		lease := state.leases[index]
		if lease.NodeID != node.ID || lease.MAC != device.MAC || lease.IPv4.Unmap() != device.FixedIPv4.Unmap() ||
			lease.PolicyID != node.PolicyID || lease.Protocol != node.Protocol {
			return false, nil
		}
	}
	return true, nil
}

func exactLiveSession(request platform.NodeRequest, session platform.Session) bool {
	return request.Node.ID != "" && request.Node.PolicyID > 0 && request.Node.Revision > 0 && request.Generation > 0 &&
		session.NodeID == request.Node.ID && session.Generation == request.Generation && session.Protocol == request.Node.Protocol && session.Interface != ""
}

func exactDesiredNode(desired model.DesiredConfig, request platform.NodeRequest) (model.Node, []model.Device, error) {
	node, exists := desired.Nodes[request.Node.ID]
	if !exists || !desired.Global.Enabled || !node.Enabled || node.ID != request.Node.ID || node.Revision != request.Node.Revision ||
		node.PolicyID != request.Node.PolicyID || node.Protocol != request.Node.Protocol {
		return model.Node{}, nil, errors.New("desired node is stale")
	}
	deviceIDs := make([]string, 0, len(desired.Devices))
	for id, device := range desired.Devices {
		if device.Enabled && device.NodeID == node.ID {
			deviceIDs = append(deviceIDs, id)
		}
	}
	sort.Strings(deviceIDs)
	devices := make([]model.Device, 0, len(deviceIDs))
	for _, id := range deviceIDs {
		devices = append(devices, desired.Devices[id])
	}
	return node, devices, nil
}

var _ platform.SessionGate = (*RouteGate)(nil)
var _ platform.SessionGate = (*DNSGate)(nil)
var _ platform.SessionGate = (*AuthorizationGate)(nil)
