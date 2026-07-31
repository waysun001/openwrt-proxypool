package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/model"
)

type failOnMutation struct {
	calls atomic.Int32
}

func (m *failOnMutation) Mutate(context.Context, string) error {
	m.calls.Add(1)
	panic("shadow attempted a platform mutation")
}

func TestShadowLoadsV2AndExposesSanitizedDesiredRuntimeAndJob(t *testing.T) {
	path := copyEngineFixture(t, "../config/testdata/v2-valid.uci")
	mutator := &failOnMutation{}
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	shadow := NewShadow(path, mutator, WithShadowClock(func() time.Time { return now }), WithJobIDSource(func() string { return "reconcile-one" }))

	shadow.Start()
	status := shadow.Status()

	if status.Mode != "v2_shadow" || status.Config.State != ConfigStateReady {
		t.Fatalf("unexpected mode/config state: %#v", status)
	}
	if status.Config.SchemaVersion != 2 || status.Config.Revision != 9 {
		t.Fatalf("unexpected config summary: %#v", status.Config)
	}
	if len(status.Desired.Nodes) != 2 || status.Desired.Nodes[0].ID != "node_a" || status.Desired.Nodes[1].ID != "node_b" {
		t.Fatalf("desired nodes are not stable and sorted: %#v", status.Desired.Nodes)
	}
	if len(status.Desired.Devices) != 2 || status.Desired.Devices[0].ID != "device_a" || status.Desired.Devices[1].ID != "device_b" {
		t.Fatalf("desired devices are not stable and sorted: %#v", status.Desired.Devices)
	}
	if len(status.Runtime.Nodes) != 2 || status.Runtime.Nodes[0].State != model.StateDisabled || status.Runtime.Nodes[1].State != model.StateDisabled {
		t.Fatalf("shadow runtime must remain disabled: %#v", status.Runtime.Nodes)
	}
	if status.Reconciliation.ID != "reconcile-one" || status.Reconciliation.Kind != "reconciliation" || status.Reconciliation.Creator != "system" || status.Reconciliation.State != JobSucceeded || status.Reconciliation.Total != 2 || status.Reconciliation.Succeeded != 2 || !status.Reconciliation.CreatedAt.Equal(now) {
		t.Fatalf("unexpected reconciliation summary: %#v", status.Reconciliation)
	}
	if got := mutator.calls.Load(); got != 0 {
		t.Fatalf("shadow invoked %d platform mutations", got)
	}

	assertStatusHasNoSensitiveModelTypes(t, reflect.TypeOf(status))
	assertStatusSecretSafe(t, status, "fixture-password-not-real", "fixture-token-not-real", "fixture-obfs-key-not-real")
}

func TestShadowClassifiesLegacyAndInvalidConfigWithoutMutation(t *testing.T) {
	tests := []struct {
		name      string
		contents  string
		wantState ConfigState
		wantCode  string
		secret    string
	}{
		{
			name:      "legacy",
			contents:  "config global 'global'\n\toption enabled '1'\n\toption max_clients '60'\n\nconfig client 'old'\n\toption enabled '1'\n\toption name 'old'\n\toption type 'l2tp'\n\toption server 'vpn.example.com'\n\toption port '1701'\n\toption username 'alice'\n\toption password 'legacy-password'\n",
			wantState: ConfigStateMigrationRequired,
			wantCode:  "migration_required",
			secret:    "legacy-password",
		},
		{
			name:      "invalid declared v2",
			contents:  "config global 'global'\n\toption schema_version '2'\n\toption revision '1'\n\toption password 'broken-v2-secret'\n",
			wantState: ConfigStateInvalid,
			wantCode:  "invalid_config",
			secret:    "broken-v2-secret",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxypool")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			mutator := &failOnMutation{}
			shadow := NewShadow(path, mutator, WithJobIDSource(func() string { return "must-not-be-used" }))

			shadow.Start()
			status := shadow.Status()

			if status.Config.State != test.wantState || status.Config.Error == nil || status.Config.Error.Code != test.wantCode {
				t.Fatalf("unexpected status: %#v", status)
			}
			if status.Reconciliation.ID != "" || len(status.Desired.Nodes) != 0 || len(status.Runtime.Nodes) != 0 {
				t.Fatalf("unsafe config created desired/runtime work: %#v", status)
			}
			if got := mutator.calls.Load(); got != 0 {
				t.Fatalf("unsafe config invoked %d mutations", got)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) || afterInfo.Mode() != beforeInfo.Mode() || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
				t.Fatal("shadow classification modified the source configuration")
			}
			assertStatusSecretSafe(t, status, test.secret)
		})
	}
}

