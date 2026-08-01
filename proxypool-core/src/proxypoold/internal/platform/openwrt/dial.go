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
)

type boundDialFunc func(context.Context, string, string, string) (net.Conn, error)

type BoundDialer struct {
	device    string
	bootstrap netip.Addr
	dial      boundDialFunc
}

func newBoundDialer(device, bootstrap string, dial boundDialFunc) (*BoundDialer, error) {
	address, err := netip.ParseAddr(bootstrap)
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() || !safeInterface.MatchString(device) || !strings.HasPrefix(device, "l2tp-ppv2") || dial == nil {
		return nil, errors.New("bound dialer configuration is invalid")
	}
	return &BoundDialer{device: device, bootstrap: address, dial: dial}, nil
}

func (dialer *BoundDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if dialer == nil || dialer.dial == nil || (network != "tcp" && network != "tcp4") {
		return nil, errors.New("bound dial request is invalid")
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("bound dial request is invalid")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, errors.New("bound dial request is invalid")
	}
	target := net.JoinHostPort(dialer.bootstrap.String(), strconv.FormatUint(port, 10))
	connection, err := dialer.dial(ctx, network, target, dialer.device)
	if err != nil {
		return nil, errors.New("bound interface dial failed")
	}
	return connection, nil
}

func NewDoHTransport(endpoint model.DoHEndpoint, device string) (*http.Transport, error) {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		endpoint.ServerName == "" || strings.ContainsAny(endpoint.ServerName, "\x00/\\: ") || !strings.EqualFold(parsed.Hostname(), endpoint.ServerName) {
		return nil, errors.New("DoH transport endpoint is invalid")
	}
	dialer, err := newBoundDialer(device, endpoint.BootstrapIP, dialBoundDevice)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		DisableCompression:  true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.ServerName},
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}, nil
}
