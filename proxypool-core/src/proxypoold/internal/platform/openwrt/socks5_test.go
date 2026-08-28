package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
	socksprotocol "proxypoold/internal/socks5"
)

func TestSOCKS5AdapterStartsPrivateTCPOnlyProcessAndProbesExactEndpoint(t *testing.T) {
	fixture := newSOCKS5Fixture(t)
	request := validSOCKS5Request()
	var probed []string
	adapter := fixture.adapter(func(_ context.Context, proxyAddress, username, password, target string) error {
		probed = []string{proxyAddress, username, password, target}
		return nil
	})

	session, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if session.NodeID != request.Node.ID || session.Generation != request.Generation || session.Protocol != model.ProtocolSOCKS5 ||
		session.Interface != "psx0001" || session.LocalPort != 12001 || session.OwnershipDigest == "" {
		t.Fatalf("session = %#v", session)
	}
	configPath := filepath.Join(fixture.stateDir, request.Node.ID, "redsocks.conf")
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []string{
		"local_ip = 192.168.9.1;", "local_port = 12001;", "ip = 203.0.113.7;", "port = 1080;", "type = socks5;",
		`login = "u\"ser";`, `password = "p\\ass";`,
	}
	for _, line := range wantLines {
		if !strings.Contains(string(contents), line) {
			t.Fatalf("config missing %q:\n%s", line, contents)
		}
	}
	for _, forbidden := range []string{"redudp", "type = direct", "direct {", "0.0.0.0", "udp_timeout"} {
		if strings.Contains(string(contents), forbidden) {
			t.Fatalf("config contains forbidden %q:\n%s", forbidden, contents)
		}
	}
	if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, %v", info, err)
	}
	pidPath := filepath.Join(fixture.stateDir, request.Node.ID, "redsocks.pid")
	if info, err := os.Stat(pidPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("PID mode = %v, %v", info, err)
	}
	if err := adapter.Probe(context.Background(), request, session); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	wantProbe := []string{"203.0.113.7:1080", `u"ser`, `p\ass`, "223.5.5.5:443"}
	if fmt.Sprint(probed) != fmt.Sprint(wantProbe) {
		t.Fatalf("probe = %#v, want %#v", probed, wantProbe)
	}

	manifestPath := filepath.Join(fixture.stateDir, request.Node.ID, "ownership.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil || !json.Valid(manifest) {
		t.Fatalf("manifest = %s, %v", manifest, err)
	}
	for _, secret := range []string{request.Node.Username, request.Node.Password} {
		if strings.Contains(string(manifest), secret) {
			t.Fatalf("manifest leaked a credential: %s", manifest)
		}
	}
	if info, err := os.Stat(manifestPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %v, %v", info, err)
	}
	for _, call := range fixture.runner.callsSnapshot() {
		if strings.Contains(call, request.Node.Username) || strings.Contains(call, request.Node.Password) {
			t.Fatalf("command argv leaked credentials: %q", call)
		}
	}
	if err := adapter.Stop(context.Background(), request, session); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.procRoot, strconv.Itoa(fixture.runner.pid))); !os.IsNotExist(err) {
		t.Fatalf("owned process still exists: %v", err)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("ownership manifest still exists: %v", err)
	}
}

func TestSOCKS5AdapterClassifiesProbeFailuresWithoutLeakingSecrets(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{name: "authentication", code: socksprotocol.CodeAuthentication, want: "auth_failed"},
		{name: "resolution", code: socksprotocol.CodeResolve, want: "resolve_failed"},
		{name: "timeout", code: socksprotocol.CodeTimeout, want: "connect_timeout"},
		{name: "connect", code: socksprotocol.CodeConnect, want: "probe_failed"},
		{name: "protocol", code: socksprotocol.CodeProtocol, want: "probe_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSOCKS5Fixture(t)
			adapter := fixture.adapter(func(context.Context, string, string, string, string) error {
				return &socksprotocol.Error{Code: test.code, Message: "safe failure"}
			})
			request := validSOCKS5Request()
			session, err := adapter.Start(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			err = adapter.Probe(context.Background(), request, session)
			var coded *model.CodeError
			if !errors.As(err, &coded) || coded.Code != test.want {
				t.Fatalf("Probe() error = %v", err)
			}
			if strings.Contains(err.Error(), request.Node.Username) || strings.Contains(err.Error(), request.Node.Password) {
				t.Fatalf("Probe() leaked credentials: %v", err)
			}
		})
	}
}

