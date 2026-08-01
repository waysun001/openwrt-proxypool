package dnsproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"proxypoold/internal/model"
)

func TestDoHChannelPostsDNSWireMessageAndValidatesResponse(t *testing.T) {
	query := dnsQuery(77)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://dns.example/dns-query" || request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/dns-message" {
			t.Fatalf("unexpected DoH request: %#v", request)
		}
		body, _ := io.ReadAll(request.Body)
		if !bytes.Equal(body, query) {
			t.Fatalf("DoH body = %x", body)
		}
		response := append([]byte(nil), query...)
		response[2] |= 0x80
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/dns-message"}}, Body: io.NopCloser(bytes.NewReader(response))}, nil
	})
	channel, err := NewDoHChannel(model.DoHEndpoint{URL: "https://dns.example/dns-query", BootstrapIP: "1.1.1.1", ServerName: "dns.example"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	response, err := channel.Resolve(context.Background(), query)
	want := append([]byte(nil), query...)
	want[2] |= 0x80
	if err != nil || !bytes.Equal(response, want) {
		t.Fatalf("Resolve() response/error = %x/%v", response, err)
	}
}

func TestDoHChannelRejectsFallbackRedirectStatusContentTypeAndID(t *testing.T) {
	query := dnsQuery(78)
	for _, test := range []struct {
		name      string
		response  *http.Response
		transport error
	}{
		{name: "transport", transport: errors.New("interface bind failed")},
		{name: "redirect", response: dohResponse(302, "application/dns-message", query)},
		{name: "status", response: dohResponse(503, "application/dns-message", query)},
		{name: "content type", response: dohResponse(200, "text/plain", query)},
		{name: "ID", response: dohResponse(200, "application/dns-message", dnsQuery(79))},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) { return test.response, test.transport })
			channel, err := NewDoHChannel(model.DoHEndpoint{URL: "https://dns.example/dns-query", BootstrapIP: "1.1.1.1", ServerName: "dns.example"}, transport)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := channel.Resolve(context.Background(), query); err == nil {
				t.Fatal("invalid DoH result was accepted")
			}
		})
	}
}

func dohResponse(status int, contentType string, body []byte) *http.Response {
	response := append([]byte(nil), body...)
	response[2] |= 0x80
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(bytes.NewReader(response))}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
