package openwrt

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"proxypoold/internal/model"
)

func TestSOCKSDoHTransportConnectsPinnedBootstrapThroughProxy(t *testing.T) {
	proxy := newDoHSOCKSServer(t)
	defer proxy.close(t)
	endpoint := model.DoHEndpoint{
		URL: "https://dns.alidns.com/dns-query", BootstrapIP: "223.5.5.5", ServerName: "dns.alidns.com",
	}
	transport, err := NewSOCKSDoHTransport(endpoint, proxy.address(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := transport.DialContext(context.Background(), "tcp", "dns.alidns.com:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	_ = conn.Close()
	select {
	case target := <-proxy.target:
		if target != "223.5.5.5:443" {
			t.Fatalf("SOCKS5 CONNECT target = %q", target)
		}
	case <-time.After(time.Second):
		t.Fatal("SOCKS5 proxy did not receive CONNECT")
	}
}

func TestSOCKSDoHTransportRejectsDirectOrUnpinnedInputs(t *testing.T) {
	tests := []struct {
		name     string
		endpoint model.DoHEndpoint
		proxy    string
	}{
		{name: "missing proxy", endpoint: model.DoHEndpoint{URL: "https://dns.alidns.com/dns-query", BootstrapIP: "223.5.5.5", ServerName: "dns.alidns.com"}},
		{name: "unpinned DoH", proxy: "127.0.0.1:1080", endpoint: model.DoHEndpoint{URL: "https://dns.alidns.com/dns-query", ServerName: "dns.alidns.com"}},
		{name: "mismatched TLS name", proxy: "127.0.0.1:1080", endpoint: model.DoHEndpoint{URL: "https://dns.alidns.com/dns-query", BootstrapIP: "223.5.5.5", ServerName: "other.example"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if transport, err := NewSOCKSDoHTransport(test.endpoint, test.proxy, "", ""); err == nil || transport != nil {
				t.Fatalf("transport = %#v, error = %v", transport, err)
			}
		})
	}
}

type dohSOCKSServer struct {
	listener net.Listener
	target   chan string
	done     chan error
}

func newDoHSOCKSServer(t *testing.T) *dohSOCKSServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dohSOCKSServer{listener: listener, target: make(chan string, 1), done: make(chan error, 1)}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			server.done <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(reader, greeting); err != nil || string(greeting) != string([]byte{5, 1, 0}) {
			server.done <- errors.New("invalid SOCKS5 greeting")
			return
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			server.done <- err
			return
		}
		header := make([]byte, 4)
		if _, err := io.ReadFull(reader, header); err != nil || string(header) != string([]byte{5, 1, 0, 1}) {
			server.done <- errors.New("invalid SOCKS5 CONNECT header")
			return
		}
		address := make([]byte, 4)
		port := make([]byte, 2)
		if _, err := io.ReadFull(reader, address); err != nil {
			server.done <- err
			return
		}
		if _, err := io.ReadFull(reader, port); err != nil {
			server.done <- err
			return
		}
		server.target <- net.JoinHostPort(net.IP(address).String(), stringPort(binary.BigEndian.Uint16(port)))
		if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1}); err != nil {
			server.done <- err
			return
		}
		server.done <- nil
	}()
	return server
}

func (server *dohSOCKSServer) address() string { return server.listener.Addr().String() }

func (server *dohSOCKSServer) close(t *testing.T) {
	t.Helper()
	_ = server.listener.Close()
	select {
	case err := <-server.done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("SOCKS5 test server did not stop")
	}
}

func stringPort(port uint16) string {
	var digits [5]byte
	index := len(digits)
	for {
		index--
		digits[index] = byte('0' + port%10)
		port /= 10
		if port == 0 {
			return string(digits[index:])
		}
	}
}
