package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

func TestL2TPStartUsesSharedNetifdAndReturnsOnlyVerifiedPPP(t *testing.T) {
	runner := newL2TPRunner()
	runner.ready = true
	resolver := &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
	adapter := newTestL2TPAdapter(t, runner, resolver)
	request := validL2TPRequest()

	session, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if session.NodeID != request.Node.ID || session.Generation != request.Generation || session.Protocol != model.ProtocolL2TP ||
		session.Interface != "l2tp-ppv20042" || session.OwnershipDigest == "" {
		t.Fatalf("unexpected session: %#v", session)
	}
	if resolver.host != "vpn.example.com" {
		t.Fatalf("resolved host = %q", resolver.host)
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.adds != 1 {
		t.Fatalf("add_dynamic calls = %d", runner.adds)
	}
	if runner.inputName != "/usr/bin/ucode" || strings.Join(runner.inputArgs, " ") != "/usr/lib/proxypool/ubus-call-stdin.uc network add_dynamic" {
		t.Fatalf("unsafe add invocation: %s %q", runner.inputName, runner.inputArgs)
	}
	var config map[string]any
	if err := json.Unmarshal(runner.input, &config); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name": "ppv20042", "proto": "l2tp", "server": "203.0.113.17:1701",
		"username": "alice", "password": "secret", "ipv6": false,
		"keepalive": "3 5", "mtu": float64(1400), "checkup_interval": float64(5), "pppd_options": "noauth",
	}
	for key, value := range want {
		if config[key] != value {
			t.Fatalf("dynamic config %s = %#v, want %#v (full=%#v)", key, config[key], value, config)
		}
	}
	if _, exists := config["defaultroute"]; exists {
		t.Fatal("invented defaultroute protocol field")
	}
	if _, exists := config["peerdns"]; exists {
		t.Fatal("invented peerdns protocol field")
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "restart") || strings.Contains(call, "disable") || strings.Contains(call, "chap-secrets") || strings.Contains(call, "ppp-up") {
			t.Fatalf("adapter mutated shared/global L2TP state: %q", call)
		}
	}
}

func TestL2TPStartDoesNotTreatAnUnmatchedUbusLookupAsInventoryFailure(t *testing.T) {
	runner := newL2TPRunner()
	runner.ready = true
	// OpenWrt ubus exits with UBUS_STATUS_NOT_FOUND when an exact `list`
	// pattern matches no object.  A first connection must still reach
	// add_dynamic when the interface is correctly absent.
	runner.exactMissingIsNotFound = true
	adapter := newTestL2TPAdapter(t, runner, &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")})

	if _, err := adapter.Start(context.Background(), validL2TPRequest()); err != nil {
		t.Fatalf("absent interface was misclassified as an inventory failure: %v", err)
	}
	if runner.adds != 1 {
		t.Fatalf("add_dynamic calls = %d, want 1", runner.adds)
	}
}

func TestL2TPStartFailsClosedWhenCompleteUbusInventoryFails(t *testing.T) {
	runner := newL2TPRunner()
	runner.ready = true
	runner.listAllErr = errors.New("ubus unavailable")
	adapter := newTestL2TPAdapter(t, runner, &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")})

	if _, err := adapter.Start(context.Background(), validL2TPRequest()); err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Fatalf("ubus inventory failure was accepted: %v", err)
	}
	if runner.adds != 0 {
		t.Fatalf("failed inventory reached add_dynamic %d times", runner.adds)
	}
}

func TestL2TPStartReturnsSafeSpecificFailureCodes(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*l2tpRunner) L2TPEndpointResolver
		code    string
	}{
		{
			name: "endpoint resolution",
			prepare: func(_ *l2tpRunner) L2TPEndpointResolver {
				return &l2tpResolver{err: errors.New("resolver included secret detail")}
			},
			code: "resolve_failed",
		},
		{
			name: "dynamic interface creation",
			prepare: func(runner *l2tpRunner) L2TPEndpointResolver {
				runner.addErr = errors.New("ubus included secret detail")
				return &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
			},
			code: "l2tp_interface_failed",
		},
		{
			name: "PPP authentication",
			prepare: func(runner *l2tpRunner) L2TPEndpointResolver {
				runner.statusError = "AUTH_FAILED"
				return &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
			},
			code: "auth_failed",
		},
		{
			name: "PPP address allocation",
			prepare: func(runner *l2tpRunner) L2TPEndpointResolver {
				runner.statusError = "NO_ADDRESS"
				return &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
			},
			code: "l2tp_no_address",
		},
		{
			name: "shared L2TP daemon",
			prepare: func(runner *l2tpRunner) L2TPEndpointResolver {
				runner.statusError = "XL2TPD_FAILED"
				return &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
			},
			code: "l2tp_daemon_failed",
		},
		{
			name: "unknown negotiation failure",
			prepare: func(runner *l2tpRunner) L2TPEndpointResolver {
				runner.statusError = "VENDOR_NEGOTIATION_ERROR"
				return &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
			},
			code: "l2tp_negotiation_failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := newL2TPRunner()
			adapter := newTestL2TPAdapter(t, runner, test.prepare(runner))
			_, err := adapter.Start(context.Background(), validL2TPRequest())
			var coded *model.CodeError
			if !errors.As(err, &coded) || coded.Code != test.code {
				t.Fatalf("error = %v, want code %q", err, test.code)
			}
			if strings.Contains(err.Error(), "secret detail") {
				t.Fatalf("unsafe internal failure detail escaped: %v", err)
			}
		})
	}
}

