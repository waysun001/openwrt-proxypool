package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	CodeInvalidConfig  = "invalid_config"
	CodeAuthentication = "authentication_failed"
	CodeResolve        = "resolve_failed"
	CodeConnect        = "connect_failed"
	CodeTimeout        = "connect_timeout"
	CodeProtocol       = "protocol_failed"
)

const (
	socksVersion          = 5
	usernamePassword      = 2
	noAuthentication      = 0
	noAcceptableMethods   = 0xff
	connectCommand        = 1
	addressIPv4           = 1
	addressHostname       = 3
	addressIPv6           = 4
	usernamePasswordVer   = 1
	maxAuthenticationSize = 255
)

type Error struct {
	Code    string
	Message string
	Cause   error
}

func (err *Error) Error() string {
	if err == nil {
		return ""
	}
	return err.Message
}

func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func ErrorCode(err error) string {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

type Dialer struct {
	ProxyAddress string
	Username     string
	Password     string
}

func (dialer Dialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	method, targetRequest, err := dialer.validate(network, target)
	if err != nil {
		return nil, err
	}
	var netDialer net.Dialer
	conn, err := netDialer.DialContext(ctx, "tcp4", dialer.ProxyAddress)
	if err != nil {
		return nil, classifyIOError(ctx, CodeConnect, "SOCKS5 endpoint connection failed", err)
	}
	cleanupContext := bindHandshakeContext(ctx, conn)
	succeeded := false
	defer func() {
		cleanupContext()
		if !succeeded {
			_ = conn.Close()
		}
	}()

	if err := writeAll(conn, []byte{socksVersion, 1, method}); err != nil {
		return nil, classifyIOError(ctx, CodeProtocol, "SOCKS5 greeting failed", err)
	}
	selection, err := readFull(conn, 2)
	if err != nil {
		return nil, classifyIOError(ctx, CodeProtocol, "SOCKS5 authentication selection failed", err)
	}
	if selection[0] != socksVersion {
		return nil, newError(CodeProtocol, "SOCKS5 authentication selection is malformed", nil)
	}
	if selection[1] == noAcceptableMethods || selection[1] != method {
		return nil, newError(CodeAuthentication, "SOCKS5 authentication method was rejected", nil)
	}
	if method == usernamePassword {
		if err := dialer.authenticate(conn, ctx); err != nil {
			return nil, err
		}
	}
	if err := writeAll(conn, targetRequest); err != nil {
		return nil, classifyIOError(ctx, CodeProtocol, "SOCKS5 CONNECT request failed", err)
	}
	header, err := readFull(conn, 4)
	if err != nil {
		return nil, classifyIOError(ctx, CodeProtocol, "SOCKS5 CONNECT response failed", err)
	}
	if header[0] != socksVersion || header[2] != 0 {
		return nil, newError(CodeProtocol, "SOCKS5 CONNECT response is malformed", nil)
	}
	if header[1] != 0 {
		return nil, replyError(header[1])
	}
	if err := discardBoundAddress(conn, header[3]); err != nil {
		return nil, classifyIOError(ctx, CodeProtocol, "SOCKS5 CONNECT address is malformed", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, newError(CodeTimeout, "SOCKS5 CONNECT timed out", err)
	}
	succeeded = true
	return conn, nil
}

func (dialer Dialer) validate(network, target string) (byte, []byte, error) {
	if network != "tcp" && network != "tcp4" {
		return 0, nil, newError(CodeInvalidConfig, "SOCKS5 network is invalid", nil)
	}
	if !validAddress(dialer.ProxyAddress) || (dialer.Username == "") != (dialer.Password == "") ||
		len(dialer.Username) > maxAuthenticationSize || len(dialer.Password) > maxAuthenticationSize {
		return 0, nil, newError(CodeInvalidConfig, "SOCKS5 endpoint or credentials are invalid", nil)
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return 0, nil, newError(CodeInvalidConfig, "SOCKS5 target is invalid", nil)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return 0, nil, newError(CodeInvalidConfig, "SOCKS5 target port is invalid", nil)
	}
	address, atyp, err := encodeAddress(host)
	if err != nil {
		return 0, nil, err
	}
	request := make([]byte, 0, 4+len(address)+2)
	request = append(request, socksVersion, connectCommand, 0, atyp)
	request = append(request, address...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	method := byte(noAuthentication)
	if dialer.Username != "" {
		method = usernamePassword
	}
	return method, request, nil
}

func (dialer Dialer) authenticate(conn net.Conn, ctx context.Context) error {
	request := make([]byte, 0, 3+len(dialer.Username)+len(dialer.Password))
	request = append(request, usernamePasswordVer, byte(len(dialer.Username)))
	request = append(request, dialer.Username...)
	request = append(request, byte(len(dialer.Password)))
	request = append(request, dialer.Password...)
	if err := writeAll(conn, request); err != nil {
		return classifyIOError(ctx, CodeAuthentication, "SOCKS5 authentication failed", err)
	}
	response, err := readFull(conn, 2)
	if err != nil {
		return classifyIOError(ctx, CodeAuthentication, "SOCKS5 authentication failed", err)
	}
	if response[0] != usernamePasswordVer {
		return newError(CodeProtocol, "SOCKS5 authentication response is malformed", nil)
	}
	if response[1] != 0 {
		return newError(CodeAuthentication, "SOCKS5 credentials were rejected", nil)
	}
	return nil
}

func validAddress(address string) bool {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	return err == nil && port > 0
}

func encodeAddress(host string) ([]byte, byte, error) {
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			return append([]byte(nil), ipv4...), addressIPv4, nil
		}
		if ipv6 := ip.To16(); ipv6 != nil {
			return append([]byte(nil), ipv6...), addressIPv6, nil
		}
	}
	if len(host) == 0 || len(host) > 255 {
		return nil, 0, newError(CodeInvalidConfig, "SOCKS5 target hostname is invalid", nil)
	}
	encoded := make([]byte, 1, len(host)+1)
	encoded[0] = byte(len(host))
	encoded = append(encoded, host...)
	return encoded, addressHostname, nil
}

func discardBoundAddress(reader io.Reader, atyp byte) error {
	length := 0
	switch atyp {
	case addressIPv4:
		length = net.IPv4len
	case addressIPv6:
		length = net.IPv6len
	case addressHostname:
		encodedLength, err := readFull(reader, 1)
		if err != nil || encodedLength[0] == 0 {
			return errors.New("invalid SOCKS5 bound hostname")
		}
		length = int(encodedLength[0])
	default:
		return errors.New("invalid SOCKS5 bound address type")
	}
	_, err := readFull(reader, length+2)
	return err
}

func replyError(reply byte) error {
	switch reply {
	case 4:
		return newError(CodeResolve, "SOCKS5 target could not be resolved", nil)
	case 5:
		return newError(CodeConnect, "SOCKS5 target connection was refused", nil)
	case 6:
		return newError(CodeTimeout, "SOCKS5 target connection timed out", nil)
	default:
		return newError(CodeConnect, fmt.Sprintf("SOCKS5 CONNECT failed with reply %d", reply), nil)
	}
}

func readFull(reader io.Reader, length int) ([]byte, error) {
	contents := make([]byte, length)
	_, err := io.ReadFull(reader, contents)
	return contents, err
}

func writeAll(writer io.Writer, contents []byte) error {
	for len(contents) > 0 {
		written, err := writer.Write(contents)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrUnexpectedEOF
		}
		contents = contents[written:]
	}
	return nil
}

func bindHandshakeContext(ctx context.Context, conn net.Conn) func() {
	finished := make(chan struct{})
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-finished:
		}
	}()
	return func() {
		close(finished)
		_ = conn.SetDeadline(time.Time{})
	}
}

func classifyIOError(ctx context.Context, fallbackCode, message string, cause error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return newError(CodeTimeout, "SOCKS5 operation timed out", ctxErr)
	}
	var netErr net.Error
	if errors.As(cause, &netErr) && netErr.Timeout() {
		return newError(CodeTimeout, "SOCKS5 operation timed out", cause)
	}
	return newError(fallbackCode, message, cause)
}

func newError(code, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}
