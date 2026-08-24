package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/buildinfo"
	"proxypoold/internal/diagnostics"
)

func TestRunPreservesVersionAndStrictlyRequiresShadow(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{name: "version", args: []string{"--version"}, wantCode: 0, wantOut: buildinfo.Version + "\n"},
		{name: "missing shadow", args: nil, wantCode: 2},
		{name: "explicit false shadow", args: []string{"--shadow=false"}, wantCode: 2},
		{name: "unknown flag", args: []string{"--shadow", "--unknown"}, wantCode: 2},
		{name: "positional", args: []string{"--shadow", "extra"}, wantCode: 2},
		{name: "version cannot be combined", args: []string{"--version", "--shadow"}, wantCode: 2},
		{name: "single dash is rejected", args: []string{"-shadow"}, wantCode: 2},
		{name: "equals syntax is rejected", args: []string{"--shadow=true"}, wantCode: 2},
		{name: "duplicate shadow", args: []string{"--shadow", "--shadow"}, wantCode: 2},
		{name: "duplicate config", args: []string{"--shadow", "--config", "one", "--config", "two"}, wantCode: 2},
		{name: "empty config", args: []string{"--shadow", "--config", ""}, wantCode: 2},
		{name: "empty socket", args: []string{"--shadow", "--socket", ""}, wantCode: 2},
		{name: "strict valid form", args: []string{"--socket", "socket", "--shadow", "--config", "config"}, wantCode: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			code := run(ctx, test.args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d; stderr=%q", code, test.wantCode, stderr.String())
			}
			if stdout.String() != test.wantOut {
				t.Fatalf("stdout = %q, want %q", stdout.String(), test.wantOut)
			}
			if test.wantCode == 2 && !strings.Contains(stderr.String(), "usage:") {
				t.Fatalf("strict flag error did not print usage: %q", stderr.String())
			}
		})
	}
}

func TestLiveControlMethodsExposeTypedNodeMutations(t *testing.T) {
	methods := liveControlMethods()
	for _, method := range []string{"node.save", "node.delete", "node.action", "device.bindings.replace", "diagnostics.create", "diagnostics.get", "diagnostics.claim", "diagnostics.release"} {
		if _, exists := methods[method]; !exists {
			t.Fatalf("live daemon method allowlist omitted %q", method)
		}
	}
}

func TestDefaultDiagnosticCommandsAreFixedBoundedInputs(t *testing.T) {
	commands := defaultDiagnosticCommands()
	if len(commands) < 6 || len(commands) > 32 {
		t.Fatalf("commands = %d", len(commands))
	}
	seen := map[string]bool{}
	for _, command := range commands {
		if !strings.HasPrefix(command.Path, "/") || strings.Contains(command.Path, "sh") {
			t.Fatalf("unsafe command = %#v", command)
		}
		if seen[command.Name] {
			t.Fatalf("duplicate command entry %q", command.Name)
		}
		seen[command.Name] = true
		for _, argument := range command.Args {
			if strings.ContainsAny(argument, ";|`$") {
				t.Fatalf("shell-like argument = %q", argument)
			}
		}
	}
	fullLog, exists := func() (diagnostics.Command, bool) {
		for _, command := range commands {
			if command.Name == "recent-system-log.txt" {
				return command, true
			}
		}
		return diagnostics.Command{}, false
	}()
	if !exists {
		t.Fatal("bounded full system log is absent from diagnostics")
	}
	if fullLog.Path != "/sbin/logread" || strings.Join(fullLog.Args, " ") != "-l 1000" {
		t.Fatalf("full log command = %#v", fullLog)
	}
}

func TestRunStrictlySelectsShadowOrLiveMode(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "live complete", args: []string{"--live", "--config", "config", "--state", "state", "--socket", "socket"}, wantCode: 0},
		{name: "live default config", args: []string{"--live", "--state", "state"}, wantCode: 0},
		{name: "live missing state", args: []string{"--live"}, wantCode: 2},
		{name: "both modes", args: []string{"--shadow", "--live", "--state", "state"}, wantCode: 2},
		{name: "state forbidden in shadow", args: []string{"--shadow", "--state", "state"}, wantCode: 2},
		{name: "duplicate live", args: []string{"--live", "--live", "--state", "state"}, wantCode: 2},
		{name: "duplicate state", args: []string{"--live", "--state", "one", "--state", "two"}, wantCode: 2},
		{name: "empty state", args: []string{"--live", "--state", ""}, wantCode: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stdout, stderr bytes.Buffer
			code := run(ctx, test.args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("code = %d, want %d; stderr=%q", code, test.wantCode, stderr.String())
			}
			if test.wantCode == 2 && !strings.Contains(stderr.String(), "usage:") {
				t.Fatalf("invalid mode did not print usage: %q", stderr.String())
			}
		})
	}
}

