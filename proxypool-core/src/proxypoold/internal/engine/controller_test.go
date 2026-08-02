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
	"proxypoold/internal/diagnostics"
	"proxypoold/internal/importer"
	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

func TestControllerDiagnosticsLifecycleUsesStrictSafeDTOs(t *testing.T) {
	service := &controllerDiagnosticsService{
		created: diagnostics.DiagnosticStatus{ID: "diagnostic-job-1", State: diagnostics.DiagnosticQueued, CreatedAt: stateTestEpoch, UpdatedAt: stateTestEpoch},
		status: diagnostics.DiagnosticStatus{ID: "diagnostic-job-1", State: diagnostics.DiagnosticReady, CreatedAt: stateTestEpoch, UpdatedAt: stateTestEpoch,
			Artifact: &diagnostics.Artifact{ID: "diag-0123456789abcdef", State: diagnostics.ArtifactReady, Filename: "proxypool-diagnostics-diag-0123456789abcdef.tar.gz", Size: 123, CreatedAt: stateTestEpoch, ExpiresAt: stateTestEpoch.Add(time.Minute)}},
		claim: diagnostics.ArtifactClaim{ArtifactID: "diag-0123456789abcdef", Path: "/tmp/proxypool/diagnostics/diag-0123456789abcdef.tar.gz", Filename: "proxypool-diagnostics-diag-0123456789abcdef.tar.gz", Size: 123},
	}
	controller, err := NewController(
		config.NewStore(writeControllerConfig(t, controllerConfig())), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), NewJobStore(),
		WithDiagnostics(service),
	)
	if err != nil {
		t.Fatal(err)
	}

	created := controller.Handle(context.Background(), controllerRequest("diagnostic-create", "diagnostics.create", `{}`))
	assertControllerSuccess(t, created)
	if !bytes.Contains(created.Result, []byte(`"job_id":"diagnostic-job-1"`)) || bytes.Contains(created.Result, []byte(`/tmp/`)) {
		t.Fatalf("unsafe create result: %s", created.Result)
	}

	got := controller.Handle(context.Background(), controllerRequest("diagnostic-get", "diagnostics.get", `{"job_id":"diagnostic-job-1"}`))
	assertControllerSuccess(t, got)
	if !bytes.Contains(got.Result, []byte(`"artifact_id":"diag-0123456789abcdef"`)) || bytes.Contains(got.Result, []byte(`/tmp/`)) {
		t.Fatalf("unsafe status result: %s", got.Result)
	}

	claimed := controller.Handle(context.Background(), controllerRequest("diagnostic-claim", "diagnostics.claim", `{"artifact_id":"diag-0123456789abcdef"}`))
	assertControllerSuccess(t, claimed)
	if !bytes.Contains(claimed.Result, []byte(`"path":"/tmp/proxypool/diagnostics/`)) {
		t.Fatalf("claim result = %s", claimed.Result)
	}
	released := controller.Handle(context.Background(), controllerRequest("diagnostic-release", "diagnostics.release", `{"artifact_id":"diag-0123456789abcdef"}`))
	assertControllerSuccess(t, released)
	if service.released != "diag-0123456789abcdef" {
		t.Fatalf("released = %q", service.released)
	}
	snapshot, err := controller.DiagnosticSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encodedEntries, _ := json.Marshal(snapshot.Entries)
	for _, name := range []string{"status.json", "config-summary.json", "jobs.json", "events.json", "daemon-metrics.json", "managed-processes.json"} {
		if !json.Valid(snapshot.Entries[name]) {
			t.Fatalf("missing or invalid diagnostic seed %s", name)
		}
	}
	for _, secret := range []string{"user-a", "password-a", "user-b", "password-b"} {
		if bytes.Contains(encodedEntries, []byte(secret)) {
			t.Fatalf("diagnostic seed leaked %s", secret)
		}
		found := false
		for _, known := range snapshot.Secrets {
			if known == secret {
				found = true
			}
		}
		if !found {
			t.Fatalf("diagnostic redactor omitted %s", secret)
		}
	}

	for _, test := range []struct{ method, params string }{
		{"diagnostics.create", `{"future":true}`}, {"diagnostics.get", `{}`}, {"diagnostics.get", `{"job_id":"bad id"}`},
		{"diagnostics.claim", `{"artifact_id":"../escape"}`}, {"diagnostics.release", `{"artifact_id":"diag-0123456789abcdef","future":true}`},
	} {
		assertControllerError(t, controller.Handle(context.Background(), controllerRequest("invalid-diagnostic", test.method, test.params)), ErrorCodeInvalidRequest)
	}
	service.createErr = diagnostics.ErrDiagnosticCapacity
	assertControllerError(t, controller.Handle(context.Background(), controllerRequest("diagnostic-capacity", "diagnostics.create", `{}`)), ErrorCodeCapacityExceeded)
	service.createErr = errors.New("collector unavailable")
	assertControllerError(t, controller.Handle(context.Background(), controllerRequest("diagnostic-internal", "diagnostics.create", `{}`)), ErrorCodeInternal)
}

