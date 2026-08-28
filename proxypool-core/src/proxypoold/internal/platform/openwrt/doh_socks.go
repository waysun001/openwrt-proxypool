package openwrt

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"proxypoold/internal/model"
	socksprotocol "proxypoold/internal/socks5"
)

type socksDoHDialer struct {
	dialer    socksprotocol.Dialer
	bootstrap netip.Addr
}

func (dialer socksDoHDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, errors.New("SOCKS5 DoH dial request is invalid")
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("SOCKS5 DoH dial request is invalid")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, errors.New("SOCKS5 DoH dial request is invalid")
	}
	target := net.JoinHostPort(dialer.bootstrap.String(), strconv.FormatUint(port, 10))
	return dialer.dialer.DialContext(ctx, "tcp4", target)
}

// NewSOCKSDoHTransport pins the DoH server address and reaches it exclusively
// through the node's SOCKS5 endpoint. It deliberately has no direct fallback.
func NewSOCKSDoHTransport(endpoint model.DoHEndpoint, proxyAddress, username, password string) (*http.Transport, error) {
	parsed, err := url.Parse(endpoint.URL)
	bootstrap, bootstrapErr := netip.ParseAddr(endpoint.BootstrapIP)
	proxyHost, proxyPort, proxyErr := net.SplitHostPort(proxyAddress)
	proxyIP, proxyIPErr := netip.ParseAddr(proxyHost)
	parsedPort, proxyPortErr := strconv.ParseUint(proxyPort, 10, 16)
	if err != nil || bootstrapErr != nil || !bootstrap.Is4() || !bootstrap.IsGlobalUnicast() ||
		proxyErr != nil || proxyIPErr != nil || !proxyIP.Is4() || proxyPortErr != nil || parsedPort == 0 ||
		parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		endpoint.ServerName == "" || strings.ContainsAny(endpoint.ServerName, "\x00/\\: ") ||
		!strings.EqualFold(parsed.Hostname(), endpoint.ServerName) || (username == "") != (password == "") {
		return nil, errors.New("SOCKS5 DoH transport endpoint is invalid")
	}
	dialer := socksDoHDialer{
		dialer:    socksprotocol.Dialer{ProxyAddress: proxyAddress, Username: username, Password: password},
		bootstrap: bootstrap.Unmap(),
	}
	return &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true, DisableCompression: true,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.ServerName},
		MaxIdleConns:    8, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second,
	}, nil
}
