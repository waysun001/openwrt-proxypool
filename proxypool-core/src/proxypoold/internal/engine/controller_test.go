package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/config"
	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

func TestControllerBindPersistsConfigJobAndIdempotencyAcrossRestart(t *testing.T) {
	configPath := writeControllerConfig(t, controllerConfig())
	runtimePath := filepath.Join(t.TempDir(), "runtime.json")
	jobs := NewJobStore()
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(runtimePath), NewMachine(nil), jobs,
		WithControllerClock(func() time.Time { return stateTestEpoch }),
		WithControllerJobIDSource(func() string { return "job-bind-1" }),
	)
	if err != nil {
		t.Fatalf("NewController() error = %v", err)
	}
	request := controllerRequest("request-bind-1", "device.bind", `{"device_id":"device_a","node_id":"node_b","expected_revision":3}`)
	first := controller.Handle(context.Background(), request)
	assertControllerSuccess(t, first)
	if !bytes.Contains(first.Result, []byte(`"job_id":"job-bind-1"`)) || !bytes.Contains(first.Result, []byte(`"config_revision":4`)) {
		t.Fatalf("bind result = %s", first.Result)
	}

	stored, err := config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 4 || stored.Devices["device_a"].NodeID != "node_b" || !stored.Devices["device_a"].Enabled {
		t.Fatalf("stored binding = revision %d device %#v", stored.Revision, stored.Devices["device_a"])
	}
	if _, exists := jobs.Get("job-bind-1"); !exists {
		t.Fatal("successful config persistence did not create retained job")
	}
	runtimeSnapshot, err := NewRuntimeStore(runtimePath).Load()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeSnapshot.ConfigRevision != 4 || len(runtimeSnapshot.Idempotency) != 1 {
		t.Fatalf("runtime snapshot = %#v", runtimeSnapshot)
	}

	second := controller.Handle(context.Background(), request)
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("same request was not idempotent:\n first: %#v\nsecond: %#v", first, second)
	}
	stored, _ = config.NewStore(configPath).Load()
	if stored.Revision != 4 || len(jobs.List()) != 1 {
		t.Fatalf("idempotent retry mutated state: revision=%d jobs=%d", stored.Revision, len(jobs.List()))
	}

	conflict := controller.Handle(context.Background(), controllerRequest("request-bind-1", "device.bind", `{"device_id":"device_a","node_id":"node_a","expected_revision":4}`))
	assertControllerError(t, conflict, ErrorCodeDuplicate)

	restartedJobs := NewJobStore()
	restarted, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(runtimePath), NewMachine(nil), restartedJobs,
		WithControllerJobIDSource(func() string { return "must-not-be-used" }),
	)
	if err != nil {
		t.Fatalf("NewController(restart) error = %v", err)
	}
	afterRestart := restarted.Handle(context.Background(), request)
	if !reflect.DeepEqual(afterRestart, first) {
		t.Fatalf("restart lost idempotency:\n got: %#v\nwant: %#v", afterRestart, first)
	}
	if job, exists := restartedJobs.Get("job-bind-1"); !exists || job.ConfigRevision != 4 {
		t.Fatalf("restart lost job: %#v exists=%t", job, exists)
	}
}

func TestControllerStrictWriteSchemasAndRevisionConflictsHaveZeroMutation(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params string
		code   string
	}{
		{name: "unknown field", method: "device.bind", params: `{"device_id":"device_a","node_id":"node_b","expected_revision":3,"future":true}`, code: ErrorCodeInvalidRequest},
		{name: "duplicate field", method: "device.bind", params: `{"device_id":"device_a","node_id":"node_b","node_id":"node_a","expected_revision":3}`, code: ErrorCodeInvalidRequest},
		{name: "missing revision", method: "device.bind", params: `{"device_id":"device_a","node_id":"node_b"}`, code: ErrorCodeInvalidRequest},
		{name: "missing device", method: "device.bind", params: `{"device_id":"missing","node_id":"node_b","expected_revision":3}`, code: ErrorCodeNotFound},
		{name: "missing node", method: "device.bind", params: `{"device_id":"device_a","node_id":"missing","expected_revision":3}`, code: ErrorCodeNotFound},
		{name: "stale revision", method: "device.bind", params: `{"device_id":"device_a","node_id":"node_b","expected_revision":2}`, code: ErrorCodeRevisionConflict},
		{name: "unknown action", method: "node.action", params: `{"node_id":"node_a","action":"explode","expected_revision":3}`, code: ErrorCodeInvalidRequest},
		{name: "unknown method", method: "node.save", params: `{}`, code: "unknown_method"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := controllerConfig()
			configPath := writeControllerConfig(t, cfg)
			runtimePath := filepath.Join(t.TempDir(), "runtime.json")
			jobs := NewJobStore()
			controller, err := NewController(config.NewStore(configPath), NewRuntimeStore(runtimePath), NewMachine(nil), jobs)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			response := controller.Handle(context.Background(), controllerRequest("strict-one", test.method, test.params))
			assertControllerError(t, response, test.code)
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) || len(jobs.List()) != 0 {
				t.Fatal("rejected write mutated config or jobs")
			}
		})
	}
}

