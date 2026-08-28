package live

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/dnsproxy"
	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

func TestLiveGatesBuildRouteNodeDNSAndExactDeviceAuthorization(t *testing.T) {
	desired := liveTestConfig()
	source := &liveConfigSource{config: desired}
	routes := &liveRouteManager{}
	bindings := &liveBindingServer{}
	authorizer := &liveAuthorizer{}
	channel := dnsproxy.NodeChannelFunc(func(_ context.Context, query []byte) ([]byte, error) {
		response := append([]byte(nil), query...)
		response[2] |= 0x80
		return response, nil
	})
	factoryCalls := 0
	factory := func(node model.Node, session platform.Session, endpoint model.DoHEndpoint) (dnsproxy.NodeChannel, error) {
		factoryCalls++
		if node.ID != "node_a" || session.Interface != "l2tp-ppv20001" || endpoint.ServerName != "dns.example" {
			t.Fatalf("unexpected DNS factory input: %#v %#v %#v", node, session, endpoint)
		}
		return channel, nil
	}
	routeGate := NewRouteGate(routes)
	dnsGate := NewDNSGate(source, bindings, factory)
	authGate := NewAuthorizationGate(source, authorizer, WithAuthorizationRenewInterval(time.Hour))
	request, session := liveRequestAndSession()

	if err := routeGate.Open(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	if err := dnsGate.Open(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	if err := authGate.Open(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	if routes.installs != 1 || routes.verifies != 1 || factoryCalls != 1 {
		t.Fatalf("route/DNS calls = install %d verify %d factory %d", routes.installs, routes.verifies, factoryCalls)
	}
	bindings.mu.Lock()
	_, dnsBound := bindings.bindings[netip.MustParseAddr("192.168.9.10")]
	_, disabledBound := bindings.bindings[netip.MustParseAddr("192.168.9.11")]
	bindings.mu.Unlock()
	if !dnsBound || disabledBound {
		t.Fatalf("unexpected DNS bindings: active=%t disabled=%t", dnsBound, disabledBound)
	}
	authorizer.mu.Lock()
	if len(authorizer.published) != 1 {
		authorizer.mu.Unlock()
		t.Fatalf("authorization leases = %#v", authorizer.published)
	}
	lease := authorizer.published[0]
	authorizer.mu.Unlock()
	if lease.MAC != "00:11:22:33:44:55" || lease.IPv4.String() != "192.168.9.10" || lease.PolicyID != 1 || lease.Protocol != model.ProtocolL2TP || lease.Interface != session.Interface || lease.RedirectPort != 0 {
		t.Fatalf("authorization lease = %#v", lease)
	}

	if err := authGate.Close(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	if err := dnsGate.Close(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	if err := routeGate.Close(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	if routes.removes != 1 || authorizer.revokes < 2 {
		t.Fatalf("cleanup calls route=%d auth=%d", routes.removes, authorizer.revokes)
	}
	bindings.mu.Lock()
	remaining := len(bindings.bindings)
	bindings.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("DNS bindings remained after close: %d", remaining)
	}
}

func TestRouteGateCleanupDerivesOwnedL2TPInterfaceAfterRestart(t *testing.T) {
	routes := &liveRouteManager{}
	request, _ := liveRequestAndSession()
	if err := NewRouteGate(routes).Close(context.Background(), request, platform.Session{}); err != nil {
		t.Fatal(err)
	}
	if routes.removes != 1 || routes.lastRemoved.Interface != "l2tp-ppv20001" {
		t.Fatalf("cleanup lease = %#v (removes %d)", routes.lastRemoved, routes.removes)
	}
}

func TestRouteGateIsAValidatedNoOpForSOCKS5(t *testing.T) {
	routes := &liveRouteManager{}
	request := platform.NodeRequest{
		Node:  model.Node{ID: "node_proxy", Protocol: model.ProtocolSOCKS5, Enabled: true, PolicyID: 2, Revision: 4},
		JobID: "job_proxy", Generation: 5,
	}
	session := platform.Session{
		NodeID: "node_proxy", Protocol: model.ProtocolSOCKS5, Generation: 5,
		Interface: "psx0002", LocalPort: 12002, RemoteAddress: "203.0.113.8:1080", OwnershipDigest: "owned",
	}
	gate := NewRouteGate(routes)
	if err := gate.Open(context.Background(), request, session); err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := gate.Verify(context.Background(), request, session); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if err := gate.Close(context.Background(), request, session); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if routes.installs != 0 || routes.verifies != 0 || routes.removes != 0 {
		t.Fatalf("SOCKS5 route calls = install %d verify %d remove %d", routes.installs, routes.verifies, routes.removes)
	}
}

func TestRouteGateReportsAStableRouteFailureCode(t *testing.T) {
	routes := &liveRouteManager{installErr: errors.New("raw route detail")}
	request, session := liveRequestAndSession()
	err := NewRouteGate(routes).Open(context.Background(), request, session)
	var coded *model.CodeError
	if !errors.As(err, &coded) || coded.Code != "route_failed" {
		t.Fatalf("error = %v, want route_failed", err)
	}
}

func TestDNSGateFailsClosedBeforePublishingBinding(t *testing.T) {
	source := &liveConfigSource{config: liveTestConfig()}
	bindings := &liveBindingServer{}
	factory := func(model.Node, platform.Session, model.DoHEndpoint) (dnsproxy.NodeChannel, error) {
		return dnsproxy.NodeChannelFunc(func(context.Context, []byte) ([]byte, error) {
			return nil, errors.New("node DNS unavailable")
		}), nil
	}
	request, session := liveRequestAndSession()
	err := NewDNSGate(source, bindings, factory).Open(context.Background(), request, session)
	var coded *model.CodeError
	if !errors.As(err, &coded) || coded.Code != "dns_failed" {
		t.Fatalf("error = %v, want dns_failed", err)
	}
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	if len(bindings.bindings) != 0 {
		t.Fatalf("failed DNS preflight published bindings: %#v", bindings.bindings)
	}
}

func TestDNSFailoverBoundsAStalledPrimaryAndUsesTheBackup(t *testing.T) {
	query := dnsPreflightQuery()
	primary := dnsproxy.NodeChannelFunc(func(ctx context.Context, _ []byte) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	backupCalls := 0
	backup := dnsproxy.NodeChannelFunc(func(_ context.Context, got []byte) ([]byte, error) {
		backupCalls++
		response := append([]byte(nil), got...)
		response[2] |= 0x80
		return response, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	response, err := (dnsFailover{primary, backup}).Resolve(ctx, query)
	if err != nil {
		t.Fatalf("DNS failover did not reach the backup: %v", err)
	}
	if backupCalls != 1 || len(response) != len(query) || response[2]&0x80 == 0 {
		t.Fatalf("backup result = calls %d response %x", backupCalls, response)
	}
}

func TestAuthorizationGateRenewsLeaseAndRevokesAfterFailureOrClose(t *testing.T) {
	source := &liveConfigSource{config: liveTestConfig()}
	authorizer := &liveAuthorizer{}
	gate := NewAuthorizationGate(source, authorizer, WithAuthorizationRenewInterval(2*time.Millisecond))
	request, session := liveRequestAndSession()
	if err := gate.Open(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		authorizer.mu.Lock()
		count := len(authorizer.published)
		authorizer.mu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := gate.Close(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	if len(authorizer.published) < 2 || authorizer.revokes < 2 {
		t.Fatalf("lease was not renewed/revoked: publish=%d revoke=%d", len(authorizer.published), authorizer.revokes)
	}
}

func TestAuthorizationGateRetriesAfterTransientRenewalFailure(t *testing.T) {
	source := &liveConfigSource{config: liveTestConfig()}
	authorizer := &liveAuthorizer{}
	gate := NewAuthorizationGate(source, authorizer, WithAuthorizationRenewInterval(2*time.Millisecond))
	request, session := liveRequestAndSession()
	if err := gate.Open(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	authorizer.mu.Lock()
	authorizer.failPublishes = 1
	authorizer.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		authorizer.mu.Lock()
		published, attempts := len(authorizer.published), authorizer.publishAttempts
		authorizer.mu.Unlock()
		if published >= 2 && attempts >= 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := gate.Close(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	if len(authorizer.published) < 2 || authorizer.publishAttempts < 3 || authorizer.revokes < 3 {
		t.Fatalf("renewal did not fail closed and recover: published=%d attempts=%d revokes=%d", len(authorizer.published), authorizer.publishAttempts, authorizer.revokes)
	}
}

func TestAuthorizationGateStopsRenewingWhenDesiredDevicesChange(t *testing.T) {
	source := &liveConfigSource{config: liveTestConfig()}
	authorizer := &liveAuthorizer{}
	gate := NewAuthorizationGate(source, authorizer, WithAuthorizationRenewInterval(2*time.Millisecond))
	request, session := liveRequestAndSession()
	if err := gate.Open(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	desired := liveTestConfig()
	device := desired.Devices["device_a"]
	device.Enabled = false
	desired.Devices["device_a"] = device
	source.Set(desired)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		authorizer.mu.Lock()
		revokes := authorizer.revokes
		authorizer.mu.Unlock()
		if revokes >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	authorizer.mu.Lock()
	before := len(authorizer.published)
	revokes := authorizer.revokes
	authorizer.mu.Unlock()
	if revokes < 2 {
		t.Fatal("stale authorization was not revoked")
	}
	time.Sleep(10 * time.Millisecond)
	authorizer.mu.Lock()
	after := len(authorizer.published)
	authorizer.mu.Unlock()
	if after != before {
		t.Fatalf("stale authorization kept renewing: %d -> %d", before, after)
	}
}

func liveRequestAndSession() (platform.NodeRequest, platform.Session) {
	node := liveTestConfig().Nodes["node_a"]
	request := platform.NodeRequest{Node: node, JobID: "job-a", Generation: 7}
	session := platform.Session{NodeID: node.ID, Generation: 7, Protocol: node.Protocol, Interface: "l2tp-ppv20001", OwnershipDigest: "owned"}
	return request, session
}

func liveTestConfig() model.DesiredConfig {
	return model.DesiredConfig{
		SchemaVersion: 2, Revision: 3,
		Global: model.GlobalConfig{Enabled: true, DoHEndpoints: []model.DoHEndpoint{{URL: "https://dns.example/dns-query", BootstrapIP: "192.0.2.53", ServerName: "dns.example"}}},
		Nodes: map[string]model.Node{
			"node_a": {ID: "node_a", Name: "A", Protocol: model.ProtocolL2TP, Enabled: true, Server: "vpn.example", Port: 1701, Username: "user", Password: "secret", PolicyID: 1, Revision: 3},
		},
		Devices: map[string]model.Device{
			"device_a": {ID: "device_a", MAC: "00:11:22:33:44:55", FixedIPv4: netip.MustParseAddr("192.168.9.10"), NodeID: "node_a", Enabled: true},
			"device_b": {ID: "device_b", MAC: "00:11:22:33:44:66", FixedIPv4: netip.MustParseAddr("192.168.9.11"), NodeID: "node_a", Enabled: false},
		},
	}
}

type liveConfigSource struct {
	mu     sync.Mutex
	config model.DesiredConfig
}

func (source *liveConfigSource) Load() (model.DesiredConfig, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.config, nil
}

func (source *liveConfigSource) Set(config model.DesiredConfig) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.config = config
}

type liveBindingServer struct {
	mu       sync.Mutex
	bindings map[netip.Addr]dnsproxy.NodeChannel
}

func (server *liveBindingServer) SetBindings(bindings map[netip.Addr]dnsproxy.NodeChannel) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.bindings = make(map[netip.Addr]dnsproxy.NodeChannel, len(bindings))
	for address, channel := range bindings {
		server.bindings[address] = channel
	}
}

type liveRouteManager struct {
	installs, verifies, removes int
	lastRemoved                 platform.RouteLease
	installErr                  error
}

func (manager *liveRouteManager) Install(context.Context, platform.RouteLease) error {
	manager.installs++
	return manager.installErr
}
func (manager *liveRouteManager) Verify(context.Context, platform.RouteLease) error {
	manager.verifies++
	return nil
}
func (manager *liveRouteManager) Remove(_ context.Context, lease platform.RouteLease) error {
	manager.removes++
	manager.lastRemoved = lease
	return nil
}

type liveAuthorizer struct {
	mu              sync.Mutex
	published       []platform.AuthorizationLease
	revokes         int
	publishAttempts int
	failPublishes   int
}

func (authorizer *liveAuthorizer) Publish(_ context.Context, lease platform.AuthorizationLease) error {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.publishAttempts++
	if authorizer.failPublishes > 0 {
		authorizer.failPublishes--
		return errors.New("transient nft failure")
	}
	authorizer.published = append(authorizer.published, lease)
	return nil
}
func (authorizer *liveAuthorizer) RevokeNode(context.Context, string, uint64) error {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.revokes++
	return nil
}
func (authorizer *liveAuthorizer) RevokeAll(context.Context) error { return nil }
