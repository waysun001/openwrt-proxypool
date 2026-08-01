package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/config"
	"proxypoold/internal/importer"
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
		{name: "import preview unknown field", method: "import.preview", params: `{"protocol":"l2tp","raw":"vpn.example|user|password","expected_revision":3,"future":true}`, code: ErrorCodeInvalidRequest},
		{name: "import commit missing revision", method: "import.commit", params: `{"preview_id":"preview","preview_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, code: ErrorCodeInvalidRequest},
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

func TestControllerBulkImportPreviewCommitIsOneAtomicJob(t *testing.T) {
	cfg := controllerConfig()
	cfg.Nodes = map[string]model.Node{}
	cfg.Devices = map[string]model.Device{}
	desired := &memoryDesiredStore{cfg: cfg}
	imports := importer.New(importer.WithIDSource(func() string { return "preview-forty" }))
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithImporter(imports), WithControllerJobIDSource(func() string { return "job-import-forty" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 40)
	for index := range lines {
		lines[index] = fmt.Sprintf("vpn-%02d.example|user-%02d|password-%02d", index, index, index)
	}
	raw := strings.Join(lines, "\n")
	previewParams, _ := json.Marshal(map[string]any{"protocol": "l2tp", "raw": raw, "expected_revision": 3})
	previewResponse := controller.Handle(context.Background(), controllerRequest("preview-forty", "import.preview", string(previewParams)))
	assertControllerSuccess(t, previewResponse)
	var preview importer.Preview
	if err := json.Unmarshal(previewResponse.Result, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Added != 40 || preview.Blocked || strings.Contains(string(previewResponse.Result), "password-") {
		t.Fatalf("preview = %s", previewResponse.Result)
	}
	commitParams, _ := json.Marshal(map[string]any{"preview_id": preview.ID, "preview_hash": preview.Hash, "expected_revision": 3})
	commitResponse := controller.Handle(context.Background(), controllerRequest("commit-forty", "import.commit", string(commitParams)))
	assertControllerSuccess(t, commitResponse)
	stored, _ := desired.Load()
	job, exists := controller.jobs.Get("job-import-forty")
	desired.mu.Lock()
	replaceCalls := desired.replaceCount
	desired.mu.Unlock()
	if stored.Revision != 4 || len(stored.Nodes) != 40 || replaceCalls != 1 || !exists || job.Kind != "import.commit" || job.Total != 40 {
		t.Fatalf("bulk commit = revision %d nodes %d replaces %d job %#v exists=%t", stored.Revision, len(stored.Nodes), replaceCalls, job, exists)
	}
}

func TestControllerBlockedImportDoesNotMutateDesiredOrCreateJob(t *testing.T) {
	desired := &memoryDesiredStore{cfg: controllerConfig()}
	imports := importer.New(importer.WithIDSource(func() string { return "preview-blocked" }))
	controller, err := NewController(desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(), WithImporter(imports))
	if err != nil {
		t.Fatal(err)
	}
	previewResponse := controller.Handle(context.Background(), controllerRequest("preview-blocked", "import.preview", `{"protocol":"l2tp","raw":"invalid","expected_revision":3}`))
	assertControllerSuccess(t, previewResponse)
	var preview importer.Preview
	if err := json.Unmarshal(previewResponse.Result, &preview); err != nil {
		t.Fatal(err)
	}
	commitParams, _ := json.Marshal(map[string]any{"preview_id": preview.ID, "preview_hash": preview.Hash, "expected_revision": 3})
	commitResponse := controller.Handle(context.Background(), controllerRequest("commit-blocked", "import.commit", string(commitParams)))
	assertControllerError(t, commitResponse, ErrorCodeInvalidConfig)
	stored, _ := desired.Load()
	if stored.Revision != 3 || desired.replaceCount != 0 || len(controller.jobs.List()) != 0 {
		t.Fatalf("blocked import mutated state: revision=%d replaces=%d jobs=%#v", stored.Revision, desired.replaceCount, controller.jobs.List())
	}
}

func TestControllerFailedImportPersistenceKeepsPreviewRetryable(t *testing.T) {
	desired := &failOnceDesiredStore{cfg: controllerConfig()}
	imports := importer.New(importer.WithIDSource(func() string { return "preview-retry" }))
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithImporter(imports), WithControllerJobIDSource(func() string { return "job-import-retry" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	previewResponse := controller.Handle(context.Background(), controllerRequest("preview-retry", "import.preview", `{"protocol":"l2tp","raw":"vpn-retry.example|user|password","expected_revision":3}`))
	assertControllerSuccess(t, previewResponse)
	var preview importer.Preview
	if err := json.Unmarshal(previewResponse.Result, &preview); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"preview_id": preview.ID, "preview_hash": preview.Hash, "expected_revision": 3})
	first := controller.Handle(context.Background(), controllerRequest("commit-retry-first", "import.commit", string(params)))
	assertControllerError(t, first, ErrorCodeInternal)
	stored, _ := desired.Load()
	if stored.Revision != 3 || len(controller.jobs.List()) != 0 {
		t.Fatalf("failed first commit mutated state: revision=%d jobs=%#v", stored.Revision, controller.jobs.List())
	}
	second := controller.Handle(context.Background(), controllerRequest("commit-retry-second", "import.commit", string(params)))
	assertControllerSuccess(t, second)
	stored, _ = desired.Load()
	if stored.Revision != 4 || len(stored.Nodes) != 3 || len(controller.jobs.List()) != 1 {
		t.Fatalf("retry commit = revision %d nodes %d jobs %#v", stored.Revision, len(stored.Nodes), controller.jobs.List())
	}
}

func TestControllerImportDoesNotPromiseJobWhenRuntimePersistenceFails(t *testing.T) {
	cfg := controllerConfig()
	cfg.Nodes = map[string]model.Node{}
	cfg.Devices = map[string]model.Device{}
	desired := &memoryDesiredStore{cfg: cfg}
	runtime := &memoryRuntimePersistence{failSaveAt: 2}
	recorder := &controllerSchedulerRecorder{}
	imports := importer.New(importer.WithIDSource(func() string { return "preview-runtime-failure" }))
	controller, err := NewController(
		desired, runtime, NewMachine(nil), NewJobStore(),
		WithImporter(imports), WithControllerJobIDSource(func() string { return "job-runtime-failure-import" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.AttachScheduler(recorder)
	previewResponse := controller.Handle(context.Background(), controllerRequest(
		"preview-runtime-failure", "import.preview", `{"protocol":"l2tp","raw":"vpn.example|user|password","expected_revision":3}`,
	))
	assertControllerSuccess(t, previewResponse)
	var preview importer.Preview
	if err := json.Unmarshal(previewResponse.Result, &preview); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"preview_id": preview.ID, "preview_hash": preview.Hash, "expected_revision": 3})
	request := controllerRequest("commit-runtime-failure", "import.commit", string(params))
	response := controller.Handle(context.Background(), request)
	assertControllerError(t, response, ErrorCodeInternal)
	stored, err := desired.Load()
	if err != nil {
		t.Fatal(err)
	}
	job, exists := controller.jobs.Get("job-runtime-failure-import")
	if stored.Revision != 4 || !exists || len(recorder.jobs) != 1 || recorder.jobs[0].ID != job.ID {
		t.Fatalf("fail-closed import = revision %d job %#v exists=%t submitted=%#v", stored.Revision, job, exists, recorder.jobs)
	}
	replayed := controller.Handle(context.Background(), request)
	assertControllerError(t, replayed, ErrorCodeRevisionConflict)
}

func TestControllerImportPostRenameFailureCannotReplaySuccessAfterRestart(t *testing.T) {
	cfg := controllerConfig()
	cfg.Nodes = map[string]model.Node{}
	cfg.Devices = map[string]model.Device{}
	desired := &memoryDesiredStore{cfg: cfg}
	path := filepath.Join(t.TempDir(), "runtime.json")
	ops := &recordingRuntimeFS{runtimeFS: osRuntimeFS{}, failSyncDirAt: 2}
	imports := importer.New(importer.WithIDSource(func() string { return "preview-post-rename" }))
	controller, err := NewController(
		desired, newRuntimeStore(path, ops), NewMachine(nil), NewJobStore(),
		WithImporter(imports), WithControllerJobIDSource(func() string { return "job-post-rename-import" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	previewResponse := controller.Handle(context.Background(), controllerRequest(
		"preview-post-rename", "import.preview", `{"protocol":"l2tp","raw":"vpn.example|user|password","expected_revision":3}`,
	))
	assertControllerSuccess(t, previewResponse)
	var preview importer.Preview
	if err := json.Unmarshal(previewResponse.Result, &preview); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"preview_id": preview.ID, "preview_hash": preview.Hash, "expected_revision": 3})
	request := controllerRequest("commit-post-rename", "import.commit", string(params))
	assertControllerError(t, controller.Handle(context.Background(), request), ErrorCodeInternal)

	restarted, err := NewController(desired, NewRuntimeStore(path), NewMachine(nil), NewJobStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := restarted.jobs.Get("job-post-rename-import"); !exists {
		t.Fatal("post-rename snapshot did not retain the fail-closed import job")
	}
	assertControllerError(t, restarted.Handle(context.Background(), request), ErrorCodeRevisionConflict)
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

func TestControllerNetifdHintQueuesOwnedNodeRecoveryAndRejectsSpoofing(t *testing.T) {
	controller, err := NewController(
		&memoryDesiredStore{cfg: controllerConfig()}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-netifd-recover" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	valid := controller.Handle(context.Background(), controllerRequest("netifd", "system.interface_event", `{"interface":"ppv20001","action":"ifdown"}`))
	assertControllerSuccess(t, valid)
	if !bytes.Contains(valid.Result, []byte(`"job_id":"job-netifd-recover"`)) {
		t.Fatalf("netifd result = %s", valid.Result)
	}
	job, exists := controller.jobs.Get("job-netifd-recover")
	if !exists || job.Kind != "system.recover" || len(job.Nodes) != 1 || job.Nodes[0].NodeID != "node_a" {
		t.Fatalf("netifd recovery job = %#v exists=%t", job, exists)
	}

	for index, params := range []string{
		`{"interface":"ppv20001\"},\"action\":\"ifup","action":"ifup"}`,
		`{"interface":"ppv29999","action":"ifup"}`,
		`{"interface":"ppv20001","action":"invented"}`,
		`{"interface":"ppv20001","action":"ifup","future":true}`,
	} {
		response := controller.Handle(context.Background(), controllerRequest("bad-netifd", "system.interface_event", params))
		assertControllerError(t, response, ErrorCodeInvalidRequest)
		if len(controller.jobs.List()) != 1 {
			t.Fatalf("invalid hint %d queued work", index)
		}
	}
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

func TestControllerUnbindRemovesStaticLeaseBeforePublishingDesiredChange(t *testing.T) {
	cfg := controllerConfig()
	configPath := writeControllerConfig(t, cfg)
	leases := &controllerLeaseManager{}
	jobs := NewJobStore()
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), jobs,
		WithDeviceServices(&controllerDeviceSource{}, leases), WithControllerJobIDSource(func() string { return "job-unbind-lease" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("unbind-lease", "device.unbind", `{"device_id":"device_a","expected_revision":3}`))
	assertControllerSuccess(t, response)
	stored, err := config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases.removed) != 1 || leases.removed[0].ID != "device_a" || stored.Devices["device_a"].Enabled || len(jobs.List()) != 1 {
		t.Fatalf("unbind cleanup = removed %#v device %#v jobs %#v", leases.removed, stored.Devices["device_a"], jobs.List())
	}
}

func TestControllerUnbindLeaseFailurePreservesDesiredBinding(t *testing.T) {
	cfg := controllerConfig()
	configPath := writeControllerConfig(t, cfg)
	leases := &controllerLeaseManager{removeErr: errors.New("dnsmasq reload failed")}
	jobs := NewJobStore()
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), jobs,
		WithDeviceServices(&controllerDeviceSource{}, leases),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("unbind-lease-fails", "device.unbind", `{"device_id":"device_a","expected_revision":3}`))
	assertControllerError(t, response, ErrorCodeInternal)
	stored, _ := config.NewStore(configPath).Load()
	if stored.Revision != 3 || !stored.Devices["device_a"].Enabled || stored.Devices["device_a"].NodeID != "node_a" || len(jobs.List()) != 0 {
		t.Fatalf("failed lease removal changed desired state: revision=%d device=%#v jobs=%#v", stored.Revision, stored.Devices["device_a"], jobs.List())
	}
}

func TestControllerUnbindRollbackIsBoundedWhenPersistenceAndLeaseRestoreFail(t *testing.T) {
	cfg := controllerConfig()
	desired := &failingDesiredStore{cfg: cfg, replaceErr: errors.New("storage unavailable")}
	leases := &controllerLeaseManager{applyWaitForContext: true}
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithDeviceServices(&controllerDeviceSource{}, leases),
		WithControllerLeaseRollbackTimeout(20*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response := controller.Handle(context.Background(), controllerRequest("bounded-unbind-rollback", "device.unbind", `{"device_id":"device_a","expected_revision":3}`))
	assertControllerError(t, response, ErrorCodeInternal)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("unbind rollback took %s", elapsed)
	}
	if len(leases.removed) != 1 || len(leases.applied) != 1 {
		t.Fatalf("unbind rollback calls = removed %#v applied %#v", leases.removed, leases.applied)
	}
	if desired.cfg.Revision != 3 || !desired.cfg.Devices["device_a"].Enabled {
		t.Fatalf("failed rollback mutated desired state: %#v", desired.cfg)
	}
}

func TestControllerUnbindConvergesToUnboundWhenLeaseRollbackFails(t *testing.T) {
	desired := &failOnceDesiredStore{cfg: controllerConfig()}
	leases := &controllerLeaseManager{applyErr: errors.New("lease restore failed")}
	jobs := NewJobStore()
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), jobs,
		WithDeviceServices(&controllerDeviceSource{}, leases),
		WithControllerLeaseRollbackTimeout(50*time.Millisecond),
		WithControllerJobIDSource(func() string { return "job-repaired-unbind" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("repaired-unbind", "device.unbind", `{"device_id":"device_a","expected_revision":3}`))
	assertControllerSuccess(t, response)
	stored, err := desired.Load()
	if err != nil {
		t.Fatal(err)
	}
	device := stored.Devices["device_a"]
	if stored.Revision != 4 || device.Enabled || device.NodeID != "" || len(jobs.List()) != 1 {
		t.Fatalf("repaired unbind = revision %d device %#v jobs %#v", stored.Revision, device, jobs.List())
	}
}

func TestControllerUnbindRecognizesDesiredStateInstalledBeforePersistenceError(t *testing.T) {
	desired := &postCommitErrorDesiredStore{cfg: controllerConfig()}
	leases := &controllerLeaseManager{}
	jobs := NewJobStore()
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), jobs,
		WithDeviceServices(&controllerDeviceSource{}, leases),
		WithControllerJobIDSource(func() string { return "job-post-commit-unbind" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("post-commit-unbind", "device.unbind", `{"device_id":"device_a","expected_revision":3}`))
	assertControllerSuccess(t, response)
	stored, err := desired.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 4 || stored.Devices["device_a"].Enabled || stored.Devices["device_a"].NodeID != "" {
		t.Fatalf("post-commit desired state = %#v", stored)
	}
	if len(leases.applied) != 0 || len(leases.removed) != 1 || len(jobs.List()) != 1 {
		t.Fatalf("post-commit recovery = applied %#v removed %#v jobs %#v", leases.applied, leases.removed, jobs.List())
	}
}

func TestControllerUnbindQueuesFailClosedCleanupWhenDesiredDurabilityCannotBeConfirmed(t *testing.T) {
	desired := &postCommitErrorDesiredStore{cfg: controllerConfig(), ensureErr: errors.New("directory sync still unavailable")}
	runtime := &memoryRuntimePersistence{}
	recorder := &controllerSchedulerRecorder{}
	controller, err := NewController(desired, runtime, NewMachine(nil), NewJobStore(), WithControllerJobIDSource(func() string { return "job-uncertain-unbind" }))
	if err != nil {
		t.Fatal(err)
	}
	controller.AttachScheduler(recorder)
	response := controller.Handle(context.Background(), controllerRequest("uncertain-unbind", "device.unbind", `{"device_id":"device_a","expected_revision":3}`))
	assertControllerError(t, response, ErrorCodeInternal)
	job, exists := controller.jobs.Get("job-uncertain-unbind")
	if !exists || len(recorder.jobs) != 1 || recorder.jobs[0].ID != job.ID {
		t.Fatalf("uncertain durable cleanup = job %#v exists=%t submitted=%#v", job, exists, recorder.jobs)
	}
}

func TestControllerSubmitsCleanupWhenRuntimePersistenceFailsAfterDesiredCommit(t *testing.T) {
	desired := &memoryDesiredStore{cfg: controllerConfig()}
	runtime := &memoryRuntimePersistence{failSaveAt: 2}
	recorder := &controllerSchedulerRecorder{}
	controller, err := NewController(desired, runtime, NewMachine(nil), NewJobStore(), WithControllerJobIDSource(func() string { return "job-runtime-failure-unbind" }))
	if err != nil {
		t.Fatal(err)
	}
	controller.AttachScheduler(recorder)
	response := controller.Handle(context.Background(), controllerRequest("runtime-failure-unbind", "device.unbind", `{"device_id":"device_a","expected_revision":3}`))
	assertControllerSuccess(t, response)
	job, exists := controller.jobs.Get("job-runtime-failure-unbind")
	if !exists || len(recorder.jobs) != 1 || recorder.jobs[0].ID != job.ID {
		t.Fatalf("runtime persistence cleanup = job %#v exists=%t submitted=%#v", job, exists, recorder.jobs)
	}
}

func TestControllerRecoversWhenRuntimeRevisionIsAheadOfDesired(t *testing.T) {
	desired := controllerConfig()
	runtime := &memoryRuntimePersistence{exists: true, snapshot: RuntimeSnapshot{SchemaVersion: RuntimeSnapshotSchemaVersion, ConfigRevision: desired.Revision + 1}}
	controller, err := NewController(&memoryDesiredStore{cfg: desired}, runtime, NewMachine(nil), NewJobStore())
	if err != nil {
		t.Fatalf("NewController() error = %v", err)
	}
	if controller.desired.Revision != desired.Revision {
		t.Fatalf("controller desired revision = %d", controller.desired.Revision)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.snapshot.ConfigRevision != desired.Revision {
		t.Fatalf("recovered runtime revision = %d", runtime.snapshot.ConfigRevision)
	}
}

func TestControllerRebindQueuesOldNodeBeforeNewNode(t *testing.T) {
	cfg := controllerConfig()
	configPath := writeControllerConfig(t, cfg)
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-rebind" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("rebind", "device.bind", `{"device_id":"device_a","node_id":"node_b","expected_revision":3}`))
	assertControllerSuccess(t, response)
	job, exists := controller.jobs.Get("job-rebind")
	if !exists || len(job.Nodes) != 2 || job.Nodes[0].NodeID != "node_a" || job.Nodes[1].NodeID != "node_b" {
		t.Fatalf("rebind job did not order old-node revocation before new-node admission: %#v", job)
	}
}

func TestControllerRejectsBindingToDisabledNode(t *testing.T) {
	cfg := controllerConfig()
	node := cfg.Nodes["node_b"]
	node.Enabled = false
	cfg.Nodes["node_b"] = node
	configPath := writeControllerConfig(t, cfg)
	jobs := NewJobStore()
	controller, err := NewController(config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), jobs)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("bind-disabled", "device.bind", `{"device_id":"device_a","node_id":"node_b","expected_revision":3}`))
	assertControllerError(t, response, ErrorCodeInvalidConfig)
	stored, _ := config.NewStore(configPath).Load()
	if stored.Revision != 3 || stored.Devices["device_a"].NodeID != "node_a" || len(jobs.List()) != 0 {
		t.Fatalf("disabled-node bind changed state: revision=%d device=%#v jobs=%#v", stored.Revision, stored.Devices["device_a"], jobs.List())
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
	applied             []model.Device
	removed             []model.Device
	applyErr            error
	removeErr           error
	applyWaitForContext bool
}

type controllerSchedulerRecorder struct{ jobs []Job }

func (recorder *controllerSchedulerRecorder) Submit(job Job) {
	recorder.jobs = append(recorder.jobs, cloneJob(job))
}

func (manager *controllerLeaseManager) Apply(ctx context.Context, device model.Device, _ uint64) error {
	manager.applied = append(manager.applied, device)
	if manager.applyWaitForContext {
		<-ctx.Done()
		return ctx.Err()
	}
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
func (s *failingDesiredStore) EnsureDurable(context.Context) error { return nil }

type failOnceDesiredStore struct {
	mu           sync.Mutex
	cfg          model.DesiredConfig
	replaceCalls int
}

func (s *failOnceDesiredStore) Load() (model.DesiredConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneControllerConfig(s.cfg), nil
}

func (s *failOnceDesiredStore) Replace(_ context.Context, expected uint64, next model.DesiredConfig) (model.DesiredConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaceCalls++
	if s.replaceCalls == 1 {
		return model.DesiredConfig{}, errors.New("first replace failed")
	}
	if s.cfg.Revision != expected {
		return model.DesiredConfig{}, codeError(ErrorCodeRevisionConflict, "revision conflict")
	}
	next.Revision = expected + 1
	s.cfg = cloneControllerConfig(next)
	return cloneControllerConfig(next), nil
}

func (s *failOnceDesiredStore) EnsureDurable(context.Context) error { return nil }

type postCommitErrorDesiredStore struct {
	mu        sync.Mutex
	cfg       model.DesiredConfig
	ensureErr error
}

func (s *postCommitErrorDesiredStore) Load() (model.DesiredConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneControllerConfig(s.cfg), nil
}

func (s *postCommitErrorDesiredStore) Replace(_ context.Context, expected uint64, next model.DesiredConfig) (model.DesiredConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Revision != expected {
		return model.DesiredConfig{}, codeError(ErrorCodeRevisionConflict, "revision conflict")
	}
	next.Revision = expected + 1
	s.cfg = cloneControllerConfig(next)
	return model.DesiredConfig{}, errors.New("directory sync failed after rename")
}

func (s *postCommitErrorDesiredStore) EnsureDurable(context.Context) error { return s.ensureErr }

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
