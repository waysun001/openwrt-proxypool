package dnsproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"proxypoold/internal/model"
)

type DoHChannel struct {
	endpoint *url.URL
	client   *http.Client
}

func NewDoHChannel(endpoint model.DoHEndpoint, transport http.RoundTripper) (*DoHChannel, error) {
	parsed, err := url.Parse(endpoint.URL)
	bootstrap, bootstrapErr := netip.ParseAddr(endpoint.BootstrapIP)
	if err != nil || bootstrapErr != nil || !bootstrap.Is4() || !bootstrap.IsGlobalUnicast() || transport == nil ||
		parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || endpoint.ServerName == "" || strings.ContainsAny(endpoint.ServerName, "\x00/\\: ") {
		return nil, errors.New("DoH endpoint is invalid")
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &DoHChannel{endpoint: parsed, client: client}, nil
}

func (channel *DoHChannel) Resolve(ctx context.Context, query []byte) ([]byte, error) {
	if channel == nil || channel.client == nil || channel.endpoint == nil || !validDNSQuery(query) {
		return nil, errors.New("DoH query is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, channel.endpoint.String(), bytes.NewReader(query))
	if err != nil {
		return nil, errors.New("DoH request failed")
	}
	request.Header.Set("Content-Type", "application/dns-message")
	request.Header.Set("Accept", "application/dns-message")
	response, err := channel.client.Do(request)
	if err != nil {
		return nil, errors.New("DoH transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("DoH status failed")
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || strings.ToLower(mediaType) != "application/dns-message" {
		return nil, errors.New("DoH content type failed")
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxDNSMessage+1))
	if err != nil || !validDNSResponse(query, contents) {
		return nil, errors.New("DoH response failed")
	}
	return append([]byte(nil), contents...), nil
}

var _ NodeChannel = (*DoHChannel)(nil)