func TestSOCKS5AdapterRefusesReusedPIDAndNeverKillsIt(t *testing.T) {
	fixture := newSOCKS5Fixture(t)
	adapter := fixture.adapter(func(context.Context, string, string, string, string) error { return nil })
	request := validSOCKS5Request()
	session, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runner.rewriteProcessStartTime(t, 999999)

	if err := adapter.Probe(context.Background(), request, session); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("Probe() error = %v", err)
	}
	callsBefore := len(fixture.runner.callsSnapshot())
	if err := adapter.Stop(context.Background(), request, session); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("Stop() error = %v", err)
	}
	for _, call := range fixture.runner.callsSnapshot()[callsBefore:] {
		if strings.Contains(call, "/bin/kill") {
			t.Fatalf("Stop() killed an unowned PID: %q", call)
		}
	}
}

func TestSOCKS5AdapterTreatsDeadOwnedProcessAsIdempotentlyStopped(t *testing.T) {
	fixture := newSOCKS5Fixture(t)
	adapter := fixture.adapter(func(context.Context, string, string, string, string) error { return nil })
	request := validSOCKS5Request()
	session, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(fixture.procRoot, strconv.Itoa(fixture.runner.pid))); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stop(context.Background(), request, session); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, request.Node.ID, "ownership.json")); !os.IsNotExist(err) {
		t.Fatalf("stale manifest was not removed: %v", err)
	}
}

func TestSOCKS5AdapterDoesNotEraseLiveProcessWhenOwnershipManifestIsMissing(t *testing.T) {
	fixture := newSOCKS5Fixture(t)
	adapter := fixture.adapter(func(context.Context, string, string, string, string) error { return nil })
	request := validSOCKS5Request()
	session, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	nodeDir := filepath.Join(fixture.stateDir, request.Node.ID)
	if err := os.Remove(filepath.Join(nodeDir, "ownership.json")); err != nil {
		t.Fatal(err)
	}
	callsBefore := len(fixture.runner.callsSnapshot())
	if err := adapter.Stop(context.Background(), request, session); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("Stop() error = %v", err)
	}
	for _, call := range fixture.runner.callsSnapshot()[callsBefore:] {
		if strings.Contains(call, "/bin/kill") {
			t.Fatalf("Stop() killed a process without ownership: %q", call)
		}
	}
	for _, name := range []string{"redsocks.conf", "redsocks.pid"} {
		if _, err := os.Stat(filepath.Join(nodeDir, name)); err != nil {
			t.Fatalf("Stop() erased %s without ownership: %v", name, err)
		}
	}
}

func TestSOCKS5AdapterEscalatesOwnedStubbornProcessToKILL(t *testing.T) {
	fixture := newSOCKS5Fixture(t)
	fixture.runner.stubborn = true
	adapter := NewSOCKS5Adapter(fixture.runner, fixture.resolver, fixture.stateDir,
		WithSOCKS5ProcRoot(fixture.procRoot), WithSOCKS5BootIDPath(fixture.bootID),
		WithSOCKS5PollInterval(time.Millisecond), WithSOCKS5TerminateGrace(3*time.Millisecond),
		WithSOCKS5Probe(func(context.Context, string, string, string, string) error { return nil }),
	)
	request := validSOCKS5Request()
	session, err := adapter.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := adapter.Stop(ctx, request, session); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	calls := strings.Join(fixture.runner.callsSnapshot(), "\n")
	if !strings.Contains(calls, "/bin/kill -TERM 4321") || !strings.Contains(calls, "/bin/kill -KILL 4321") {
		t.Fatalf("termination calls =\n%s", calls)
	}
}

func validSOCKS5Request() platform.NodeRequest {
	return platform.NodeRequest{
		Node: model.Node{
			ID: "node_a", Name: "Proxy", Protocol: model.ProtocolSOCKS5, Enabled: true,
			Server: "proxy.example", Port: 1080, Username: `u"ser`, Password: `p\ass`, PolicyID: 1, Revision: 7,
		},
		JobID: "job_a", Generation: 3,
	}
}

type socks5Fixture struct {
	stateDir string
	procRoot string
	bootID   string
	runner   *socks5Runner
	resolver *socks5Resolver
}

