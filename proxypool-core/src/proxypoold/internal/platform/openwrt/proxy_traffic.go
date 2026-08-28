package openwrt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"sync"
	"time"

	"proxypoold/internal/platform"
)

const (
	defaultProxyTrafficCacheTTL = 250 * time.Millisecond
	proxyTrafficReadTimeout     = 2 * time.Second
	maxProxyTrafficJSONBytes    = 4 << 20
)

var proxyInterfacePattern = regexp.MustCompile(`^psx([0-9]{4})$`)

type ProtocolTrafficReader struct {
	sysfs    *SysfsTrafficReader
	runner   platform.CommandRunner
	mu       sync.Mutex
	now      func() time.Time
	cacheTTL time.Duration
	cachedAt time.Time
	cache    map[uint16]platform.InterfaceCounters
	lastRaw  map[string]uint64
	totals   map[string]uint64
}

func NewProtocolTrafficReader(sysfsRoot string, runner platform.CommandRunner) *ProtocolTrafficReader {
	return &ProtocolTrafficReader{
		sysfs: NewSysfsTrafficReader(sysfsRoot), runner: runner, now: time.Now,
		cacheTTL: defaultProxyTrafficCacheTTL, cache: make(map[uint16]platform.InterfaceCounters),
		lastRaw: make(map[string]uint64), totals: make(map[string]uint64),
	}
}

func (reader *ProtocolTrafficReader) ReadInterfaceCounters(interfaceName string) (platform.InterfaceCounters, error) {
	if reader == nil {
		return platform.InterfaceCounters{}, errors.New("protocol traffic reader is unavailable")
	}
	match := proxyInterfacePattern.FindStringSubmatch(interfaceName)
	if match == nil {
		return reader.sysfs.ReadInterfaceCounters(interfaceName)
	}
	policy, err := strconv.ParseUint(match[1], 10, 16)
	if err != nil || policy == 0 || policy > 60 {
		return platform.InterfaceCounters{}, errors.New("proxy traffic interface is invalid")
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.runner == nil || reader.now == nil || reader.cacheTTL <= 0 {
		return platform.InterfaceCounters{}, errors.New("proxy traffic reader is unavailable")
	}
	now := reader.now()
	if reader.cachedAt.IsZero() || now.Sub(reader.cachedAt) >= reader.cacheTTL || now.Before(reader.cachedAt) {
		if err := reader.refreshLocked(now); err != nil {
			return platform.InterfaceCounters{}, err
		}
	}
	return reader.cache[uint16(policy)], nil
}

func (reader *ProtocolTrafficReader) refreshLocked(now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), proxyTrafficReadTimeout)
	defer cancel()
	contents, err := reader.runner.Run(ctx, nftPath, "-j", "-nn", "list", "table", "inet", "proxypool_guard")
	if err != nil || len(contents) == 0 || len(contents) > maxProxyTrafficJSONBytes {
		return errors.New("proxy traffic counters are unavailable")
	}
	raw, active, err := parseProxyCounterJSON(contents)
	if err != nil {
		return errors.New("proxy traffic counter evidence is invalid")
	}
	for key, value := range raw {
		previous, existed := reader.lastRaw[key]
		if !existed || value < previous {
			reader.totals[key] += value
		} else {
			reader.totals[key] += value - previous
		}
		reader.lastRaw[key] = value
	}
	cache := make(map[uint16]platform.InterfaceCounters)
	for _, element := range active {
		counter := cache[element.policy]
		if element.download {
			counter.RXBytes += reader.totals[element.key]
		} else {
			counter.TXBytes += reader.totals[element.key]
		}
		cache[element.policy] = counter
	}
	reader.cache = cache
	reader.cachedAt = now
	return nil
}

type proxyCounterElement struct {
	key      string
	policy   uint16
	download bool
}

