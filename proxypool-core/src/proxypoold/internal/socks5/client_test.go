package socks5

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDialContextCompletesNoAuthHostnameCONNECT(t *testing.T) {
	server := newTestServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if got, err := readExactly(reader, 3); err != nil || string(got) != string([]byte{5, 1, 0}) {
			return errors.New("unexpected no-auth greeting")
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return err
		}
		request, err := readConnectRequest(reader)
		if err != nil {
			return err
		}
		if request.host != "example.test" || request.port != 443 || request.atyp != 3 {
			return errors.New("unexpected hostname CONNECT target")
		}
		return writeFragments(conn, []byte{5, 0, 0, 1, 127, 0, 0, 1, 0x23, 0x45})
	})
	defer server.close(t)

	conn, err := (Dialer{ProxyAddress: server.address()}).DialContext(context.Background(), "tcp", "example.test:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	_ = conn.Close()
}

func TestDialContextCompletesUsernamePasswordIPv4CONNECT(t *testing.T) {
	server := newTestServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if got, err := readExactly(reader, 3); err != nil || string(got) != string([]byte{5, 1, 2}) {
			return errors.New("unexpected authenticated greeting")
		}
		if _, err := conn.Write([]byte{5, 2}); err != nil {
			return err
		}
		auth, err := readExactly(reader, 1+1+4+1+6)
		if err != nil || string(auth) != string([]byte{1, 4, 'u', 's', 'e', 'r', 6, 's', 'e', 'c', 'r', 'e', 't'}) {
			return errors.New("unexpected username/password request")
		}
		if _, err := conn.Write([]byte{1, 0}); err != nil {
			return err
		}
		request, err := readConnectRequest(reader)
		if err != nil {
			return err
		}
		if request.host != "223.5.5.5" || request.port != 443 || request.atyp != 1 {
			return errors.New("unexpected IPv4 CONNECT target")
		}
		_, err = conn.Write([]byte{5, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1})
		return err
	})
	defer server.close(t)

	conn, err := (Dialer{ProxyAddress: server.address(), Username: "user", Password: "secret"}).DialContext(
		context.Background(), "tcp4", "223.5.5.5:443",
	)
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	_ = conn.Close()
}

func TestDialContextClassifiesAuthenticationRejection(t *testing.T) {
	server := newTestServer(t, func(conn net.Conn) error {
		if _, err := readExactly(conn, 3); err != nil {
			return err
		}
		if _, err := conn.Write([]byte{5, 2}); err != nil {
			return err
		}
		if _, err := readExactly(conn, 1+1+4+1+5); err != nil {
			return err
		}
		_, err := conn.Write([]byte{1, 1})
		return err
	})
	defer server.close(t)

	_, err := (Dialer{ProxyAddress: server.address(), Username: "user", Password: "wrong"}).DialContext(
		context.Background(), "tcp", "example.test:80",
	)
	if ErrorCode(err) != CodeAuthentication {
		t.Fatalf("error = %v, code = %q", err, ErrorCode(err))
	}
}

func TestDialContextRejectsUnsupportedAuthenticationSelection(t *testing.T) {
	server := newTestServer(t, func(conn net.Conn) error {
		if _, err := readExactly(conn, 3); err != nil {
			return err
		}
		_, err := conn.Write([]byte{5, 0})
		return err
	})
	defer server.close(t)

	_, err := (Dialer{ProxyAddress: server.address(), Username: "user", Password: "secret"}).DialContext(
		context.Background(), "tcp", "example.test:80",
	)
	if ErrorCode(err) != CodeAuthentication {
		t.Fatalf("error = %v, code = %q", err, ErrorCode(err))
	}
}

func TestDialContextClassifiesSOCKSReply(t *testing.T) {
	tests := []struct {
		name  string
		reply byte
		want  string
	}{
		{name: "host unreachable", reply: 4, want: CodeResolve},
		{name: "connection refused", reply: 5, want: CodeConnect},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t, func(conn net.Conn) error {
				reader := bufio.NewReader(conn)
				if _, err := readExactly(reader, 3); err != nil {
					return err
				}
				if _, err := conn.Write([]byte{5, 0}); err != nil {
					return err
				}
				if _, err := readConnectRequest(reader); err != nil {
					return err
				}
				_, err := conn.Write([]byte{5, test.reply, 0, 1, 0, 0, 0, 0, 0, 0})
				return err
			})
			defer server.close(t)

			_, err := (Dialer{ProxyAddress: server.address()}).DialContext(context.Background(), "tcp", "example.test:80")
			if ErrorCode(err) != test.want {
				t.Fatalf("error = %v, code = %q", err, ErrorCode(err))
			}
		})
	}
}

