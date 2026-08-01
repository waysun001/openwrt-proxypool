package dnsproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	maxDNSMessage     = 4096
	defaultDNSTimeout = 5 * time.Second
	maxDNSConcurrency = 128
)

type NodeChannel interface {
	Resolve(context.Context, []byte) ([]byte, error)
}

type NodeChannelFunc func(context.Context, []byte) ([]byte, error)

func (function NodeChannelFunc) Resolve(ctx context.Context, query []byte) ([]byte, error) {
	return function(ctx, query)
}

type binding struct {
	channel NodeChannel
	ctx     context.Context
	cancel  context.CancelFunc
}

type ServerOption func(*Server)

func WithQueryTimeout(timeout time.Duration) ServerOption {
	return func(server *Server) {
		if timeout > 0 {
			server.queryTimeout = timeout
		}
	}
}

type Server struct {
	mu           sync.RWMutex
	listen       netip.AddrPort
	bound        netip.AddrPort
	bindings     map[netip.Addr]binding
	queryTimeout time.Duration
	ready        chan struct{}
	readyOnce    sync.Once
	runOnce      sync.Once
	semaphore    chan struct{}
}

func NewServer(address netip.AddrPort, options ...ServerOption) *Server {
	server := &Server{
		listen: address, bindings: make(map[netip.Addr]binding), queryTimeout: defaultDNSTimeout,
		ready: make(chan struct{}), semaphore: make(chan struct{}, maxDNSConcurrency),
	}
	for _, option := range options {
		if option != nil {
			option(server)
		}
	}
	return server
}

func (server *Server) SetBindings(channels map[netip.Addr]NodeChannel) {
	if server == nil {
		return
	}
	next := make(map[netip.Addr]binding, len(channels))
	for address, channel := range channels {
		address = address.Unmap()
		if !address.Is4() || channel == nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		next[address] = binding{channel: channel, ctx: ctx, cancel: cancel}
	}
	server.mu.Lock()
	previous := server.bindings
	server.bindings = next
	server.mu.Unlock()
	for _, old := range previous {
		old.cancel()
	}
}

func (server *Server) Ready() <-chan struct{} {
	if server == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return server.ready
}

func (server *Server) Addr() netip.AddrPort {
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.bound
}

func (server *Server) Run(ctx context.Context) error {
	if server == nil || !server.listen.IsValid() || !server.listen.Addr().Is4() || server.queryTimeout <= 0 {
		return errors.New("DNS server configuration is invalid")
	}
	ran := false
	server.runOnce.Do(func() { ran = true })
	if !ran {
		return errors.New("DNS server is already used")
	}
	tcp, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(server.listen))
	if err != nil {
		return errors.New("DNS TCP listen failed")
	}
	bound := tcp.Addr().(*net.TCPAddr).AddrPort()
	udp, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(bound))
	if err != nil {
		_ = tcp.Close()
		return errors.New("DNS UDP listen failed")
	}
	server.mu.Lock()
	server.bound = bound
	server.mu.Unlock()
	server.readyOnce.Do(func() { close(server.ready) })
	defer func() {
		_ = tcp.Close()
		_ = udp.Close()
		server.SetBindings(nil)
	}()

	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- server.serveUDP(ctx, udp) }()
	go func() { errorsChannel <- server.serveTCP(ctx, tcp) }()
	select {
	case <-ctx.Done():
		_ = tcp.Close()
		_ = udp.Close()
		return nil
	case err := <-errorsChannel:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func (server *Server) serveUDP(ctx context.Context, listener *net.UDPConn) error {
	buffer := make([]byte, maxDNSMessage+1)
	for {
		length, source, err := listener.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return errors.New("DNS UDP read failed")
		}
		if length > maxDNSMessage {
			continue
		}
		query := append([]byte(nil), buffer[:length]...)
		select {
		case server.semaphore <- struct{}{}:
			go func() {
				defer func() { <-server.semaphore }()
				response, err := server.resolve(source.Addr().Unmap(), query)
				if err == nil {
					_, _ = listener.WriteToUDPAddrPort(response, source)
				}
			}()
		default:
		}
	}
}

func (server *Server) serveTCP(ctx context.Context, listener *net.TCPListener) error {
	for {
		connection, err := listener.AcceptTCP()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return errors.New("DNS TCP accept failed")
		}
		select {
		case server.semaphore <- struct{}{}:
			go func() {
				defer func() { <-server.semaphore }()
				server.serveTCPConnection(connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func (server *Server) serveTCPConnection(connection *net.TCPConn) {
	defer connection.Close()
	remote, ok := connection.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	source := remote.AddrPort().Addr().Unmap()
	header := make([]byte, 2)
	for {
		_ = connection.SetDeadline(time.Now().Add(server.queryTimeout))
		if _, err := io.ReadFull(connection, header); err != nil {
			return
		}
		length := int(binary.BigEndian.Uint16(header))
		if length < 12 || length > maxDNSMessage {
			return
		}
		query := make([]byte, length)
		if _, err := io.ReadFull(connection, query); err != nil {
			return
		}
		response, err := server.resolve(source, query)
		if err != nil || len(response) > 65535 {
			return
		}
		binary.BigEndian.PutUint16(header, uint16(len(response)))
		if _, err := connection.Write(append(header, response...)); err != nil {
			return
		}
	}
}

func (server *Server) resolve(source netip.Addr, query []byte) ([]byte, error) {
	if !validDNSQuery(query) {
		return nil, errors.New("DNS query is invalid")
	}
	server.mu.RLock()
	selected, exists := server.bindings[source]
	server.mu.RUnlock()
	if !exists {
		return nil, errors.New("DNS source is not bound")
	}
	ctx, cancel := context.WithTimeout(selected.ctx, server.queryTimeout)
	defer cancel()
	response, err := selected.channel.Resolve(ctx, append([]byte(nil), query...))
	if err != nil || !validDNSResponse(query, response) {
		return nil, errors.New("DNS node resolution failed")
	}
	return append([]byte(nil), response...), nil
}

func validDNSQuery(query []byte) bool {
	return len(query) >= 12 && len(query) <= maxDNSMessage && query[2]&0x80 == 0
}

func validDNSResponse(query, response []byte) bool {
	return len(response) >= 12 && len(response) <= maxDNSMessage && response[2]&0x80 != 0 &&
		binary.BigEndian.Uint16(response[:2]) == binary.BigEndian.Uint16(query[:2])
}