func parseProxyCounterJSON(contents []byte) (map[string]uint64, []proxyCounterElement, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	var envelope struct {
		Nftables []json.RawMessage `json:"nftables"`
	}
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Nftables) == 0 {
		return nil, nil, errors.New("invalid nft JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("trailing nft JSON")
	}
	rawCounters := make(map[string]uint64)
	active := make([]proxyCounterElement, 0)
	seenSets := map[string]bool{}
	for _, objectRaw := range envelope.Nftables {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(objectRaw, &object); err != nil {
			return nil, nil, err
		}
		setRaw, exists := object["set"]
		if !exists {
			continue
		}
		var set struct {
			Family string            `json:"family"`
			Table  string            `json:"table"`
			Name   string            `json:"name"`
			Elem   []json.RawMessage `json:"elem"`
		}
		if err := json.Unmarshal(setRaw, &set); err != nil {
			return nil, nil, err
		}
		if set.Name != "v2_proxy_uploads" && set.Name != "v2_proxy_downloads" {
			continue
		}
		if set.Family != "inet" || set.Table != "proxypool_guard" || seenSets[set.Name] {
			return nil, nil, errors.New("counter set identity is invalid")
		}
		seenSets[set.Name] = true
		for _, elementRaw := range set.Elem {
			element, value, err := parseProxyCounterElement(set.Name, elementRaw)
			if err != nil {
				return nil, nil, err
			}
			if _, duplicate := rawCounters[element.key]; duplicate {
				return nil, nil, errors.New("duplicate counter element")
			}
			rawCounters[element.key] = value
			active = append(active, element)
		}
	}
	if !seenSets["v2_proxy_uploads"] || !seenSets["v2_proxy_downloads"] {
		return nil, nil, errors.New("counter sets are missing")
	}
	return rawCounters, active, nil
}

func parseProxyCounterElement(setName string, raw json.RawMessage) (proxyCounterElement, uint64, error) {
	var wrapper struct {
		Elem struct {
			Val struct {
				Concat []json.RawMessage `json:"concat"`
			} `json:"val"`
			Counter struct {
				Bytes json.Number `json:"bytes"`
			} `json:"counter"`
		} `json:"elem"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&wrapper); err != nil || wrapper.Elem.Counter.Bytes == "" {
		return proxyCounterElement{}, 0, errors.New("counter element is malformed")
	}
	byteCount, err := strconv.ParseUint(wrapper.Elem.Counter.Bytes.String(), 10, 64)
	if err != nil {
		return proxyCounterElement{}, 0, errors.New("counter bytes are invalid")
	}
	parts := wrapper.Elem.Val.Concat
	if setName == "v2_proxy_uploads" {
		if len(parts) != 3 {
			return proxyCounterElement{}, 0, errors.New("upload counter key is malformed")
		}
		mac, err := jsonString(parts[0])
		if err != nil {
			return proxyCounterElement{}, 0, err
		}
		parsedMAC, err := net.ParseMAC(mac)
		if err != nil || len(parsedMAC) != 6 {
			return proxyCounterElement{}, 0, errors.New("upload counter MAC is invalid")
		}
		address, err := jsonIPv4(parts[1])
		if err != nil {
			return proxyCounterElement{}, 0, err
		}
		mark, err := proxyJSONUint(parts[2], 32)
		if err != nil || mark&0xffff0000 != 0x005a0000 || mark&0xffff == 0 || mark&0xffff > 60 {
			return proxyCounterElement{}, 0, errors.New("upload counter mark is invalid")
		}
		key := fmt.Sprintf("upload|%s|%s|%08x", parsedMAC.String(), address, mark)
		return proxyCounterElement{key: key, policy: uint16(mark & 0xffff)}, byteCount, nil
	}
	if len(parts) != 2 {
		return proxyCounterElement{}, 0, errors.New("download counter key is malformed")
	}
	address, err := jsonIPv4(parts[0])
	if err != nil {
		return proxyCounterElement{}, 0, err
	}
	port, err := proxyJSONUint(parts[1], 16)
	if err != nil || port <= socks5BasePort || port > socks5BasePort+60 {
		return proxyCounterElement{}, 0, errors.New("download counter port is invalid")
	}
	key := fmt.Sprintf("download|%s|%d", address, port)
	return proxyCounterElement{key: key, policy: uint16(port - socks5BasePort), download: true}, byteCount, nil
}

func jsonString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", errors.New("nft JSON string is invalid")
	}
	return value, nil
}

func jsonIPv4(raw json.RawMessage) (netip.Addr, error) {
	value, err := jsonString(raw)
	if err != nil {
		return netip.Addr{}, err
	}
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() || !authorizationLAN.Contains(address) {
		return netip.Addr{}, errors.New("nft JSON IPv4 is invalid")
	}
	return address.Unmap(), nil
}

func proxyJSONUint(raw json.RawMessage, bitSize int) (uint64, error) {
	text := string(raw)
	if len(text) > 1 && text[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, err
		}
		text = value
	}
	base := 10
	if len(text) > 2 && text[0:2] == "0x" {
		base = 0
	}
	return strconv.ParseUint(text, base, bitSize)
}

var _ platform.InterfaceTrafficReader = (*ProtocolTrafficReader)(nil)
