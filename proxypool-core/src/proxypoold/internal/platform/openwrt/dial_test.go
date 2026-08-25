package openwrt

import (
	"context"
	"errors"
	"net"
	"testing"

	"proxypoold/internal/model"
)

func TestDoHTransportDialsBootstrapAddressThroughBoundInterface(t *testing.T) {
	var gotNetwork, gotAddress, gotInterface string
	var gotMark uint32
	dialer, err := newBoundDialer("l2tp-ppv20042", "1.1.1.1", 42, func(_ context.Context, network, address, device string, mark uint32) (net.Conn, error) {
		gotNetwork, gotAddress, gotInterface = network, address, device
		gotMark = mark
		return nil, errors.New("stop after observation")
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = dialer.DialContext(context.Background(), "tcp", "dns.example:443")
	if gotNetwork != "tcp" || gotAddress != "1.1.1.1:443" || gotInterface != "l2tp-ppv20042" || gotMark != 0x005a002a {
		t.Fatalf("bound dial target = %q/%q/%q mark %#x", gotNetwork, gotAddress, gotInterface, gotMark)
	}
}

func TestNewDoHTransportRequiresHTTPSBootstrapServerNameAndOwnedInterface(t *testing.T) {
	base := model.DoHEndpoint{URL: "https://dns.example/dns-query", BootstrapIP: "1.1.1.1", ServerName: "dns.example"}
	for _, mutate := range []func(*model.DoHEndpoint, *string){
		func(endpoint *model.DoHEndpoint, _ *string) { endpoint.URL = "http://dns.example/dns-query" },
		func(endpoint *model.DoHEndpoint, _ *string) { endpoint.BootstrapIP = "dns.example" },
		func(endpoint *model.DoHEndpoint, _ *string) { endpoint.ServerName = "" },
		func(_ *model.DoHEndpoint, device *string) { *device = `ppp0;reboot` },
	} {
		endpoint, device := base, "l2tp-ppv20042"
		mutate(&endpoint, &device)
		if _, err := NewDoHTransport(endpoint, device, 42); err == nil {
			t.Fatal("unsafe DoH transport configuration was accepted")
		}
	}
	if _, err := NewDoHTransport(base, "l2tp-ppv20042", 0); err == nil {
		t.Fatal("DoH transport without a node policy was accepted")
	}
}

func TestNewBootstrapDoHTransportDoesNotRequireAnEstablishedNodeInterface(t *testing.T) {
	endpoint := model.DoHEndpoint{URL: "https://dns.example/dns-query", BootstrapIP: "1.1.1.1", ServerName: "dns.example"}
	transport, err := NewBootstrapDoHTransport(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if transport == nil || transport.DialContext == nil || transport.Proxy != nil || transport.TLSClientConfig.ServerName != "dns.example" {
		t.Fatalf("bootstrap transport = %#v", transport)
	}
}
