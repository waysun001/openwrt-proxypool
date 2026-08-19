package openwrt

import (
	"context"
	"errors"
	"testing"
)

func TestWANStatusSourceRequiresAuthoritativeReadyIPv4Uplink(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		runnerErr error
		want      bool
		wantErr   bool
	}{
		{name: "ready", response: `{"up":true,"pending":false,"available":true,"l3_device":"eth1","ipv4-address":[{"address":"203.0.113.9","mask":24}]}`, want: true},
		{name: "down", response: `{"up":false,"pending":false,"available":true,"l3_device":"eth1","ipv4-address":[{"address":"203.0.113.9","mask":24}]}`},
		{name: "pending", response: `{"up":true,"pending":true,"available":true,"l3_device":"eth1","ipv4-address":[{"address":"203.0.113.9","mask":24}]}`},
		{name: "no-ipv4", response: `{"up":true,"pending":false,"available":true,"l3_device":"eth1","ipv4-address":[]}`},
		{name: "loopback-only", response: `{"up":true,"pending":false,"available":true,"l3_device":"eth1","ipv4-address":[{"address":"127.0.0.1","mask":8}]}`},
		{name: "malformed", response: `{`, wantErr: true},
		{name: "ubus-error", runnerErr: errors.New("ubus unavailable"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &wanRunner{response: []byte(test.response), err: test.runnerErr}
			source := NewWANStatusSource(runner)
			got, err := source.Available(context.Background())
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("Available() = %t, %v; want %t, error=%t", got, err, test.want, test.wantErr)
			}
			if runner.path != "/bin/ubus" || len(runner.args) != 3 || runner.args[0] != "call" ||
				runner.args[1] != "network.interface.wan" || runner.args[2] != "status" {
				t.Fatalf("runner call = %q %v", runner.path, runner.args)
			}
		})
	}
}

type wanRunner struct {
	response []byte
	err      error
	path     string
	args     []string
}

func (runner *wanRunner) Run(_ context.Context, path string, args ...string) ([]byte, error) {
	runner.path = path
	runner.args = append([]string(nil), args...)
	return append([]byte(nil), runner.response...), runner.err
}