func TestShadowCreatesANewInMemoryReconciliationJobOnEachStart(t *testing.T) {
	path := copyEngineFixture(t, "../config/testdata/v2-valid.uci")
	first := NewShadow(path, nil, WithJobIDSource(func() string { return "boot-one" }))
	second := NewShadow(path, nil, WithJobIDSource(func() string { return "boot-two" }))

	first.Start()
	second.Start()

	if got := first.Status().Reconciliation.ID; got != "boot-one" {
		t.Fatalf("first job ID = %q", got)
	}
	if got := second.Status().Reconciliation.ID; got != "boot-two" {
		t.Fatalf("second job ID = %q", got)
	}
	if first.Status().Reconciliation.ID == second.Status().Reconciliation.ID {
		t.Fatal("restart reused an in-memory job ID")
	}
}

func TestShadowCreatesACompletedEmptyReconciliationJob(t *testing.T) {
	contents := strictEmptyV2Config()
	path := filepath.Join(t.TempDir(), "proxypool")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	shadow := NewShadow(path, nil, WithJobIDSource(func() string { return "empty-boot" }))

	shadow.Start()
	job := shadow.Status().Reconciliation

	if job.ID != "empty-boot" || job.State != JobSucceeded || job.Total != 0 || job.Succeeded != 0 {
		t.Fatalf("unexpected empty reconciliation: %#v", job)
	}
}

func TestShadowRunStopsWhenContextIsCancelled(t *testing.T) {
	path := copyEngineFixture(t, "../config/testdata/v2-valid.uci")
	shadow := NewShadow(path, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- shadow.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestShadowHandlerAllowsOnlyStatusGetWithEmptyParams(t *testing.T) {
	path := copyEngineFixture(t, "../config/testdata/v2-valid.uci")
	shadow := NewShadow(path, nil)
	shadow.Start()

	response := shadow.Handle(context.Background(), api.Request{Version: api.ProtocolVersion, ID: "one", Method: "status.get", Params: json.RawMessage(`{}`)})
	if response.Error != nil || len(response.Result) == 0 {
		t.Fatalf("status.get response = %#v", response)
	}
	var status ShadowStatus
	if err := json.Unmarshal(response.Result, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Config.State != ConfigStateReady {
		t.Fatalf("status config state = %q", status.Config.State)
	}

	for _, request := range []api.Request{
		{Version: api.ProtocolVersion, ID: "two", Method: "node.save", Params: json.RawMessage(`{}`)},
		{Version: api.ProtocolVersion, ID: "three", Method: "status.get", Params: json.RawMessage(`{"unexpected":true}`)},
	} {
		response := shadow.Handle(context.Background(), request)
		if response.Error == nil || response.Result != nil {
			t.Fatalf("request %q unexpectedly succeeded: %#v", request.Method, response)
		}
	}
}

func copyEngineFixture(t *testing.T, source string) string {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "proxypool")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertStatusHasNoSensitiveModelTypes(t *testing.T, root reflect.Type) {
	t.Helper()
	forbidden := map[reflect.Type]struct{}{
		reflect.TypeOf(model.Node{}):          {},
		reflect.TypeOf(model.DesiredConfig{}): {},
		reflect.TypeOf((*error)(nil)).Elem():  {},
	}
	seen := make(map[reflect.Type]struct{})
	var walk func(reflect.Type)
	walk = func(current reflect.Type) {
		if current == nil {
			return
		}
		if _, found := forbidden[current]; found {
			t.Fatalf("status DTO embeds forbidden type %v", current)
		}
		if _, found := seen[current]; found {
			return
		}
		seen[current] = struct{}{}
		switch current.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(current.Elem())
		case reflect.Struct:
			for i := 0; i < current.NumField(); i++ {
				walk(current.Field(i).Type)
			}
		}
	}
	walk(root)
}

func assertStatusSecretSafe(t *testing.T, status ShadowStatus, secrets ...string) {
	t.Helper()
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	renderings := []string{string(encoded)}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		renderings = append(renderings, fmt.Sprintf(format, status))
	}
	for _, rendered := range renderings {
		for _, secret := range secrets {
			if strings.Contains(rendered, secret) {
				t.Fatalf("status leaked secret %q in %q", secret, rendered)
			}
		}
	}
}

func strictEmptyV2Config() string {
	return `config global 'global'
	option schema_version '2'
	option revision '1'
	option enabled '1'
	option runtime_backend 'v2_shadow'
	option max_nodes '60'
	option lan_device 'br-lan'
	list management_port '80'
	list management_port '443'
	option l2tp_concurrency '4'
	option proxy_concurrency '8'
	option connect_timeout '90s'
	option stop_timeout '30s'
	list doh_url 'https://cloudflare-dns.com/dns-query'
	list doh_bootstrap_ip '1.1.1.1'
	list doh_server_name 'cloudflare-dns.com'
`
}