func TestL2TPRejectsUnsafeCredentialsAndForeignInterfaceBeforeMutation(t *testing.T) {
	for _, mutate := range []func(*platform.NodeRequest){
		func(request *platform.NodeRequest) { request.Node.Protocol = model.ProtocolSOCKS5 },
		func(request *platform.NodeRequest) { request.Node.Username = "bad\nuser" },
		func(request *platform.NodeRequest) { request.Node.Username = `bad"user` },
		func(request *platform.NodeRequest) { request.Node.Password = `bad\secret` },
		func(request *platform.NodeRequest) { request.Generation = 0 },
		func(request *platform.NodeRequest) { request.Node.PolicyID = 61 },
	} {
		runner := newL2TPRunner()
		adapter := newTestL2TPAdapter(t, runner, &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")})
		request := validL2TPRequest()
		mutate(&request)
		if _, err := adapter.Start(context.Background(), request); err == nil {
			t.Fatal("invalid L2TP request was accepted")
		}
		if runner.adds != 0 {
			t.Fatal("invalid request reached add_dynamic")
		}
	}

	runner := newL2TPRunner()
	runner.exists = true
	runner.ready = true
	adapter := newTestL2TPAdapter(t, runner, &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")})
	if _, err := adapter.Start(context.Background(), validL2TPRequest()); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("foreign interface error = %v", err)
	}
	if runner.adds != 0 || runner.removes != 0 {
		t.Fatal("foreign interface was mutated")
	}
}

func TestL2TPProbeRejectsWrongStatusMissingAddressStaleGenerationAndDeadSharedDaemon(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*l2tpRunner, *platform.Session)
	}{
		{name: "wrong l3 device", mutate: func(r *l2tpRunner, _ *platform.Session) { r.l3Device = "l2tp-foreign" }},
		{name: "missing PPP address", mutate: func(r *l2tpRunner, _ *platform.Session) { r.address = "" }},
		{name: "shared daemon absent", mutate: func(r *l2tpRunner, _ *platform.Session) { r.daemonAlive = false }},
		{name: "stale generation", mutate: func(_ *l2tpRunner, s *platform.Session) { s.Generation-- }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := newL2TPRunner()
			runner.ready = true
			adapter := newTestL2TPAdapter(t, runner, &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")})
			request := validL2TPRequest()
			session, err := adapter.Start(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(runner, &session)
			if err := adapter.Probe(context.Background(), request, session); err == nil {
				t.Fatal("invalid L2TP health was accepted")
			}
		})
	}
}

func TestL2TPTimeoutReturnsOwnedPartialSessionAndStopUsesInterfaceRemove(t *testing.T) {
	runner := newL2TPRunner()
	adapter := newTestL2TPAdapter(t, runner, &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")})
	request := validL2TPRequest()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	session, err := adapter.Start(ctx, request)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || session.Interface != "l2tp-ppv20042" || session.OwnershipDigest == "" {
		t.Fatalf("timeout result = %#v, %v", session, err)
	}
	if err := adapter.Stop(context.Background(), request, session); err != nil {
		t.Fatal(err)
	}
	if runner.removes != 1 {
		t.Fatalf("interface remove calls = %d", runner.removes)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "del_dynamic") || strings.Contains(call, "xl2tpd restart") || strings.Contains(call, "xl2tpd disable") {
			t.Fatalf("unsupported/destructive stop call: %q", call)
		}
	}
}