type controllerDiagnosticsService struct {
	created   diagnostics.DiagnosticStatus
	status    diagnostics.DiagnosticStatus
	claim     diagnostics.ArtifactClaim
	released  string
	createErr error
}

func (s *controllerDiagnosticsService) Create() (diagnostics.DiagnosticStatus, error) {
	return s.created, s.createErr
}
func (s *controllerDiagnosticsService) Get(string) (diagnostics.DiagnosticStatus, bool) {
	return s.status, true
}
func (s *controllerDiagnosticsService) Claim(string) (diagnostics.ArtifactClaim, error) {
	return s.claim, nil
}
func (s *controllerDiagnosticsService) Release(id string) error { s.released = id; return nil }

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
		{name: "node save missing revision", method: "node.save", params: `{"name":"New","protocol":"l2tp","enabled":true,"server":"new.example","port":1701,"username":"user","password":"password"}`, code: ErrorCodeInvalidRequest},
		{name: "node delete missing revision", method: "node.delete", params: `{"node_id":"node_a"}`, code: ErrorCodeInvalidRequest},
		{name: "unknown method", method: "node.future", params: `{}`, code: "unknown_method"},
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

func TestControllerNodeDeleteAtomicallyOfflinesBindingsAndPersistsTombstone(t *testing.T) {
	cfg := controllerConfig()
	cfg.PendingBindings = map[string]model.PendingBinding{
		"pending_a": {ID: "pending_a", LegacyIPv4: netip.MustParseAddr("192.168.9.20"), NodeID: "node_a", CreatedAt: stateTestEpoch},
	}
	configPath := writeControllerConfig(t, cfg)
	jobs := NewJobStore()
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), jobs,
		WithControllerJobIDSource(func() string { return "job-node-delete" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("delete-node-a", "node.delete", `{"node_id":"node_a","expected_revision":3}`))
	assertControllerSuccess(t, response)
	stored, err := config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	node, exists := stored.Nodes["node_a"]
	device := stored.Devices["device_a"]
	if stored.Revision != 4 || !exists || node.Enabled || !node.DeletePending || device.Enabled || device.NodeID != "" || len(stored.PendingBindings) != 0 {
		t.Fatalf("delete tombstone = revision %d node %#v exists=%t device %#v pending %#v", stored.Revision, node, exists, device, stored.PendingBindings)
	}
	job, exists := jobs.Get("job-node-delete")
	if !exists || job.Kind != "node.delete" || len(job.Nodes) != 1 || job.Nodes[0].NodeID != "node_a" {
		t.Fatalf("delete job = %#v exists=%t", job, exists)
	}
	status := controller.Handle(context.Background(), controllerRequest("status-after-delete", "status.get", `{}`))
	assertControllerSuccess(t, status)
	if !bytes.Contains(status.Result, []byte(`"delete_pending":true`)) || bytes.Contains(status.Result, []byte("password-a")) {
		t.Fatalf("delete status is unsafe or incomplete: %s", status.Result)
	}
	action := controller.Handle(context.Background(), controllerRequest("connect-deleting", "node.action", `{"node_id":"node_a","action":"connect","expected_revision":4}`))
	assertControllerError(t, action, ErrorCodeInvalidConfig)
	save := controller.Handle(context.Background(), controllerRequest("save-deleting", "node.save", `{"node_id":"node_a","name":"Node A","protocol":"l2tp","enabled":true,"server":"a.example","port":1701,"username":"","password":"","expected_revision":4}`))
	assertControllerError(t, save, ErrorCodeInvalidConfig)
}

