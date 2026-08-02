package diagnostics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCollectorRedactsAndBoundsEveryEntry(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"/bin/ubus": []byte(`{"password":"command-secret","state":"online"}`),
		"/sbin/ip":  []byte(strings.Repeat("x", MaxEntryBytes+128)),
	}}
	collector := NewCollector(runner, NewRedactor([]string{"command-secret"}), []Command{
		{Name: "ubus-network.json", Path: "/bin/ubus", Args: []string{"call", "network.interface", "dump"}},
		{Name: "ip-rules.txt", Path: "/sbin/ip", Args: []string{"-4", "rule", "show"}},
	})
	entries, err := collector.Collect(context.Background(), map[string][]byte{
		"status.json": []byte(`{"token":"seed-secret","state":"ready"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := entryMap(entries)
	if strings.Contains(string(byName["ubus-network.json"]), "command-secret") || strings.Contains(string(byName["status.json"]), "seed-secret") {
		t.Fatalf("collector leaked a secret: %#v", byName)
	}
	if len(byName["ip-rules.txt"]) > MaxEntryBytes || !strings.Contains(string(byName["ip-rules.txt"]), TruncationMarker) {
		t.Fatalf("oversized entry was not bounded: %d bytes", len(byName["ip-rules.txt"]))
	}
	if totalEntryBytes(entries) > MaxBundleBytes {
		t.Fatalf("bundle exceeded total limit: %d", totalEntryBytes(entries))
	}
}

func TestCollectorAppliesPerCommandDeadlineAndContinues(t *testing.T) {
	runner := &fakeRunner{run: func(ctx context.Context, path string, _ ...string) ([]byte, error) {
		if path == "/slow" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []byte("ok"), nil
	}}
	collector := NewCollector(runner, NewRedactor(nil), []Command{
		{Name: "slow.txt", Path: "/slow"},
		{Name: "next.txt", Path: "/next"},
	}, WithCommandTimeout(20*time.Millisecond))
	entries, err := collector.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := entryMap(entries)
	if !strings.Contains(string(byName["slow.txt"]), "collection_timeout") || string(byName["next.txt"]) != "ok" {
		t.Fatalf("collector did not isolate timeout: %#v", byName)
	}
}

func TestCollectorRejectsUnsafeCommandAndEntryNames(t *testing.T) {
	for _, commands := range [][]Command{
		{{Name: "../escape", Path: "/bin/true"}},
		{{Name: "safe.txt", Path: "relative"}},
		{{Name: "safe.txt", Path: "/bin/../tmp/evil"}},
		{{Name: "safe.txt", Path: "/bin/true", Args: []string{"bad\x00arg"}}},
	} {
		collector := NewCollector(&fakeRunner{}, NewRedactor(nil), commands)
		if _, err := collector.Collect(context.Background(), nil); err == nil {
			t.Fatalf("unsafe command accepted: %#v", commands)
		}
	}
	collector := NewCollector(&fakeRunner{}, NewRedactor(nil), nil)
	if _, err := collector.Collect(context.Background(), map[string][]byte{"../escape": []byte("x")}); err == nil {
		t.Fatal("unsafe seed entry accepted")
	}
}

func TestCollectorStopsOnParentCancellation(t *testing.T) {
	runs := 0
	runner := &fakeRunner{run: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		runs++
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	collector := NewCollector(runner, NewRedactor(nil), []Command{
		{Name: "first.txt", Path: "/first"},
		{Name: "second.txt", Path: "/second"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collector.Collect(ctx, map[string][]byte{"seed.txt": []byte("seed")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Collect error = %v, want context cancellation", err)
	}
	if runs > 1 {
		t.Fatalf("collector ran %d commands after cancellation", runs)
	}
}

func TestCollectorRejectsUnboundedSourceCountsBeforeRunningCommands(t *testing.T) {
	runs := 0
	runner := &fakeRunner{run: func(context.Context, string, ...string) ([]byte, error) {
		runs++
		return nil, nil
	}}
	commands := make([]Command, MaxCommands+1)
	for index := range commands {
		commands[index] = Command{Name: "entry-" + strings.Repeat("x", index%4) + string(rune('a'+index)), Path: "/bin/true"}
	}
	if _, err := NewCollector(runner, NewRedactor(nil), commands).Collect(context.Background(), nil); err == nil {
		t.Fatal("unbounded command list accepted")
	}
	if runs != 0 {
		t.Fatalf("collector ran %d commands before rejecting the source count", runs)
	}
}

type fakeRunner struct {
	outputs map[string][]byte
	run     func(context.Context, string, ...string) ([]byte, error)
}

func (runner *fakeRunner) RunBounded(ctx context.Context, limit int, path string, args ...string) ([]byte, bool, error) {
	if runner.run != nil {
		output, err := runner.run(ctx, path, args...)
		if len(output) > limit {
			return append([]byte(nil), output[:limit]...), true, err
		}
		return output, false, err
	}
	if output, ok := runner.outputs[path]; ok {
		if len(output) > limit {
			return append([]byte(nil), output[:limit]...), true, nil
		}
		return append([]byte(nil), output...), false, nil
	}
	return nil, false, errors.New("missing fixture")
}

func entryMap(entries []Entry) map[string][]byte {
	result := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		result[entry.Name] = entry.Data
	}
	return result
}

func totalEntryBytes(entries []Entry) int {
	total := 0
	for _, entry := range entries {
		total += len(entry.Data)
	}
	return total
}