func TestL2TPStopTimeoutKeepsOwnershipForRetry(t *testing.T) {
	runner := newL2TPRunner()
	runner.ready = true
	adapter := newTestL2TPAdapter(t, runner, &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")})
	request := validL2TPRequest()
	session, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	runner.removeSticks = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := adapter.Stop(ctx, request, session); err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "stop deadline") {
		t.Fatalf("stop timeout error = %v", err)
	}
	owned, err := adapter.lookupOwnership(l2tpOwnership{
		NodeID: request.Node.ID, PolicyID: request.Node.PolicyID, ConfigRevision: request.Node.Revision, Generation: request.Generation,
		LogicalInterface: "ppv20042", L3Device: session.Interface,
		Endpoint: "203.0.113.17", OwnershipDigest: session.OwnershipDigest,
	})
	if err != nil || !owned {
		t.Fatalf("stop timeout discarded retry ownership: owned=%t err=%v", owned, err)
	}
}

func TestL2TPProbeAndStopDoNotDependOnEndpointDNSAfterStart(t *testing.T) {
	runner := newL2TPRunner()
	runner.ready = true
	resolver := &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
	adapter := newTestL2TPAdapter(t, runner, resolver)
	request := validL2TPRequest()
	session, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	resolver.err = errors.New("bootstrap DNS unavailable")
	if err := adapter.Probe(context.Background(), request, session); err != nil {
		t.Fatalf("healthy owned PPP started depending on DNS again: %v", err)
	}
	if err := adapter.Stop(context.Background(), request, session); err != nil {
		t.Fatalf("owned PPP could not be cleaned while DNS was unavailable: %v", err)
	}
	if runner.removes != 1 {
		t.Fatalf("remove calls = %d", runner.removes)
	}
}

func TestL2TPStopUsesDurableOwnershipAfterNodeWasDisabledAndRevisionAdvanced(t *testing.T) {
	runner := newL2TPRunner()
	runner.ready = true
	adapter := newTestL2TPAdapter(t, runner, &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")})
	request := validL2TPRequest()
	if _, err := adapter.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	// The controller persists desired=false before it asks the scheduler to
	// tear down the old generation. A daemon restart can also lose the in-memory
	// Session, so Stop must rely on the root-private ownership manifest.
	request.Node.Enabled = false
	request.Node.Revision++
	if err := adapter.Stop(context.Background(), request, platform.Session{}); err != nil {
		t.Fatalf("owned disabled L2TP interface was stranded: %v", err)
	}
	if runner.removes != 1 {
		t.Fatalf("interface remove calls = %d, want 1", runner.removes)
	}
}

func TestL2TPOwnershipPersistsPrivatelyAndRecoversSameGeneration(t *testing.T) {
	directory := t.TempDir()
	manifest := filepath.Join(directory, "l2tp.json")
	boot := filepath.Join(directory, "boot_id")
	if err := os.WriteFile(boot, []byte("boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newL2TPRunner()
	runner.ready = true
	resolver := &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
	adapter := NewL2TPAdapter(runner, resolver, manifest, WithL2TPBootIDPath(boot), WithL2TPPollInterval(time.Millisecond))
	request := validL2TPRequest()
	first, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(manifest)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("manifest mode/error = %v/%v", info, err)
	}

	restarted := NewL2TPAdapter(runner, resolver, manifest, WithL2TPBootIDPath(boot), WithL2TPPollInterval(time.Millisecond))
	second, err := restarted.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if runner.adds != 1 || first.OwnershipDigest != second.OwnershipDigest {
		t.Fatalf("restart recreated owned interface: adds=%d first=%#v second=%#v", runner.adds, first, second)
	}

	stale := request
	stale.Generation--
	if err := restarted.Stop(context.Background(), stale, first); err == nil {
		t.Fatal("stale generation removed current interface")
	}
	if runner.removes != 0 {
		t.Fatal("stale stop reached netifd")
	}
}

func TestL2TPRecoverySurvivesEndpointDNSRotationWithoutManualReconnect(t *testing.T) {
	runner := newL2TPRunner()
	runner.ready = true
	resolver := &l2tpResolver{address: netip.MustParseAddr("203.0.113.17")}
	adapter := newTestL2TPAdapter(t, runner, resolver)
	request := validL2TPRequest()
	first, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	resolver.address = netip.MustParseAddr("203.0.113.18")
	existing, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("existing owned interface was rejected after DNS rotation: %v", err)
	}
	if existing.OwnershipDigest != first.OwnershipDigest || runner.adds != 1 {
		t.Fatalf("existing ownership was replaced: first=%#v existing=%#v adds=%d", first, existing, runner.adds)
	}

	runner.mu.Lock()
	runner.exists = false
	runner.mu.Unlock()
	recreated, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("absent owned interface did not adopt the new endpoint: %v", err)
	}
	if recreated.OwnershipDigest == first.OwnershipDigest || runner.adds != 2 {
		t.Fatalf("dormant ownership did not rotate endpoint: first=%#v recreated=%#v adds=%d", first, recreated, runner.adds)
	}
}

