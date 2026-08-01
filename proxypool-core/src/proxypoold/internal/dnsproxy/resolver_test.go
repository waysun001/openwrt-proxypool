package dnsproxy

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestHostResolverUsesDoHFallbackAndReturnsValidatedIPv4(t *testing.T) {
	first := NodeChannelFunc(func(context.Context, []byte) ([]byte, error) {
		return nil, errors.New("first endpoint down")
	})
	second := NodeChannelFunc(func(_ context.Context, query []byte) ([]byte, error) {
		response := append([]byte(nil), query...)
		response[2] = 0x81
		response[3] = 0x80
		response[6], response[7] = 0, 1
		response = append(response,
			0xc0, 0x0c, 0x00, 0x01, 0x00, 0x01,
			0x00, 0x00, 0x00, 0x3c, 0x00, 0x04,
			203, 0, 113, 17,
		)
		return response, nil
	})
	resolver := NewHostResolver(first, second)
	address, err := resolver.ResolveIPv4(context.Background(), "vpn.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if address != netip.MustParseAddr("203.0.113.17") {
		t.Fatalf("resolved address = %s", address)
	}
}

func TestHostResolverRejectsUnsafeNameAndMalformedOrPrivateAnswer(t *testing.T) {
	for _, host := range []string{"", "bad name", "-bad.example", "bad..example"} {
		if _, err := NewHostResolver(NodeChannelFunc(func(context.Context, []byte) ([]byte, error) { return nil, nil })).ResolveIPv4(context.Background(), host); err == nil {
			t.Fatalf("unsafe host %q was accepted", host)
		}
	}
	private := NodeChannelFunc(func(_ context.Context, query []byte) ([]byte, error) {
		response := append([]byte(nil), query...)
		response[2], response[3], response[6], response[7] = 0x81, 0x80, 0, 1
		return append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 127, 0, 0, 1), nil
	})
	if _, err := NewHostResolver(private).ResolveIPv4(context.Background(), "vpn.example"); err == nil {
		t.Fatal("loopback DNS answer was accepted as an L2TP endpoint")
	}
}
