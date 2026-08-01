package openwrt

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestRunnerPassesInjectionShapedValuesAsOneArgvElement(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := newRunner(time.Second, 1024, func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte("ok"), nil
	})
	value := `dhcp.proxypool_device.name=$(touch /tmp/pwned); echo secret`
	output, err := runner.Run(context.Background(), "/sbin/uci", "set", value)
	if err != nil || string(output) != "ok" {
		t.Fatalf("Run() output=%q error=%v", output, err)
	}
	if gotName != "/sbin/uci" || !reflect.DeepEqual(gotArgs, []string{"set", value}) {
		t.Fatalf("executor received name=%q args=%q", gotName, gotArgs)
	}
}

func TestRunnerRejectsRelativeExecutableNULAndOversizedOutput(t *testing.T) {
	calls := 0
	runner := newRunner(time.Second, 4, func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		calls++
		return []byte("12345"), nil
	})
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "relative", args: []string{"uci", "show"}},
		{name: "nul", args: []string{"/sbin/uci", "bad\x00arg"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := runner.Run(context.Background(), test.args[0], test.args[1:]...); err == nil {
				t.Fatal("Run() error = nil")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid argv reached executor %d times", calls)
	}
	if _, err := runner.Run(context.Background(), "/sbin/uci", "show"); err == nil {
		t.Fatal("oversized output was accepted")
	}
}

func TestRunnerAppliesDeadlineAndDoesNotExposeExecutorError(t *testing.T) {
	runner := newRunner(10*time.Millisecond, 1024, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, errors.New("password=DO-NOT-LEAK")
	})
	started := time.Now()
	_, err := runner.Run(context.Background(), "/sbin/uci", "show", "dhcp")
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("Run() error=%v elapsed=%s", err, time.Since(started))
	}
	if bytes.Contains([]byte(err.Error()), []byte("DO-NOT-LEAK")) {
		t.Fatalf("Run() leaked executor error: %v", err)
	}
}