func TestDialContextRejectsMalformedReply(t *testing.T) {
	server := newTestServer(t, func(conn net.Conn) error {
		reader := bufio.NewReader(conn)
		if _, err := readExactly(reader, 3); err != nil {
			return err
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return err
		}
		if _, err := readConnectRequest(reader); err != nil {
			return err
		}
		_, err := conn.Write([]byte{4, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		return err
	})
	defer server.close(t)

	_, err := (Dialer{ProxyAddress: server.address()}).DialContext(context.Background(), "tcp", "example.test:80")
	if ErrorCode(err) != CodeProtocol {
		t.Fatalf("error = %v, code = %q", err, ErrorCode(err))
	}
}

func TestDialContextInterruptsAStalledHandshakeAtContextDeadline(t *testing.T) {
	server := newTestServer(t, func(conn net.Conn) error {
		if _, err := readExactly(conn, 3); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	defer server.close(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (Dialer{ProxyAddress: server.address()}).DialContext(ctx, "tcp", "example.test:80")
	if ErrorCode(err) != CodeTimeout {
		t.Fatalf("error = %v, code = %q", err, ErrorCode(err))
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("context cancellation took %s", elapsed)
	}
}

func TestDialContextRejectsInvalidConfigurationBeforeNetworkIO(t *testing.T) {
	tests := []struct {
		name    string
		dialer  Dialer
		network string
		target  string
	}{
		{name: "missing proxy", dialer: Dialer{}, network: "tcp", target: "example.test:80"},
		{name: "username only", dialer: Dialer{ProxyAddress: "127.0.0.1:1080", Username: "user"}, network: "tcp", target: "example.test:80"},
		{name: "password only", dialer: Dialer{ProxyAddress: "127.0.0.1:1080", Password: "secret"}, network: "tcp", target: "example.test:80"},
		{name: "long username", dialer: Dialer{ProxyAddress: "127.0.0.1:1080", Username: strings.Repeat("u", 256), Password: "secret"}, network: "tcp", target: "example.test:80"},
		{name: "unsupported network", dialer: Dialer{ProxyAddress: "127.0.0.1:1080"}, network: "udp", target: "example.test:80"},
		{name: "invalid target", dialer: Dialer{ProxyAddress: "127.0.0.1:1080"}, network: "tcp", target: "bad-target"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.dialer.DialContext(context.Background(), test.network, test.target); ErrorCode(err) != CodeInvalidConfig {
				t.Fatalf("DialContext(%#v) error = %v, code = %q", test.dialer, err, ErrorCode(err))
			}
		})
	}
}

type connectRequest struct {
	atyp byte
	host string
	port uint16
}

func readConnectRequest(reader io.Reader) (connectRequest, error) {
	header, err := readExactly(reader, 4)
	if err != nil {
		return connectRequest{}, err
	}
	if string(header[:3]) != string([]byte{5, 1, 0}) {
		return connectRequest{}, errors.New("unexpected CONNECT header")
	}
	request := connectRequest{atyp: header[3]}
	switch request.atyp {
	case 1:
		address, err := readExactly(reader, 4)
		if err != nil {
			return connectRequest{}, err
		}
		request.host = net.IP(address).String()
	case 3:
		length, err := readExactly(reader, 1)
		if err != nil {
			return connectRequest{}, err
		}
		address, err := readExactly(reader, int(length[0]))
		if err != nil {
			return connectRequest{}, err
		}
		request.host = string(address)
	case 4:
		address, err := readExactly(reader, 16)
		if err != nil {
			return connectRequest{}, err
		}
		request.host = net.IP(address).String()
	default:
		return connectRequest{}, errors.New("unexpected address type")
	}
	port, err := readExactly(reader, 2)
	if err != nil {
		return connectRequest{}, err
	}
	request.port = binary.BigEndian.Uint16(port)
	return request, nil
}

func readExactly(reader io.Reader, length int) ([]byte, error) {
	buffer := make([]byte, length)
	_, err := io.ReadFull(reader, buffer)
	return buffer, err
}

func writeFragments(writer io.Writer, contents []byte) error {
	for _, value := range contents {
		if _, err := writer.Write([]byte{value}); err != nil {
			return err
		}
	}
	return nil
}

type testServer struct {
	listener net.Listener
	done     chan error
}

func newTestServer(t *testing.T, serve func(net.Conn) error) *testServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testServer{listener: listener, done: make(chan error, 1)}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			server.done <- err
			return
		}
		defer conn.Close()
		server.done <- serve(conn)
	}()
	return server
}

func (server *testServer) address() string { return server.listener.Addr().String() }

func (server *testServer) close(t *testing.T) {
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
