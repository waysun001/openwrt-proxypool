package dnsproxy

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestServerRoutesUDPAndTCPBySourceIP(t *testing.T) {
	server := NewServer(netip.MustParseAddrPort("127.0.0.1:0"), WithQueryTimeout(time.Second))
	server.SetBindings(map[netip.Addr]NodeChannel{
		netip.MustParseAddr("127.0.0.2"): markerChannel(2),
		netip.MustParseAddr("127.0.0.3"): markerChannel(3),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	awaitServerReady(t, server)

	for _, test := range []struct {
		source string
		mark   byte
	}{
		{source: "127.0.0.2", mark: 2},
		{source: "127.0.0.3", mark: 3},
	} {
		query := dnsQuery(uint16(test.mark))
		response := udpQuery(t, test.source, server.Addr(), query)
		if response[11] != test.mark || binary.BigEndian.Uint16(response[:2]) != uint16(test.mark) {
			t.Fatalf("UDP source %s received wrong channel response %x", test.source, response)
		}
	}
	query := dnsQuery(44)
	response := tcpQuery(t, "127.0.0.2", server.Addr(), query)
	if response[11] != 2 || binary.BigEndian.Uint16(response[:2]) != 44 {
		t.Fatalf("TCP received wrong channel response %x", response)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestServerDropsUnboundUDPWithoutFallback(t *testing.T) {
	server := NewServer(netip.MustParseAddrPort("127.0.0.1:0"), WithQueryTimeout(50*time.Millisecond))
	server.SetBindings(map[netip.Addr]NodeChannel{netip.MustParseAddr("127.0.0.2"): markerChannel(2)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Run(ctx)
	awaitServerReady(t, server)
	connection, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.4")}, net.UDPAddrFromAddrPort(server.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := connection.Write(dnsQuery(9)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 512)
	if _, _, err := connection.ReadFromUDP(buffer); err == nil {
		t.Fatal("unbound source received a DNS response")
	}
}

func TestServerRejectsMismatchedResponseID(t *testing.T) {
	channel := NodeChannelFunc(func(_ context.Context, query []byte) ([]byte, error) {
		response := append([]byte(nil), query...)
		binary.BigEndian.PutUint16(response[:2], binary.BigEndian.Uint16(query[:2])+1)
		response[2] |= 0x80
		return response, nil
	})
	server := NewServer(netip.MustParseAddrPort("127.0.0.1:0"), WithQueryTimeout(50*time.Millisecond))
	server.SetBindings(map[netip.Addr]NodeChannel{netip.MustParseAddr("127.0.0.2"): channel})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Run(ctx)
	awaitServerReady(t, server)
	connection, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.2")}, net.UDPAddrFromAddrPort(server.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = connection.Write(dnsQuery(10))
	if _, _, err := connection.ReadFromUDP(make([]byte, 512)); err == nil {
		t.Fatal("mismatched DNS response ID was forwarded")
	}
}

func TestServerCancelsInflightQueryWhenBindingIsRemoved(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var once sync.Once
	channel := NodeChannelFunc(func(ctx context.Context, _ []byte) ([]byte, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	server := NewServer(netip.MustParseAddrPort("127.0.0.1:0"), WithQueryTimeout(time.Second))
	server.SetBindings(map[netip.Addr]NodeChannel{netip.MustParseAddr("127.0.0.2"): channel})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Run(ctx)
	awaitServerReady(t, server)
	connection, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.2")}, net.UDPAddrFromAddrPort(server.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = connection.Write(dnsQuery(11))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("query did not start")
	}
	server.SetBindings(nil)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("removed binding did not cancel in-flight query")
	}
}

func dnsQuery(id uint16) []byte {
	query := make([]byte, 12)
	binary.BigEndian.PutUint16(query[:2], id)
	query[2] = 1
	query[5] = 1
	return query
}

type markerChannel byte

func (channel markerChannel) Resolve(_ context.Context, query []byte) ([]byte, error) {
	response := append([]byte(nil), query...)
	response[2] |= 0x80
	response[11] = byte(channel)
	return response, nil
}

func awaitServerReady(t *testing.T, server *Server) {
	t.Helper()
	select {
	case <-server.Ready():
	case <-time.After(time.Second):
		t.Fatal("DNS server did not become ready")
	}
}

func udpQuery(t *testing.T, source string, destination netip.AddrPort, query []byte) []byte {
	t.Helper()
	connection, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP(source)}, net.UDPAddrFromAddrPort(destination))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := connection.Write(query); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4096)
	length, err := connection.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buffer[:length]...)
}

func tcpQuery(t *testing.T, source string, destination netip.AddrPort, query []byte) []byte {
	t.Helper()
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
	connection, err := dialer.Dial("tcp4", destination.String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	frame := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(query)))
	copy(frame[2:], query)
	if _, err := connection.Write(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(connection, frame[:2]); err != nil {
		t.Fatal(err)
	}
	length := int(binary.BigEndian.Uint16(frame[:2]))
	response := make([]byte, length)
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatal(err)
	}
	return response
}
