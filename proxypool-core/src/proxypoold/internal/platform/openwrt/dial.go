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

type boundDialFunc func(context.Context, string, string, string, uint32) (net.Conn, error)

type BoundDialer struct {
	device    string
	bootstrap netip.Addr
	mark      uint32
	dial      boundDialFunc
}

type bootstrapDialer struct{ bootstrap netip.Addr }

func (dialer bootstrapDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, errors.New("bootstrap dial request is invalid")
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("bootstrap dial request is invalid")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, errors.New("bootstrap dial request is invalid")
	}
	target := net.JoinHostPort(dialer.bootstrap.String(), strconv.FormatUint(port, 10))
	return (&net.Dialer{}).DialContext(ctx, network, target)
}

func newBoundDialer(device, bootstrap string, policyID uint16, dial boundDialFunc) (*BoundDialer, error) {
	address, err := netip.ParseAddr(bootstrap)
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() || !safeInterface.MatchString(device) || !strings.HasPrefix(device, "l2tp-ppv2") || policyID == 0 || policyID > 60 || dial == nil {
		return nil, errors.New("bound dialer configuration is invalid")
	}
	return &BoundDialer{device: device, bootstrap: address, mark: policyMark(policyID), dial: dial}, nil
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
	connection, err := dialer.dial(ctx, network, target, dialer.device, dialer.mark)
	if err != nil {
		return nil, errors.New("bound interface dial failed")
	}
	return connection, nil
}

func NewDoHTransport(endpoint model.DoHEndpoint, device string, policyID uint16) (*http.Transport, error) {
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		endpoint.ServerName == "" || strings.ContainsAny(endpoint.ServerName, "\x00/\\: ") || !strings.EqualFold(parsed.Hostname(), endpoint.ServerName) {
		return nil, errors.New("DoH transport endpoint is invalid")
	}
	dialer, err := newBoundDialer(device, endpoint.BootstrapIP, policyID, dialBoundDevice)
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

// NewBootstrapDoHTransport is used only by router-originated endpoint lookup
// before a node PPP interface exists. Client traffic is never sent through it.
func NewBootstrapDoHTransport(endpoint model.DoHEndpoint) (*http.Transport, error) {
	parsed, err := url.Parse(endpoint.URL)
	bootstrap, bootstrapErr := netip.ParseAddr(endpoint.BootstrapIP)
	if err != nil || bootstrapErr != nil || !bootstrap.Is4() || !bootstrap.IsGlobalUnicast() ||
		parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		endpoint.ServerName == "" || strings.ContainsAny(endpoint.ServerName, "\x00/\\: ") || !strings.EqualFold(parsed.Hostname(), endpoint.ServerName) {
		return nil, errors.New("bootstrap DoH transport endpoint is invalid")
	}
	dialer := bootstrapDialer{bootstrap: bootstrap}
	return &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true, DisableCompression: true,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.ServerName},
		MaxIdleConns:    4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
	}, nil
}
