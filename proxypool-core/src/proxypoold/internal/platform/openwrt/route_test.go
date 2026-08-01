package openwrt

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"proxypoold/internal/platform"
)

func TestRouteManagerInstallsAndVerifiesExactOwnedPolicyPath(t *testing.T) {
	runner := newRouteRunner()
	manager := NewRouteManager(runner)
	lease := platform.RouteLease{NodeID: "node_a", PolicyID: 42, Generation: 9, Interface: "l2tp-ppv20042"}
	if err := manager.Install(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/sbin/ip -4 -j rule show", "/sbin/ip -4 -j route show table 100042",
		"/sbin/ip -4 route add table 100042 default dev l2tp-ppv20042 proto 186",
		"/sbin/ip -4 rule add pref 200042 fwmark 0x005a002a/0x00ffffff lookup 100042",
		"/sbin/ip -4 -j rule show", "/sbin/ip -4 -j route show table 100042",
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("route calls:\n got %q\nwant %q", runner.calls, want)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " main") {
			t.Fatalf("main-table fallback was used: %s", call)
		}
	}
}

func TestRouteManagerRejectsForeignTargetTableBeforeMutation(t *testing.T) {
	runner := newRouteRunner()
	runner.routes = `[ {"dst":"default","dev":"eth1","table":100042,"protocol":4} ]`
	err := NewRouteManager(runner).Install(context.Background(), platform.RouteLease{NodeID: "node_a", PolicyID: 42, Generation: 1, Interface: "l2tp-ppv20042"})
	if err == nil {
		t.Fatal("foreign route table was accepted")
	}
	if len(runner.calls) != 2 {
		t.Fatalf("foreign table was mutated: %q", runner.calls)
	}
}

func TestRouteManagerRollsBackRouteWhenRuleInstallFails(t *testing.T) {
	runner := newRouteRunner()
	runner.failContains = "rule add"
	err := NewRouteManager(runner).Install(context.Background(), platform.RouteLease{NodeID: "node_a", PolicyID: 42, Generation: 1, Interface: "l2tp-ppv20042"})
	if err == nil {
		t.Fatal("rule failure was accepted")
	}
	last := runner.calls[len(runner.calls)-1]
	if last != "/sbin/ip -4 route del table 100042 default dev l2tp-ppv20042 proto 186" {
		t.Fatalf("partial route was not rolled back: %q", runner.calls)
	}
}

func TestRouteManagerRejectsInvalidLeaseBeforeCommands(t *testing.T) {
	runner := newRouteRunner()
	err := NewRouteManager(runner).Install(context.Background(), platform.RouteLease{NodeID: "node_a", PolicyID: 0, Generation: 1, Interface: `ppp0;reboot`})
	if err == nil || len(runner.calls) != 0 {
		t.Fatalf("invalid lease error/calls = %v/%q", err, runner.calls)
	}
}

type routeRunner struct {
	calls        []string
	rules        string
	routes       string
	installed    bool
	failContains string
}

func newRouteRunner() *routeRunner { return &routeRunner{rules: "[]", routes: "[]"} }

func (runner *routeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, call)
	if runner.failContains != "" && strings.Contains(call, runner.failContains) {
		return nil, errors.New("injected command failure")
	}
	switch call {
	case "/sbin/ip -4 -j rule show":
		if runner.installed {
			return []byte(`[{"priority":200042,"fwmark":"0x5a002a","fwmask":"0xffffff","table":100042}]`), nil
		}
		return []byte(runner.rules), nil
	case "/sbin/ip -4 -j route show table 100042":
		if runner.installed {
			return []byte(`[{"dst":"default","dev":"l2tp-ppv20042","table":100042,"protocol":186,"scope":"link"}]`), nil
		}
		return []byte(runner.routes), nil
	case "/sbin/ip -4 rule add pref 200042 fwmark 0x005a002a/0x00ffffff lookup 100042":
		runner.installed = true
		return nil, nil
	case "/sbin/ip -4 route add table 100042 default dev l2tp-ppv20042 proto 186",
		"/sbin/ip -4 route del table 100042 default dev l2tp-ppv20042 proto 186":
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", call)
	}
}