func TestControllerNodeDeleteRecognizesPostRenameCommitAndQueuesCleanup(t *testing.T) {
	desired := &postCommitErrorDesiredStore{cfg: controllerConfig()}
	recorder := &controllerSchedulerRecorder{}
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-post-rename-delete" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.AttachScheduler(recorder)
	response := controller.Handle(context.Background(), controllerRequest("post-rename-delete", "node.delete", `{"node_id":"node_a","expected_revision":3}`))
	assertControllerSuccess(t, response)
	stored, err := desired.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Nodes["node_a"].DeletePending || stored.Nodes["node_a"].Enabled || stored.Devices["device_a"].Enabled || stored.Devices["device_a"].NodeID != "" {
		t.Fatalf("post-rename delete state = %#v", stored)
	}
	job, exists := controller.jobs.Get("job-post-rename-delete")
	if !exists || len(recorder.jobs) != 1 || recorder.jobs[0].ID != job.ID {
		t.Fatalf("post-rename cleanup = job %#v exists=%t submitted=%#v", job, exists, recorder.jobs)
	}
}

func TestControllerNodeDeleteUncertainDurabilityStillQueuesFailClosedCleanup(t *testing.T) {
	desired := &postCommitErrorDesiredStore{cfg: controllerConfig(), ensureErr: errors.New("directory sync unavailable")}
	recorder := &controllerSchedulerRecorder{}
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-uncertain-delete" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.AttachScheduler(recorder)
	response := controller.Handle(context.Background(), controllerRequest("uncertain-delete", "node.delete", `{"node_id":"node_a","expected_revision":3}`))
	assertControllerError(t, response, ErrorCodeInternal)
	job, exists := controller.jobs.Get("job-uncertain-delete")
	if !exists || len(recorder.jobs) != 1 || recorder.jobs[0].ID != job.ID {
		t.Fatalf("uncertain delete cleanup = job %#v exists=%t submitted=%#v", job, exists, recorder.jobs)
	}
}

func TestControllerNodeSaveCreatesL2TPWithServerAllocatedIdentity(t *testing.T) {
	cfg := controllerConfig()
	configPath := writeControllerConfig(t, cfg)
	jobs := NewJobStore()
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), jobs,
		WithControllerJobIDSource(func() string { return "job-node-create" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest(
		"node-create", "node.save", `{"name":"Node C","protocol":"l2tp","enabled":true,"server":"c.example","port":1701,"username":"user-c","password":"new-node-secret","expected_revision":3}`,
	))
	assertControllerSuccess(t, response)
	if bytes.Contains(response.Result, []byte("new-node-secret")) {
		t.Fatal("node.save response exposed the password")
	}
	stored, err := config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 4 || len(stored.Nodes) != 3 {
		t.Fatalf("stored config = revision %d nodes %d", stored.Revision, len(stored.Nodes))
	}
	var created model.Node
	for id, node := range stored.Nodes {
		if id != "node_a" && id != "node_b" {
			created = node
		}
	}
	if created.ID == "" || created.PolicyID != 3 || created.Password != "new-node-secret" || created.Revision != 4 || !created.Enabled {
		t.Fatalf("created node = %#v", created)
	}
	job, exists := jobs.Get("job-node-create")
	if !exists || job.Kind != "node.save" || len(job.Nodes) != 1 || job.Nodes[0].NodeID != created.ID || job.ConfigRevision != 4 {
		t.Fatalf("created job = %#v exists=%t", job, exists)
	}
}