func TestControllerUnbindAndNodeActionsUseOneDeviceOneNodeSemantics(t *testing.T) {
	configPath := writeControllerConfig(t, controllerConfig())
	runtimePath := filepath.Join(t.TempDir(), "runtime.json")
	ids := []string{"job-unbind", "job-stop", "job-connect", "job-reconnect"}
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(runtimePath), NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { id := ids[0]; ids = ids[1:]; return id }),
	)
	if err != nil {
		t.Fatal(err)
	}

	unbound := controller.Handle(context.Background(), controllerRequest("unbind-1", "device.unbind", `{"device_id":"device_a","expected_revision":3}`))
	assertControllerSuccess(t, unbound)
	cfg, _ := config.NewStore(configPath).Load()
	device := cfg.Devices["device_a"]
	if device.NodeID != "" || device.Enabled || cfg.Revision != 4 {
		t.Fatalf("unbind result = revision %d device %#v", cfg.Revision, device)
	}

	stopped := controller.Handle(context.Background(), controllerRequest("stop-1", "node.action", `{"node_id":"node_a","action":"stop","expected_revision":4}`))
	assertControllerSuccess(t, stopped)
	cfg, _ = config.NewStore(configPath).Load()
	if cfg.Nodes["node_a"].Enabled || cfg.Revision != 5 {
		t.Fatalf("stop did not persist disabled node: %#v", cfg.Nodes["node_a"])
	}

	connected := controller.Handle(context.Background(), controllerRequest("connect-1", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":5}`))
	assertControllerSuccess(t, connected)
	cfg, _ = config.NewStore(configPath).Load()
	if !cfg.Nodes["node_a"].Enabled || cfg.Revision != 6 {
		t.Fatalf("connect did not persist enabled node: %#v", cfg.Nodes["node_a"])
	}

	reconnected := controller.Handle(context.Background(), controllerRequest("reconnect-1", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":6}`))
	assertControllerSuccess(t, reconnected)
	cfg, _ = config.NewStore(configPath).Load()
	if cfg.Revision != 6 {
		t.Fatalf("reconnect changed desired revision to %d", cfg.Revision)
	}
}

func TestControllerReadMethodsAreStrictBoundedAndCredentialFree(t *testing.T) {
	cfg := controllerConfig()
	node := cfg.Nodes["node_a"]
	node.Username = "secret-user"
	node.Password = "credential-DO-NOT-RETURN"
	cfg.Nodes["node_a"] = node
	configPath := writeControllerConfig(t, cfg)
	runtimePath := filepath.Join(t.TempDir(), "runtime.json")
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(runtimePath), NewMachine(nil), NewJobStore(),
		WithControllerClock(func() time.Time { return stateTestEpoch }),
		WithControllerJobIDSource(func() string { return "job-read-1" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	created := controller.Handle(context.Background(), controllerRequest("action-for-read", "node.action", `{"node_id":"node_a","action":"reconnect","expected_revision":3}`))
	assertControllerSuccess(t, created)

	requests := []api.Request{
		controllerRequest("status", "status.get", `{}`),
		controllerRequest("devices", "device.list", `{}`),
		controllerRequest("job", "job.get", `{"job_id":"job-read-1"}`),
		controllerRequest("jobs", "job.list", `{}`),
		controllerRequest("events", "system.events", `{"after_sequence":0,"limit":20}`),
	}
	for _, request := range requests {
		response := controller.Handle(context.Background(), request)
		assertControllerSuccess(t, response)
		lower := strings.ToLower(string(response.Result))
		for _, forbidden := range []string{"credential-do-not-return", "secret-user", "password", "slp_token", "obfs_key"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s response leaked %q: %s", request.Method, forbidden, response.Result)
			}
		}
	}

	assertControllerError(t, controller.Handle(context.Background(), controllerRequest("bad-status", "status.get", `{"future":1}`)), ErrorCodeInvalidRequest)
	assertControllerError(t, controller.Handle(context.Background(), controllerRequest("bad-job", "job.get", `{"job_id":"missing"}`)), ErrorCodeNotFound)
	assertControllerError(t, controller.Handle(context.Background(), controllerRequest("bad-events", "system.events", `{"after_sequence":0,"limit":1001}`)), ErrorCodeInvalidRequest)
}

func TestControllerDoesNotCreateJobWhenDesiredPersistenceFails(t *testing.T) {
	cfg := controllerConfig()
	desired := &failingDesiredStore{cfg: cfg, replaceErr: errors.New("injected credential=DO-NOT-LEAK")}
	runtimePath := filepath.Join(t.TempDir(), "runtime.json")
	jobs := NewJobStore()
	controller, err := NewController(desired, NewRuntimeStore(runtimePath), NewMachine(nil), jobs)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("write-fails", "device.bind", `{"device_id":"device_a","node_id":"node_b","expected_revision":3}`))
	assertControllerError(t, response, ErrorCodeInternal)
	if len(jobs.List()) != 0 {
		t.Fatal("failed desired persistence created a job")
	}
	if strings.Contains(response.Error.Message, "DO-NOT-LEAK") {
		t.Fatal("persistence error leaked through API")
	}
}

func TestControllerCancelledRequestHasZeroMutation(t *testing.T) {
	configPath := writeControllerConfig(t, controllerConfig())
	jobs := NewJobStore()
	controller, err := NewController(config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), jobs)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(configPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := controller.Handle(ctx, controllerRequest("cancelled", "device.bind", `{"device_id":"device_a","node_id":"node_b","expected_revision":3}`))
	assertControllerError(t, response, "operation_timeout")
	after, _ := os.ReadFile(configPath)
	if !bytes.Equal(after, before) || len(jobs.List()) != 0 {
		t.Fatal("cancelled request mutated controller state")
	}
}

func TestControllerListsAndBindsDiscoveredDeviceWithoutCallerMAC(t *testing.T) {
	cfg := controllerConfig()
	cfg.Devices = map[string]model.Device{}
	configPath := writeControllerConfig(t, cfg)
	discovered := platform.DiscoveredDevice{
		ID: "device_001122334455", MAC: "00:11:22:33:44:55", IPv4: netip.MustParseAddr("192.168.9.10"),
		Hostname: "phone", Ingress: "wlan0", LastSeen: stateTestEpoch, Confirmed: true,
	}
	source := &controllerDeviceSource{devices: []platform.DiscoveredDevice{discovered}}
	leases := &controllerLeaseManager{}
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), NewJobStore(),
		WithDeviceServices(source, leases), WithControllerJobIDSource(func() string { return "job-discovered-bind" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	listed := controller.Handle(context.Background(), controllerRequest("list-discovered", "device.list", `{}`))
	assertControllerSuccess(t, listed)
	if !bytes.Contains(listed.Result, []byte(`"id":"device_001122334455"`)) || !bytes.Contains(listed.Result, []byte(`"ingress":"wlan0"`)) {
		t.Fatalf("device.list result = %s", listed.Result)
	}
	if bytes.Contains(listed.Result, []byte(`"confirmed":false`)) {
		t.Fatalf("confirmed DHCP device was downgraded: %s", listed.Result)
	}

	bound := controller.Handle(context.Background(), controllerRequest("bind-discovered", "device.bind", `{"device_id":"device_001122334455","node_id":"node_b","expected_revision":3}`))
	assertControllerSuccess(t, bound)
	stored, err := config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	device, exists := stored.Devices[discovered.ID]
	if !exists || device.MAC != discovered.MAC || device.FixedIPv4 != discovered.IPv4 || device.NodeID != "node_b" || !device.Enabled {
		t.Fatalf("stored discovered binding = %#v exists=%t", device, exists)
	}
	if len(leases.applied) != 1 || leases.applied[0].MAC != discovered.MAC || leases.applied[0].NodeID != "node_b" {
		t.Fatalf("lease Apply calls = %#v", leases.applied)
	}

	withMAC := controller.Handle(context.Background(), controllerRequest("caller-mac", "device.bind", `{"device_id":"device_001122334455","node_id":"node_a","expected_revision":4,"mac":"00:00:00:00:00:01"}`))
	assertControllerError(t, withMAC, ErrorCodeInvalidRequest)
}

func TestControllerLeaseFailureDoesNotPublishDiscoveredBinding(t *testing.T) {
	cfg := controllerConfig()
	cfg.Devices = map[string]model.Device{}
	configPath := writeControllerConfig(t, cfg)
	discovered := platform.DiscoveredDevice{ID: "device_001122334455", MAC: "00:11:22:33:44:55", IPv4: netip.MustParseAddr("192.168.9.10"), Confirmed: true}
	leases := &controllerLeaseManager{applyErr: errors.New("dnsmasq reload failed")}
	jobs := NewJobStore()
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), jobs,
		WithDeviceServices(&controllerDeviceSource{devices: []platform.DiscoveredDevice{discovered}}, leases),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("lease-fails", "device.bind", `{"device_id":"device_001122334455","node_id":"node_b","expected_revision":3}`))
	assertControllerError(t, response, ErrorCodeInternal)
	stored, _ := config.NewStore(configPath).Load()
	if stored.Revision != 3 || len(stored.Devices) != 0 || len(jobs.List()) != 0 {
		t.Fatalf("failed lease published binding: revision=%d devices=%#v jobs=%#v", stored.Revision, stored.Devices, jobs.List())
	}
}

type controllerDeviceSource struct {
	devices []platform.DiscoveredDevice
	err     error
}

func (source *controllerDeviceSource) List(context.Context) ([]platform.DiscoveredDevice, error) {
	return append([]platform.DiscoveredDevice(nil), source.devices...), source.err
}

type controllerLeaseManager struct {
	applied   []model.Device
	removed   []model.Device
	applyErr  error
	removeErr error
}

func (manager *controllerLeaseManager) Apply(_ context.Context, device model.Device, _ uint64) error {
	manager.applied = append(manager.applied, device)
	return manager.applyErr
}

func (manager *controllerLeaseManager) Remove(_ context.Context, device model.Device, _ uint64) error {
	manager.removed = append(manager.removed, device)
	return manager.removeErr
}

type failingDesiredStore struct {
	cfg        model.DesiredConfig
	replaceErr error
}

func (s *failingDesiredStore) Load() (model.DesiredConfig, error) { return s.cfg, nil }
func (s *failingDesiredStore) Replace(context.Context, uint64, model.DesiredConfig) (model.DesiredConfig, error) {
	return model.DesiredConfig{}, s.replaceErr
}

func controllerConfig() model.DesiredConfig {
	return model.DesiredConfig{
		SchemaVersion: 2,
		Revision:      3,
		Global: model.GlobalConfig{
			Enabled: true, RuntimeBackend: "v2_shadow", MaxNodes: 60, LANDevice: "br-lan",
			ManagementPorts: []uint16{80, 443}, L2TPConcurrency: 4, ProxyConcurrency: 8,
			ConnectTimeout: 30 * time.Second, StopTimeout: 20 * time.Second,
			DoHEndpoints: []model.DoHEndpoint{{URL: "https://dns.example/dns-query", BootstrapIP: "192.0.2.53", ServerName: "dns.example"}},
		},
		Nodes: map[string]model.Node{
			"node_a": {ID: "node_a", Name: "Node A", Protocol: model.ProtocolL2TP, Enabled: true, Server: "a.example", Port: 1701, Username: "user-a", Password: "password-a", PolicyID: 1, Revision: 3},
			"node_b": {ID: "node_b", Name: "Node B", Protocol: model.ProtocolL2TP, Enabled: true, Server: "b.example", Port: 1701, Username: "user-b", Password: "password-b", PolicyID: 2, Revision: 3},
		},
		Devices: map[string]model.Device{
			"device_a": {ID: "device_a", MAC: "00:11:22:33:44:55", Hostname: "Device A", FixedIPv4: netip.MustParseAddr("192.168.9.10"), NodeID: "node_a", Enabled: true},
		},
	}
}

func writeControllerConfig(t *testing.T, cfg model.DesiredConfig) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxypool")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Encode(file, cfg); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func controllerRequest(id, method, params string) api.Request {
	return api.Request{Version: api.ProtocolVersion, ID: id, Method: method, Params: json.RawMessage(params)}
}

func assertControllerSuccess(t *testing.T, response api.Response) {
	t.Helper()
	if response.Error != nil || !json.Valid(response.Result) {
		t.Fatalf("response = version=%d id=%q result=%s error=%+v", response.Version, response.ID, response.Result, response.Error)
	}
}

func assertControllerError(t *testing.T, response api.Response, code string) {
	t.Helper()
	if response.Error == nil || response.Error.Code != code || response.Result != nil {
		t.Fatalf("response = %#v, want error %q", response, code)
	}
}
