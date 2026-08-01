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
	dialer, err := newBoundDialer("l2tp-ppv20042", "1.1.1.1", func(_ context.Context, network, address, device string) (net.Conn, error) {
		gotNetwork, gotAddress, gotInterface = network, address, device
		return nil, errors.New("stop after observation")
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = dialer.DialContext(context.Background(), "tcp", "dns.example:443")
	if gotNetwork != "tcp" || gotAddress != "1.1.1.1:443" || gotInterface != "l2tp-ppv20042" {
		t.Fatalf("bound dial target = %q/%q/%q", gotNetwork, gotAddress, gotInterface)
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
		if _, err := NewDoHTransport(endpoint, device); err == nil {
			t.Fatal("unsafe DoH transport configuration was accepted")
		}
	}
}