func TestRunReservesLiveEndpointBeforeReadingConfiguration(t *testing.T) {
	directory := t.TempDir()
	socketPath := filepath.Join(directory, "proxypoold.sock")
	lease, err := api.AcquireEndpointLease(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--live", "--config", filepath.Join(directory, "missing-config"),
		"--state", filepath.Join(directory, "runtime.json"), "--socket", socketPath,
	}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || stderr.String() != "proxypoold: control endpoint is already owned\n" {
		t.Fatalf("run result = %d / %q / %q", code, stdout.String(), stderr.String())
	}
}

func TestRunServesOnlyStatusAndShutsDownAfterCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go's Unix socket server is not available on Windows")
	}
	configPath := filepath.Join(t.TempDir(), "proxypool")
	if err := os.WriteFile(configPath, []byte(daemonEmptyV2Config()), 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(t.TempDir(), "proxypoold.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- run(ctx, []string{"--shadow", "--config", configPath, "--socket", socketPath}, &stdout, &stderr)
	}()

	client := &api.Client{Path: socketPath, Timeout: 200 * time.Millisecond}
	status := callDaemonEventually(t, client, api.Request{Version: api.ProtocolVersion, ID: "status", Method: "status.get", Params: json.RawMessage(`{}`)})
	if status.Error != nil || !bytes.Contains(status.Result, []byte(`"state":"ready"`)) {
		t.Fatalf("status response = %#v", status)
	}
	mutation := callDaemonEventually(t, client, api.Request{Version: api.ProtocolVersion, ID: "mutate", Method: "node.save", Params: json.RawMessage(`{}`)})
	if mutation.Error == nil || mutation.Error.Code != "unknown_method" {
		t.Fatalf("mutation response = %#v", mutation)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run returned %d; stderr=%q", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not complete bounded shutdown")
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket remained after shutdown: %v", err)
	}
}

func TestRunKeepsMigrationAndInvalidConfigQueryable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go's Unix socket server is not available on Windows")
	}
	tests := []struct {
		name     string
		contents string
		want     string
		secret   string
	}{
		{name: "legacy", contents: "config global 'global'\n\toption enabled '1'\n\toption max_clients '60'\n\nconfig client 'old'\n\toption password 'legacy-daemon-secret'\n", want: "migration_required", secret: "legacy-daemon-secret"},
		{name: "invalid", contents: "config global 'global'\n\toption schema_version '2'\n\toption revision '1'\n\toption password 'invalid-daemon-secret'\n", want: "invalid_config", secret: "invalid-daemon-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "proxypool")
			if err := os.WriteFile(configPath, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			socketPath := filepath.Join(t.TempDir(), "proxypoold.sock")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan int, 1)
			var stdout, stderr bytes.Buffer
			go func() {
				done <- run(ctx, []string{"--shadow", "--config", configPath, "--socket", socketPath}, &stdout, &stderr)
			}()
			client := &api.Client{Path: socketPath, Timeout: 200 * time.Millisecond}
			response := callDaemonEventually(t, client, api.Request{Version: api.ProtocolVersion, ID: "status", Method: "status.get", Params: json.RawMessage(`{}`)})
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if response.Error != nil || !bytes.Contains(encoded, []byte(`"state":"`+test.want+`"`)) {
				t.Fatalf("response = %s", encoded)
			}
			if bytes.Contains(encoded, []byte(test.secret)) || strings.Contains(stderr.String(), test.secret) || strings.Contains(stdout.String(), test.secret) {
				t.Fatal("daemon status/log output leaked a configuration secret")
			}
			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("run returned %d; stderr=%q", code, stderr.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("daemon did not stop")
			}
		})
	}
}

func TestLiveServiceResultDoesNotHideCleanupFailureOnShutdown(t *testing.T) {
	if err := liveServiceResult(context.Canceled, nil, errors.New("session stop failed"), nil); err == nil {
		t.Fatal("signal cancellation hid a live cleanup failure")
	}
	if err := liveServiceResult(context.Canceled, nil, nil, nil); err != nil {
		t.Fatalf("clean signal shutdown returned %v", err)
	}
}

func callDaemonEventually(t *testing.T, client *api.Client, request api.Request) api.Response {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Call(context.Background(), request)
		if err == nil {
			return response
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon control socket did not become queryable")
	return api.Response{}
}

func daemonEmptyV2Config() string {
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