func TestControllerNodeSavePreservesBlankPasswordAndReplacesExplicitSecret(t *testing.T) {
	configPath := writeControllerConfig(t, controllerConfig())
	jobIDs := []string{"job-node-preserve", "job-node-replace"}
	controller, err := NewController(
		config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string {
			id := jobIDs[0]
			jobIDs = jobIDs[1:]
			return id
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	preserve := controller.Handle(context.Background(), controllerRequest(
		"node-preserve", "node.save", `{"node_id":"node_a","name":"Node A edited","protocol":"l2tp","enabled":true,"server":"a-edited.example","port":1701,"username":"","password":"","expected_revision":3}`,
	))
	assertControllerSuccess(t, preserve)
	stored, err := config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Nodes["node_a"].Username != "user-a" || stored.Nodes["node_a"].Password != "password-a" || stored.Nodes["node_a"].Server != "a-edited.example" {
		t.Fatalf("blank password update = %#v", stored.Nodes["node_a"])
	}
	replace := controller.Handle(context.Background(), controllerRequest(
		"node-replace", "node.save", `{"node_id":"node_a","name":"Node A edited","protocol":"l2tp","enabled":true,"server":"a-edited.example","port":1701,"username":"user-a","password":"replacement-secret","expected_revision":4}`,
	))
	assertControllerSuccess(t, replace)
	if bytes.Contains(replace.Result, []byte("replacement-secret")) {
		t.Fatal("node.save response exposed the replacement password")
	}
	stored, err = config.NewStore(configPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 5 || stored.Nodes["node_a"].Password != "replacement-secret" || stored.Nodes["node_a"].Revision != 5 {
		t.Fatalf("explicit password update = revision %d node %#v", stored.Revision, stored.Nodes["node_a"])
	}
}

func TestControllerStoredMutationMatchIncludesNodeSecrets(t *testing.T) {
	current := controllerConfig()
	next := cloneControllerConfig(current)
	node := next.Nodes["node_a"]
	node.Password = "replacement-secret"
	next.Nodes[node.ID] = node
	observed := cloneControllerConfig(next)
	observed.Revision = 4
	observedNode := observed.Nodes["node_a"]
	observedNode.Revision = 4
	observedNode.Password = "password-a"
	observed.Nodes[observedNode.ID] = observedNode
	if controllerConfigMatchesStoredMutation(current, next, observed) {
		t.Fatal("ambiguous commit comparison ignored a mismatched password")
	}
}

func TestControllerNodeSaveRejectsUnsupportedProtocolAndSixtyFirstNodeWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		cfg    model.DesiredConfig
		params string
		code   string
	}{
		{
			name: "socks5 is not yet implemented", cfg: controllerConfig(), code: ErrorCodeUnsupported,
			params: `{"name":"Proxy","protocol":"socks5","enabled":false,"server":"proxy.example","port":1080,"username":"","password":"","expected_revision":3}`,
		},
	}
	full := controllerConfig()
	full.Nodes = make(map[string]model.Node, 60)
	for index := 1; index <= 60; index++ {
		id := fmt.Sprintf("node_%02d", index)
		full.Nodes[id] = model.Node{ID: id, Name: fmt.Sprintf("Node %02d", index), Protocol: model.ProtocolL2TP, Enabled: true, Server: fmt.Sprintf("vpn-%02d.example", index), Port: 1701, Username: "user", Password: "password", PolicyID: uint16(index), Revision: 3}
	}
	full.Devices = map[string]model.Device{}
	tests = append(tests, struct {
		name   string
		cfg    model.DesiredConfig
		params string
		code   string
	}{
		name: "sixty first node", cfg: full, code: ErrorCodeCapacityExceeded,
		params: `{"name":"Overflow","protocol":"l2tp","enabled":true,"server":"overflow.example","port":1701,"username":"user","password":"password","expected_revision":3}`,
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := writeControllerConfig(t, test.cfg)
			jobs := NewJobStore()
			controller, err := NewController(config.NewStore(configPath), NewRuntimeStore(filepath.Join(t.TempDir(), "runtime.json")), NewMachine(nil), jobs)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			response := controller.Handle(context.Background(), controllerRequest("node-rejected", "node.save", test.params))
			assertControllerError(t, response, test.code)
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || len(jobs.List()) != 0 {
				t.Fatal("rejected node.save mutated config or jobs")
			}
		})
	}
}