func validL2TPRequest() platform.NodeRequest {
	return platform.NodeRequest{
		JobID: "job-a", Generation: 9,
		Node: model.Node{ID: "node_a", Name: "Node A", Protocol: model.ProtocolL2TP, Enabled: true,
			Server: "vpn.example.com", Port: 1701, Username: "alice", Password: "secret", PolicyID: 42, Revision: 3},
	}
}

func newTestL2TPAdapter(t *testing.T, runner *l2tpRunner, resolver L2TPEndpointResolver) *L2TPAdapter {
	t.Helper()
	directory := t.TempDir()
	boot := filepath.Join(directory, "boot_id")
	if err := os.WriteFile(boot, []byte("boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewL2TPAdapter(runner, resolver, filepath.Join(directory, "l2tp.json"), WithL2TPBootIDPath(boot), WithL2TPPollInterval(time.Millisecond))
}

type l2tpResolver struct {
	host    string
	address netip.Addr
	err     error
}

func (resolver *l2tpResolver) ResolveIPv4(_ context.Context, host string) (netip.Addr, error) {
	resolver.host = host
	return resolver.address, resolver.err
}

type l2tpRunner struct {
	mu                     sync.Mutex
	exists                 bool
	ready                  bool
	daemonAlive            bool
	l3Device               string
	address                string
	adds                   int
	removes                int
	inputName              string
	inputArgs              []string
	input                  []byte
	calls                  []string
	removeSticks           bool
	exactMissingIsNotFound bool
	listAllErr             error
	addErr                 error
	statusError            string
}

func newL2TPRunner() *l2tpRunner {
	return &l2tpRunner{daemonAlive: true, l3Device: "l2tp-ppv20042", address: "10.64.0.2"}
}

func (runner *l2tpRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, call)
	if name == "/etc/init.d/xl2tpd" && len(args) == 1 && args[0] == "status" {
		if runner.daemonAlive {
			return nil, nil
		}
		return nil, errors.New("not running")
	}
	if name == "/bin/ubus" && len(args) == 2 && args[0] == "-S" && args[1] == "list" {
		if runner.listAllErr != nil {
			return nil, runner.listAllErr
		}
		objects := "system\nnetwork\n"
		if runner.exists {
			objects += "network.interface.ppv20042\n"
		}
		return []byte(objects), nil
	}
	if name == "/bin/ubus" && len(args) == 3 && args[0] == "-S" && args[1] == "list" && args[2] == "network.interface.ppv20042" {
		if runner.exists {
			return []byte("network.interface.ppv20042\n"), nil
		}
		if runner.exactMissingIsNotFound {
			return nil, errors.New("Command failed: Not found")
		}
		return nil, nil
	}
	if name != "/bin/ubus" || len(args) != 3 || args[0] != "call" || args[2] != "status" || args[1] != "network.interface.ppv20042" {
		if name == "/bin/ubus" && len(args) == 3 && args[0] == "call" && args[2] == "remove" && args[1] == "network.interface.ppv20042" {
			runner.removes++
			if !runner.removeSticks {
				runner.exists = false
			}
			return nil, nil
		}
		return nil, errors.New("unexpected command")
	}
	if !runner.exists {
		return nil, errors.New("not found")
	}
	status := map[string]any{"up": runner.ready, "pending": !runner.ready, "available": true, "dynamic": true, "proto": "l2tp"}
	if runner.statusError != "" {
		status["errors"] = []any{map[string]any{"subsystem": "ppp", "code": runner.statusError}}
	}
	if runner.ready {
		status["l3_device"] = runner.l3Device
		addresses := []any{}
		if runner.address != "" {
			addresses = append(addresses, map[string]any{"address": runner.address, "mask": float64(32)})
		}
		status["ipv4-address"] = addresses
	}
	return json.Marshal(status)
}

func (runner *l2tpRunner) RunInput(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls = append(runner.calls, name+" "+strings.Join(args, " "))
	if name != "/usr/bin/ucode" || strings.Join(args, " ") != "/usr/lib/proxypool/ubus-call-stdin.uc network add_dynamic" {
		return nil, errors.New("unexpected input command")
	}
	runner.adds++
	runner.exists = true
	runner.inputName = name
	runner.inputArgs = append([]string(nil), args...)
	runner.input = append([]byte(nil), input...)
	return nil, runner.addErr
}

var _ platform.InputCommandRunner = (*l2tpRunner)(nil)
