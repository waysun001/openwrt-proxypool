package openwrt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestProtocolTrafficReaderUsesSysfsForL2TPAndCachedNftCountersForSOCKS5(t *testing.T) {
	root := t.TempDir()
	writeTrafficCounters(t, root, "l2tp-ppv20001", "123\n", "456\n")
	runner := &protocolTrafficRunner{output: proxyCounterJSON(100, 300, 40, 60)}
	reader := NewProtocolTrafficReader(root, runner)

	l2tp, err := reader.ReadInterfaceCounters("l2tp-ppv20001")
	if err != nil || l2tp.RXBytes != 123 || l2tp.TXBytes != 456 {
		t.Fatalf("L2TP counters = %#v, error = %v", l2tp, err)
	}
	proxy2, err := reader.ReadInterfaceCounters("psx0002")
	if err != nil || proxy2.RXBytes != 300 || proxy2.TXBytes != 100 {
		t.Fatalf("SOCKS5 policy 2 counters = %#v, error = %v", proxy2, err)
	}
	proxy3, err := reader.ReadInterfaceCounters("psx0003")
	if err != nil || proxy3.RXBytes != 60 || proxy3.TXBytes != 40 {
		t.Fatalf("SOCKS5 policy 3 counters = %#v, error = %v", proxy3, err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("nft table calls = %d", runner.callCount())
	}
}

func TestProtocolTrafficReaderKeepsCountersMonotonicAcrossLeaseRefresh(t *testing.T) {
	runner := &protocolTrafficRunner{output: proxyCounterJSON(100, 300, 0, 0)}
	reader := NewProtocolTrafficReader(t.TempDir(), runner)
	now := time.Unix(1_700_000_000, 0)
	reader.now = func() time.Time { return now }
	reader.cacheTTL = time.Second

	first, err := reader.ReadInterfaceCounters("psx0002")
	if err != nil || first.RXBytes != 300 || first.TXBytes != 100 {
		t.Fatalf("first counters = %#v, error = %v", first, err)
	}
	runner.setOutput(proxyCounterJSON(10, 20, 0, 0))
	now = now.Add(2 * time.Second)
	reset, err := reader.ReadInterfaceCounters("psx0002")
	if err != nil || reset.RXBytes != 320 || reset.TXBytes != 110 {
		t.Fatalf("reset counters = %#v, error = %v", reset, err)
	}
	runner.setOutput(proxyCounterJSON(30, 70, 0, 0))
	now = now.Add(2 * time.Second)
	grown, err := reader.ReadInterfaceCounters("psx0002")
	if err != nil || grown.RXBytes != 370 || grown.TXBytes != 130 {
		t.Fatalf("grown counters = %#v, error = %v", grown, err)
	}
}

func TestProtocolTrafficReaderRejectsMalformedEvidenceAndUnsafeNames(t *testing.T) {
	runner := &protocolTrafficRunner{output: []byte(`{"nftables":[{"set":{"name":"v2_proxy_uploads","elem":[{"elem":{"val":{"concat":["aa:bb:cc:dd:ee:01","192.168.9.22","not-a-mark"]},"counter":{"bytes":10}}}]}}]}`)}
	reader := NewProtocolTrafficReader(t.TempDir(), runner)
	for _, name := range []string{"psx0000", "psx0061", "psx2", "psx0002;reboot"} {
		if _, err := reader.ReadInterfaceCounters(name); err == nil {
			t.Fatalf("unsafe interface %q was accepted", name)
		}
	}
	if _, err := reader.ReadInterfaceCounters("psx0002"); err == nil {
		t.Fatal("malformed nft counter evidence was accepted")
	}
	runner.setError(errors.New("nft unavailable"))
	reader.cachedAt = time.Time{}
	if _, err := reader.ReadInterfaceCounters("psx0002"); err == nil {
		t.Fatal("nft command failure was accepted")
	}
}

func proxyCounterJSON(upload2, download2, upload3, download3 uint64) []byte {
	return []byte(`{"nftables":[` +
		`{"set":{"family":"inet","table":"proxypool_guard","name":"v2_proxy_uploads","elem":[` +
		`{"elem":{"val":{"concat":["aa:bb:cc:dd:ee:01","192.168.9.22","0x005a0002"]},"counter":{"packets":1,"bytes":` + decimal(upload2) + `}}},` +
		`{"elem":{"val":{"concat":["aa:bb:cc:dd:ee:02","192.168.9.23",5898243]},"counter":{"packets":1,"bytes":` + decimal(upload3) + `}}}` +
		`]}},` +
		`{"set":{"family":"inet","table":"proxypool_guard","name":"v2_proxy_downloads","elem":[` +
		`{"elem":{"val":{"concat":["192.168.9.22",12002]},"counter":{"packets":1,"bytes":` + decimal(download2) + `}}},` +
		`{"elem":{"val":{"concat":["192.168.9.23","12003"]},"counter":{"packets":1,"bytes":` + decimal(download3) + `}}}` +
		`]}}]}`)
}

func decimal(value uint64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}

type protocolTrafficRunner struct {
	mu     sync.Mutex
	output []byte
	err    error
	calls  int
}

func (runner *protocolTrafficRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls++
	if name != nftPath || len(args) != 6 || args[0] != "-j" || args[1] != "-nn" || args[2] != "list" ||
		args[3] != "table" || args[4] != "inet" || args[5] != "proxypool_guard" {
		return nil, errors.New("unexpected nft command")
	}
	return append([]byte(nil), runner.output...), runner.err
}

func (runner *protocolTrafficRunner) setOutput(output []byte) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.output = output
}

func (runner *protocolTrafficRunner) setError(err error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.err = err
}

func (runner *protocolTrafficRunner) callCount() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.calls
}
