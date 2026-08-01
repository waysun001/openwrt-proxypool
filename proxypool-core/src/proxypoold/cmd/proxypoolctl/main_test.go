package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"proxypoold/internal/api"
)

func TestRunVersion(t *testing.T) {
	var out, err bytes.Buffer
	if code := run([]string{"--version"}, bytes.NewReader(nil), &out, &err); code != 0 || out.Len() == 0 || err.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), err.String())
	}
}

func TestRunClassifyEmitsOnlySecretSafeStartupClass(t *testing.T) {
	strictV2 := `config global 'global'
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
	v2WithLegacyBackend := strings.Replace(strictV2, "option runtime_backend 'v2_shadow'", "option runtime_backend 'v1'", 1)
	tests := []struct {
		name       string
		contents   string
		missing    bool
		directory  bool
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "genuine legacy v1", contents: "config global 'global'\n\toption enabled '1'\n\toption max_clients '60'\n\nconfig client 'old'\n\toption password 'legacy-classifier-secret'\n", wantStdout: "v1\n"},
		{name: "strict v2", contents: strictV2, wantStdout: "v2_shadow\n"},
		{name: "declared invalid v2", contents: "config global 'global'\n\toption schema_version '2'\n\toption password 'declared-v2-secret'\n", wantStdout: "v2_shadow_invalid\n"},
		{name: "v2 schema cannot select legacy", contents: v2WithLegacyBackend, wantStdout: "v2_shadow_invalid\n"},
		{name: "unknown schema is not diagnostic v2", contents: "config global 'global'\n\toption schema_version '99'\n\toption runtime_backend 'v2_shadow'\n", wantCode: 1, wantStdout: "unknown\n", wantStderr: "configuration classification failed\n"},
		{name: "unknown backend", contents: "config global 'global'\n\toption runtime_backend 'surprise'\n\toption max_clients '60'\n", wantCode: 1, wantStdout: "unknown\n", wantStderr: "configuration classification failed\n"},
		{name: "unknown section", contents: "config mystery 'x'\n\toption password 'unknown-secret'\n", wantCode: 1, wantStdout: "unknown\n", wantStderr: "configuration classification failed\n"},
		{name: "missing file", missing: true, wantCode: 1, wantStdout: "unknown\n", wantStderr: "configuration classification failed\n"},
		{name: "unreadable path type", directory: true, wantCode: 1, wantStdout: "unknown\n", wantStderr: "configuration classification failed\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxypool")
			if test.directory {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if !test.missing {
				if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"classify", "--config", path}, bytes.NewReader(nil), &stdout, &stderr)
			if code != test.wantCode || stdout.String() != test.wantStdout || stderr.String() != test.wantStderr {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			for _, secret := range []string{"legacy-classifier-secret", "declared-v2-secret", "unknown-secret"} {
				if bytes.Contains(stdout.Bytes(), []byte(secret)) || bytes.Contains(stderr.Bytes(), []byte(secret)) {
					t.Fatalf("classifier leaked %q", secret)
				}
			}
		})
	}
}

func TestRunClassifyRequiresExactArguments(t *testing.T) {
	for _, args := range [][]string{
		{"classify"},
		{"classify", "--config"},
		{"classify", "--config", ""},
		{"classify", "--config=secret"},
		{"classify", "-config", "secret"},
		{"classify", "--config", "one", "extra"},
		{"classify", "--config", "one", "--config", "two"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, bytes.NewReader(nil), &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.String() != "usage: proxypoolctl classify --config PATH\n" {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunSelectBackendReportsStrictSelectorState(t *testing.T) {
	tests := []struct {
		name       string
		contents   string
		missing    bool
		directory  bool
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "v1", contents: "config global 'global'\n\toption runtime_backend 'v1'\n", wantStdout: "v1\n"},
		{name: "v2 shadow", contents: "config global 'global'\n\toption runtime_backend 'v2_shadow'\n", wantStdout: "v2_shadow\n"},
		{name: "missing compatibility selector", missing: true, wantStdout: "missing\n"},
		{name: "unknown backend", contents: "config global 'global'\n\toption runtime_backend 'future'\n", wantCode: 1, wantStdout: "unknown\n", wantStderr: "runtime selector classification failed\n"},
		{name: "extra option", contents: "config global 'global'\n\toption runtime_backend 'v1'\n\toption password 'selector-secret'\n", wantCode: 1, wantStdout: "unknown\n", wantStderr: "runtime selector classification failed\n"},
		{name: "unreadable path type", directory: true, wantCode: 1, wantStdout: "unknown\n", wantStderr: "runtime selector classification failed\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxypool_runtime")
			if test.directory {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if !test.missing {
				if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"select-backend", "--config", path}, bytes.NewReader(nil), &stdout, &stderr)
			if code != test.wantCode || stdout.String() != test.wantStdout || stderr.String() != test.wantStderr {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if bytes.Contains(stdout.Bytes(), []byte("selector-secret")) || bytes.Contains(stderr.Bytes(), []byte("selector-secret")) {
				t.Fatal("select-backend leaked selector contents")
			}
		})
	}
}

func TestRunSelectBackendRequiresExactArguments(t *testing.T) {
	for _, args := range [][]string{
		{"select-backend"},
		{"select-backend", "--config"},
		{"select-backend", "--config", ""},
		{"select-backend", "--config=secret"},
		{"select-backend", "--config", "one", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, bytes.NewReader(nil), &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.String() != "usage: proxypoolctl select-backend --config PATH\n" {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunConfigEnabledReportsOnlyEffectiveBoolean(t *testing.T) {
	tests := []struct {
		name       string
		contents   string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "legacy default enabled", contents: "config global 'global'\n\toption max_clients '60'\n\nconfig client 'old'\n\toption password 'enabled-secret'\n", wantStdout: "1\n"},
		{name: "legacy disabled", contents: "config global 'global'\n\toption enabled '0'\n\toption max_clients '60'\n", wantStdout: "0\n"},
		{name: "unknown", contents: "config mystery 'x'\n\toption password 'enabled-secret'\n", wantCode: 1, wantStderr: "configuration enabled-state inspection failed\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxypool")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"config-enabled", "--config", path}, bytes.NewReader(nil), &stdout, &stderr)
			if code != test.wantCode || stdout.String() != test.wantStdout || stderr.String() != test.wantStderr {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if bytes.Contains(stdout.Bytes(), []byte("enabled-secret")) || bytes.Contains(stderr.Bytes(), []byte("enabled-secret")) {
				t.Fatal("config-enabled leaked config contents")
			}
		})
	}
}

func TestRunConfigEnabledRequiresExactArguments(t *testing.T) {
	for _, args := range [][]string{
		{"config-enabled"},
		{"config-enabled", "--config"},
		{"config-enabled", "--config", ""},
		{"config-enabled", "--config=secret"},
		{"config-enabled", "--config", "one", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, bytes.NewReader(nil), &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.String() != "usage: proxypoolctl config-enabled --config PATH\n" {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunProcdStateCallsFilteredUbusQuery(t *testing.T) {
	var gotName string
	var gotArgs []string
	execute := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte(`{"proxypool":{"instances":{"main":{"running":true}}}}`), nil
	}

	var stdout, stderr bytes.Buffer
	code := runWithUbus([]string{"procd-state", "--service", "proxypool"}, bytes.NewReader(nil), &stdout, &stderr, execute)
	if code != 0 || stdout.String() != "present\n" || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if gotName != "/bin/ubus" || !reflect.DeepEqual(gotArgs, []string{"call", "service", "list", `{"name":"proxypool"}`}) {
		t.Fatalf("command=%q args=%q", gotName, gotArgs)
	}
}

func TestRunProcdStateRequiresExactArgumentsWithoutQuerying(t *testing.T) {
	tests := [][]string{
		{"procd-state"},
		{"procd-state", "--service"},
		{"procd-state", "--service", ""},
		{"procd-state", "--service=proxypool"},
		{"procd-state", "--service", "other"},
		{"procd-state", "--instance", "main", "--service", "proxypool"},
		{"procd-state", "--service", "proxypool", "--instance"},
		{"procd-state", "--service", "proxypool", "--instance", ""},
		{"procd-state", "--service", "proxypool", "--instance=main"},
		{"procd-state", "--service", "proxypool", "--instance", "main", "extra"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			execute := func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("invalid arguments reached ubus")
				return nil, nil
			}
			var stdout, stderr bytes.Buffer
			code := runWithUbus(args, bytes.NewReader(nil), &stdout, &stderr, execute)
			if code != 2 || stdout.Len() != 0 || stderr.String() != "usage: proxypoolctl procd-state --service proxypool [--instance TOKEN]\n" {
				t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunProcdStateReportsStructuralThreeState(t *testing.T) {
	tests := []struct {
		name       string
		instance   string
		response   string
		queryErr   error
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "service missing", response: `{}`, wantStdout: "absent\n"},
		{name: "instances empty", response: `{"proxypool":{"instances":{}}}`, wantStdout: "absent\n"},
		{name: "any instance present", response: "{\n\"proxypool\":{\"instances\":{\"main\":{\"running\":false}}}}\n", wantStdout: "present\n"},
		{name: "exact instance present", instance: "blue", response: `{"proxypool":{"instances":{"main":{},"blue":{"running":true}}}}`, wantStdout: "present\n"},
		{name: "exact instance absent", instance: "blue", response: `{"proxypool":{"instances":{"main":{"running":true}}}}`, wantStdout: "absent\n"},
		{name: "query error", queryErr: errors.New("ubus-secret-password"), wantCode: 1, wantStdout: "unknown\n", wantStderr: "procd state query failed\n"},
		{name: "second document", response: `{"proxypool":{"instances":{}}} {"password":"json-secret-password"}`, wantCode: 1, wantStdout: "unknown\n", wantStderr: "procd state query failed\n"},
		{name: "root malformed", response: `[]`, wantCode: 1, wantStdout: "unknown\n", wantStderr: "procd state query failed\n"},
		{name: "service malformed", response: `{"proxypool":null,"password":"json-secret-password"}`, wantCode: 1, wantStdout: "unknown\n", wantStderr: "procd state query failed\n"},
		{name: "instances missing", response: `{"proxypool":{"password":"json-secret-password"}}`, wantCode: 1, wantStdout: "unknown\n", wantStderr: "procd state query failed\n"},
		{name: "instances malformed", response: `{"proxypool":{"instances":[]}}`, wantCode: 1, wantStdout: "unknown\n", wantStderr: "procd state query failed\n"},
		{name: "instance value malformed", response: `{"proxypool":{"instances":{"main":null}}}`, wantCode: 1, wantStdout: "unknown\n", wantStderr: "procd state query failed\n"},
		{name: "non-target instance malformed", instance: "blue", response: `{"proxypool":{"instances":{"main":"json-secret-password","blue":{}}}}`, wantCode: 1, wantStdout: "unknown\n", wantStderr: "procd state query failed\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			execute := func(context.Context, string, ...string) ([]byte, error) {
				return []byte(test.response), test.queryErr
			}
			args := []string{"procd-state", "--service", "proxypool"}
			if test.instance != "" {
				args = append(args, "--instance", test.instance)
			}
			var stdout, stderr bytes.Buffer
			code := runWithUbus(args, bytes.NewReader(nil), &stdout, &stderr, execute)
			if code != test.wantCode || stdout.String() != test.wantStdout || stderr.String() != test.wantStderr {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			for _, secret := range []string{"ubus-secret-password", "json-secret-password"} {
				if bytes.Contains(stdout.Bytes(), []byte(secret)) || bytes.Contains(stderr.Bytes(), []byte(secret)) {
					t.Fatalf("procd-state leaked %q", secret)
				}
			}
		})
	}
}

func TestRunCallSeparatesResponseAndDiagnostics(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "pp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "ctl.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte(`{"version":1,"id":"cli","result":{"ok":true}}` + "\n"))
	}()
	var out, stderr bytes.Buffer
	code := run([]string{"call", "--socket", path}, bytes.NewBufferString(`{"version":1,"id":"cli","method":"status.get","params":{}}`), &out, &stderr)
	if code != 0 || stderr.Len() != 0 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
	var response map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil || response["id"] != "cli" {
		t.Fatalf("stdout invalid: %v %q", err, out.String())
	}
}

func TestRunCallRejectsMalformedAndOversizeWithoutEchoingInput(t *testing.T) {
	for _, input := range []string{`{"version":1,"id":"x","method":"status.get","params":{},"extra":1}`, `{"version":1,"id":"x","method":"status.get","params":{"password":"cli-secret"}}` + string(bytes.Repeat([]byte("x"), 1<<20))} {
		var out, stderr bytes.Buffer
		code := run([]string{"call"}, bytes.NewBufferString(input), &out, &stderr)
		if code == 0 || out.Len() != 0 || bytes.Contains(stderr.Bytes(), []byte("cli-secret")) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
		}
	}
}

func TestRunCallRequiresOneObject(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run([]string{"call"}, bytes.NewBufferString(`{"version":1,"id":"a","method":"status.get","params":{}} {}`), &out, &stderr)
	if code == 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
	_ = context.Background()
	_ = os.ErrNotExist
	_ = time.Second
}

func TestRunCallRejectsUnknownOption(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := run([]string{"call", "--bad"}, bytes.NewBufferString(`{}`), &out, &stderr); code == 0 || out.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
}

func TestRunCallAcceptsExactMaxPayloadWithTerminator(t *testing.T) {
	base := `{"version":1,"id":"max","method":"status.get","params":{"padding":"` + string(bytes.Repeat([]byte("x"), 64)) + `"}}`
	input := base + string(bytes.Repeat([]byte(" "), api.MaxFrameSize-len(base)))
	if len(input) != api.MaxFrameSize {
		t.Fatalf("payload size=%d", len(input))
	}
	for _, suffix := range []string{"\n", "\r\n"} {
		var out, stderr bytes.Buffer
		code := run([]string{"call", "--socket", "missing.sock"}, bytes.NewBufferString(input+suffix), &out, &stderr)
		if code != 1 || out.Len() != 0 || bytes.Contains(stderr.Bytes(), []byte("padding")) {
			t.Fatalf("suffix=%q code=%d stdout=%q stderr=%q", suffix, code, out.String(), stderr.String())
		}
	}
	var out, stderr bytes.Buffer
	if code := run([]string{"call"}, bytes.NewBufferString(input+"x\n"), &out, &stderr); code != 2 || out.Len() != 0 {
		t.Fatalf("oversize code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
}

type shortWriter struct{ limit int }

func (w shortWriter) Write(p []byte) (int, error) {
	if w.limit == 0 {
		return 0, nil
	}
	if len(p) > w.limit {
		return w.limit, nil
	}
	return len(p), nil
}

func TestRunFailsWhenStdoutCannotMakeProgress(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--version"}, bytes.NewReader(nil), shortWriter{}, &stderr); code == 0 {
		t.Fatal("version succeeded with zero-progress stdout")
	}
}