func TestControllerNodeMethodsDoNotPromiseSuccessBeforeJobIsDurable(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params string
	}{
		{
			name: "save", method: "node.save",
			params: `{"node_id":"node_a","name":"Node A","protocol":"l2tp","enabled":true,"server":"a-new.example","port":1701,"username":"","password":"","expected_revision":3}`,
		},
		{name: "delete", method: "node.delete", params: `{"node_id":"node_a","expected_revision":3}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := &memoryDesiredStore{cfg: controllerConfig()}
			runtime := &memoryRuntimePersistence{failSaveAt: 2}
			recorder := &controllerSchedulerRecorder{}
			controller, err := NewController(
				desired, runtime, NewMachine(nil), NewJobStore(),
				WithControllerJobIDSource(func() string { return "job-durability-" + test.name }),
			)
			if err != nil {
				t.Fatal(err)
			}
			controller.AttachScheduler(recorder)
			response := controller.Handle(context.Background(), controllerRequest("durability-"+test.name, test.method, test.params))
			assertControllerError(t, response, ErrorCodeInternal)
			stored, err := desired.Load()
			if err != nil {
				t.Fatal(err)
			}
			job, exists := controller.jobs.Get("job-durability-" + test.name)
			if stored.Revision != 4 || !exists || len(recorder.jobs) != 1 || recorder.jobs[0].ID != job.ID {
				t.Fatalf("durability failure = revision %d job %#v exists=%t submitted=%#v", stored.Revision, job, exists, recorder.jobs)
			}
		})
	}
}

func TestControllerNodeSavePersistsReplayWithPromisedJobInOneSnapshot(t *testing.T) {
	desired := &memoryDesiredStore{cfg: controllerConfig()}
	runtime := &memoryRuntimePersistence{failSaveAt: 3}
	firstJobs := NewJobStore()
	first, err := NewController(
		desired, runtime, NewMachine(nil), firstJobs,
		WithControllerJobIDSource(func() string { return "job-atomic-node-replay" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := controllerRequest(
		"atomic-node-replay", "node.save", `{"node_id":"node_a","name":"Node A","protocol":"l2tp","enabled":true,"server":"a-atomic.example","port":1701,"username":"","password":"","expected_revision":3}`,
	)
	response := first.Handle(context.Background(), request)
	assertControllerSuccess(t, response)
	restarted, err := NewController(desired, runtime, NewMachine(nil), NewJobStore(), WithControllerJobIDSource(func() string { return "must-not-be-used" }))
	if err != nil {
		t.Fatal(err)
	}
	replayed := restarted.Handle(context.Background(), request)
	if !reflect.DeepEqual(replayed, response) {
		t.Fatalf("restart lost atomic node replay:\n got: %#v\nwant: %#v", replayed, response)
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
		for _, forbidden := range []string{"credential-do-not-return", "secret-user", `"password":`, `"username":`, "slp_token", "obfs_key"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s response leaked %q: %s", request.Method, forbidden, response.Result)
			}
		}
	}

	assertControllerError(t, controller.Handle(context.Background(), controllerRequest("bad-status", "status.get", `{"future":1}`)), ErrorCodeInvalidRequest)
	assertControllerError(t, controller.Handle(context.Background(), controllerRequest("bad-job", "job.get", `{"job_id":"missing"}`)), ErrorCodeNotFound)
	assertControllerError(t, controller.Handle(context.Background(), controllerRequest("bad-events", "system.events", `{"after_sequence":0,"limit":1001}`)), ErrorCodeInvalidRequest)
}

func TestControllerStatusReturnsEditableNonSecretNodeFields(t *testing.T) {
	cfg := controllerConfig()
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	node := cfg.Nodes["node_a"]
	node.ExpiresAt = &expires
	cfg.Nodes[node.ID] = node
	controller, err := NewController(&memoryDesiredStore{cfg: cfg}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore())
	if err != nil {
		t.Fatal(err)
	}
	response := controller.Handle(context.Background(), controllerRequest("editable-status", "status.get", `{}`))
	assertControllerSuccess(t, response)
	encoded := string(response.Result)
	for _, required := range []string{`"server":"a.example"`, `"port":1701`, `"has_username":true`, `"has_password":true`, `"expires_at":"2030-01-02T03:04:05Z"`, `"revision":3`} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("status omitted editable field %s: %s", required, response.Result)
		}
	}
	for _, forbidden := range []string{"user-a", "password-a"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("status exposed credential %s", forbidden)
		}
	}
}

func TestControllerStatusReturnsPublicRuntimeFailureAndRetryMetadata(t *testing.T) {
	cfg := controllerConfig()
	controller, err := NewController(&memoryDesiredStore{cfg: cfg}, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore())
	if err != nil {
		t.Fatal(err)
	}
	controller.statuses["node_a"] = NodeStatus{
		NodeID: "node_a", State: model.StateBackoff, Attempts: 3,
		LastError: &PublicError{Code: "probe_failed", Message: "credential=DO-NOT-RETURN"},
		RetryAt:   stateTestEpoch.Add(15 * time.Second),
	}
	response := controller.Handle(context.Background(), controllerRequest("runtime-status", "status.get", `{}`))
	assertControllerSuccess(t, response)
	encoded := string(response.Result)
	for _, required := range []string{`"state":"backoff"`, `"attempts":3`, `"last_error":{"code":"probe_failed"`, `"retry_at":"`} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("status omitted runtime field %s: %s", required, response.Result)
		}
	}
	if strings.Contains(encoded, "DO-NOT-RETURN") {
		t.Fatalf("status exposed private runtime error: %s", response.Result)
	}
	if strings.Contains(encoded, `"retry_at":"0001-01-01T00:00:00Z"`) {
		t.Fatalf("status exposed a false retry deadline for ordinary nodes: %s", response.Result)
	}
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

func TestControllerLearnsPendingBindingFromConfirmedDHCP(t *testing.T) {
	cfg := controllerConfig()
	cfg.Devices = map[string]model.Device{}
	cfg.PendingBindings = map[string]model.PendingBinding{
		"pending_192_168_9_20": {
			ID: "pending_192_168_9_20", LegacyIPv4: netip.MustParseAddr("192.168.9.20"),
			NodeID: "node_a", CreatedAt: stateTestEpoch,
		},
	}
	desired := &memoryDesiredStore{cfg: cfg}
	discovered := platform.DiscoveredDevice{
		ID: "device_001122334455", MAC: "00:11:22:33:44:55", IPv4: netip.MustParseAddr("192.168.9.20"),
		Hostname: "phone", Ingress: "lan1", Confirmed: true,
	}
	leases := &controllerLeaseManager{}
	recorder := &controllerSchedulerRecorder{}
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithDeviceServices(&controllerDeviceSource{devices: []platform.DiscoveredDevice{discovered}}, leases),
		WithControllerJobIDSource(func() string { return "job-pending-learn" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.AttachScheduler(recorder)
	if err := controller.LearnPendingBindings(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := desired.Load()
	if err != nil {
		t.Fatal(err)
	}
	device := stored.Devices[discovered.ID]
	if stored.Revision != 4 || len(stored.PendingBindings) != 0 || device.MAC != discovered.MAC || device.NodeID != "node_a" || !device.Enabled {
		t.Fatalf("learned config = revision %d pending %#v device %#v", stored.Revision, stored.PendingBindings, device)
	}
	if len(leases.applied) != 1 || len(recorder.jobs) != 1 || recorder.jobs[0].Kind != "pending.learn" {
		t.Fatalf("learned side effects = leases %#v jobs %#v", leases.applied, recorder.jobs)
	}
}

func TestControllerKeepsAmbiguousPendingBindingWithoutRepeatedRevision(t *testing.T) {
	cfg := controllerConfig()
	cfg.Devices = map[string]model.Device{}
	cfg.PendingBindings = map[string]model.PendingBinding{
		"pending_192_168_9_20": {
			ID: "pending_192_168_9_20", LegacyIPv4: netip.MustParseAddr("192.168.9.20"),
			NodeID: "node_a", CreatedAt: stateTestEpoch,
		},
	}
	desired := &memoryDesiredStore{cfg: cfg}
	source := &controllerDeviceSource{devices: []platform.DiscoveredDevice{
		{ID: "device_001122334455", MAC: "00:11:22:33:44:55", IPv4: netip.MustParseAddr("192.168.9.20"), Confirmed: true},
		{ID: "device_001122334466", MAC: "00:11:22:33:44:66", IPv4: netip.MustParseAddr("192.168.9.20"), Confirmed: true},
	}}
	leases := &controllerLeaseManager{}
	controller, err := NewController(desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(), WithDeviceServices(source, leases))
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.LearnPendingBindings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := controller.LearnPendingBindings(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, _ := desired.Load()
	pending := stored.PendingBindings["pending_192_168_9_20"]
	if stored.Revision != 4 || pending.ErrorCode != "duplicate" || len(stored.Devices) != 0 || desired.replaceCount != 1 || len(leases.applied) != 0 || len(controller.jobs.List()) != 0 {
		t.Fatalf("ambiguous learn = revision %d pending %#v devices %#v replaces %d", stored.Revision, pending, stored.Devices, desired.replaceCount)
	}
}

func TestControllerLearnsIndependentPendingWhenOfflineAddressStillConflicts(t *testing.T) {
	cfg := controllerConfig()
	cfg.Devices = map[string]model.Device{
		"device_existing": {
			ID: "device_existing", MAC: "00:11:22:33:44:10", FixedIPv4: netip.MustParseAddr("192.168.9.20"),
			NodeID: "node_a", Enabled: true,
		},
	}
	cfg.PendingBindings = map[string]model.PendingBinding{
		"pending_192_168_9_20": {
			ID: "pending_192_168_9_20", LegacyIPv4: netip.MustParseAddr("192.168.9.20"),
			NodeID: "node_a", CreatedAt: stateTestEpoch, ErrorCode: "duplicate",
		},
		"pending_192_168_9_21": {
			ID: "pending_192_168_9_21", LegacyIPv4: netip.MustParseAddr("192.168.9.21"),
			NodeID: "node_b", CreatedAt: stateTestEpoch,
		},
	}
	desired := &memoryDesiredStore{cfg: cfg}
	discovered := platform.DiscoveredDevice{
		ID: "device_001122334421", MAC: "00:11:22:33:44:21", IPv4: netip.MustParseAddr("192.168.9.21"), Confirmed: true,
	}
	leases := &controllerLeaseManager{}
	controller, err := NewController(
		desired, &memoryRuntimePersistence{}, NewMachine(nil), NewJobStore(),
		WithDeviceServices(&controllerDeviceSource{devices: []platform.DiscoveredDevice{discovered}}, leases),
		WithControllerJobIDSource(func() string { return "job-independent-pending-learn" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.LearnPendingBindings(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := desired.Load()
	if err != nil {
		t.Fatal(err)
	}
	conflict := stored.PendingBindings["pending_192_168_9_20"]
	learned := stored.Devices[discovered.ID]
	if stored.Revision != 4 || conflict.ErrorCode != "duplicate" || learned.NodeID != "node_b" || len(stored.PendingBindings) != 1 {
		t.Fatalf("partial learn = revision %d pending %#v learned %#v", stored.Revision, stored.PendingBindings, learned)
	}
	if len(leases.applied) != 1 || leases.applied[0].ID != discovered.ID {
		t.Fatalf("lease applications = %#v", leases.applied)
	}
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

func TestControllerRestartCompletesActiveJobForAlreadyFinalizedDelete(t *testing.T) {
	desiredStore := &memoryDesiredStore{cfg: controllerConfig()}
	runtime := &memoryRuntimePersistence{}
	first, err := NewController(
		desiredStore, runtime, NewMachine(nil), NewJobStore(),
		WithControllerJobIDSource(func() string { return "job-crash-after-delete-finalize" }),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertControllerSuccess(t, first.Handle(context.Background(), controllerRequest("crash-delete", "node.delete", `{"node_id":"node_a","expected_revision":3}`)))
	tombstone, err := desiredStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	finalized := cloneControllerConfig(tombstone)
	delete(finalized.Nodes, "node_a")
	if _, err := desiredStore.Replace(context.Background(), 4, finalized); err != nil {
		t.Fatal(err)
	}
	restartedJobs := NewJobStore()
	if _, err := NewController(desiredStore, runtime, NewMachine(nil), restartedJobs); err != nil {
		t.Fatal(err)
	}
	job, exists := restartedJobs.Get("job-crash-after-delete-finalize")
	if !exists || job.State != JobSucceeded {
		t.Fatalf("restart orphaned finalized delete job: %#v exists=%t", job, exists)
	}
	runtime.mu.Lock()
	runtimeRevision := runtime.snapshot.ConfigRevision
	runtime.mu.Unlock()
	if runtimeRevision != 5 {
		t.Fatalf("restart runtime revision = %d, want 5", runtimeRevision)
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
	for id, node := range next.Nodes {
		previous, exists := s.cfg.Nodes[id]
		if exists && sameControllerNodeIgnoringRevision(previous, node) {
			node.Revision = previous.Revision
		} else {
			node.Revision = next.Revision
		}
		next.Nodes[id] = node
	}
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
	for id, node := range next.Nodes {
		previous, exists := s.cfg.Nodes[id]
		if exists && sameControllerNodeIgnoringRevision(previous, node) {
			node.Revision = previous.Revision
		} else {
			node.Revision = next.Revision
		}
		next.Nodes[id] = node
	}
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