func newSOCKS5Fixture(t *testing.T) *socks5Fixture {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	procRoot := filepath.Join(root, "proc")
	if err := os.MkdirAll(filepath.Join(procRoot, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	bootID := filepath.Join(root, "boot_id")
	if err := os.WriteFile(bootID, []byte("boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &socks5Runner{t: t, procRoot: procRoot, pid: 4321, startTime: 424242}
	return &socks5Fixture{
		stateDir: stateDir, procRoot: procRoot, bootID: bootID, runner: runner,
		resolver: &socks5Resolver{address: netip.MustParseAddr("203.0.113.7")},
	}
}

func (fixture *socks5Fixture) adapter(probe SOCKS5Probe) *SOCKS5Adapter {
	return NewSOCKS5Adapter(fixture.runner, fixture.resolver, fixture.stateDir,
		WithSOCKS5ProcRoot(fixture.procRoot), WithSOCKS5BootIDPath(fixture.bootID),
		WithSOCKS5PollInterval(time.Millisecond), WithSOCKS5Probe(probe),
	)
}

type socks5Resolver struct {
	address netip.Addr
	err     error
}

func (resolver *socks5Resolver) ResolveIPv4(context.Context, string) (netip.Addr, error) {
	return resolver.address, resolver.err
}

type socks5Runner struct {
	t         *testing.T
	procRoot  string
	pid       int
	startTime uint64
	stubborn  bool

	mu    sync.Mutex
	calls []string
}

func (runner *socks5Runner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.mu.Lock()
	runner.calls = append(runner.calls, strings.Join(append([]string{name}, args...), " "))
	runner.mu.Unlock()
	switch name {
	case "/usr/sbin/redsocks":
		configPath, pidPath, ok := redsocksPaths(args)
		if !ok {
			return nil, errors.New("invalid redsocks invocation")
		}
		if err := os.WriteFile(pidPath, []byte(strconv.Itoa(runner.pid)+"\n"), 0o644); err != nil {
			return nil, err
		}
		if err := runner.writeProcess(configPath, pidPath, runner.startTime); err != nil {
			return nil, err
		}
		return nil, nil
	case "/bin/kill":
		if len(args) != 2 || (args[0] != "-TERM" && args[0] != "-KILL") || args[1] != strconv.Itoa(runner.pid) {
			return nil, errors.New("invalid kill invocation")
		}
		if args[0] == "-TERM" && runner.stubborn {
			return nil, nil
		}
		return nil, os.RemoveAll(filepath.Join(runner.procRoot, strconv.Itoa(runner.pid)))
	default:
		return nil, errors.New("unexpected command")
	}
}

func (runner *socks5Runner) RunInput(ctx context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	return runner.Run(ctx, name, args...)
}

func (runner *socks5Runner) callsSnapshot() []string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]string(nil), runner.calls...)
}

func (runner *socks5Runner) writeProcess(configPath, pidPath string, startTime uint64) error {
	directory := filepath.Join(runner.procRoot, strconv.Itoa(runner.pid))
	if err := os.MkdirAll(filepath.Join(directory, "fd"), 0o700); err != nil {
		return err
	}
	if err := os.Symlink("/usr/sbin/redsocks", filepath.Join(directory, "exe")); err != nil {
		return err
	}
	cmdline := strings.Join([]string{"/usr/sbin/redsocks", "-c", configPath, "-p", pidPath, ""}, "\x00")
	if err := os.WriteFile(filepath.Join(directory, "cmdline"), []byte(cmdline), 0o600); err != nil {
		return err
	}
	if err := runner.rewriteProcessStartTime(runner.t, startTime); err != nil {
		return err
	}
	if err := os.Symlink("socket:[12345]", filepath.Join(directory, "fd", "3")); err != nil {
		return err
	}
	line := "  0: 0109A8C0:2EE1 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 12345 1\n"
	return os.WriteFile(filepath.Join(runner.procRoot, "net", "tcp"), []byte(line), 0o600)
}

func (runner *socks5Runner) rewriteProcessStartTime(t *testing.T, startTime uint64) error {
	t.Helper()
	stat := fmt.Sprintf("%d (redsocks) S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 %d 0\n", runner.pid, startTime)
	return os.WriteFile(filepath.Join(runner.procRoot, strconv.Itoa(runner.pid), "stat"), []byte(stat), 0o600)
}

func redsocksPaths(args []string) (string, string, bool) {
	if len(args) != 4 || args[0] != "-c" || args[2] != "-p" {
		return "", "", false
	}
	return args[1], args[3], true
}
