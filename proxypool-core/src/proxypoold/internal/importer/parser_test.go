package importer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"proxypoold/internal/model"
)

func TestParseL2TPLegacyFormsAndCRLF(t *testing.T) {
	raw := "vpn-a.example|alice|secret-a\r\n\r\nvpn-b.example|1702|bob|secret-b\r\n" +
		"vpn-c.example|carol|secret-c|2026-12-31\n" +
		"vpn-d.example|1703|dave|secret-d|2027-01-02\n"
	result := Parse(model.ProtocolL2TP, raw)
	if len(result.Errors) != 0 || len(result.Nodes) != 4 {
		t.Fatalf("Parse() nodes=%d errors=%#v", len(result.Nodes), result.Errors)
	}
	if result.Nodes[0].Port != 1701 || result.Nodes[1].Port != 1702 || result.Nodes[2].Port != 1701 || result.Nodes[3].Port != 1703 {
		t.Fatalf("ports = %d %d %d %d", result.Nodes[0].Port, result.Nodes[1].Port, result.Nodes[2].Port, result.Nodes[3].Port)
	}
	if result.Nodes[0].Password != "secret-a" || result.Nodes[1].Username != "bob" {
		t.Fatalf("credentials were not preserved: %#v", result.Nodes)
	}
	if result.Nodes[2].ExpiresAt == nil || result.Nodes[2].ExpiresAt.Format("2006-01-02") != "2026-12-31" || result.Nodes[3].ExpiresAt == nil {
		t.Fatalf("expiry values = %#v %#v", result.Nodes[2].ExpiresAt, result.Nodes[3].ExpiresAt)
	}
}

func TestParseLegacyFixtures(t *testing.T) {
	tests := []struct {
		path     string
		protocol model.Protocol
		count    int
	}{
		{path: "testdata/legacy-l2tp.txt", protocol: model.ProtocolL2TP, count: 4},
		{path: "testdata/legacy-socks5.txt", protocol: model.ProtocolSOCKS5, count: 2},
		{path: "testdata/legacy-slp.txt", protocol: model.ProtocolSLP, count: 2},
	}
	for _, test := range tests {
		raw, err := os.ReadFile(test.path)
		if err != nil {
			t.Fatal(err)
		}
		result := Parse(test.protocol, string(raw))
		if len(result.Errors) != 0 || len(result.Nodes) != test.count {
			t.Fatalf("Parse(%s) nodes=%d errors=%#v", test.path, len(result.Nodes), result.Errors)
		}
	}
}

func TestParseSOCKS5AndSLPPreservesSecretsWithoutPreviewLeak(t *testing.T) {
	socks := Parse(model.ProtocolSOCKS5, "proxy.example|1080|user|socks-secret|2027-03-04")
	if len(socks.Errors) != 0 || len(socks.Nodes) != 1 || socks.Nodes[0].Password != "socks-secret" {
		t.Fatalf("SOCKS5 parse = %#v", socks)
	}
	slp := Parse(model.ProtocolSLP, "slp.example|443|slp-token-secret|quic")
	if len(slp.Errors) != 0 || len(slp.Nodes) != 1 || slp.Nodes[0].SLPToken != "slp-token-secret" || slp.Nodes[0].SLPTransport != "quic" {
		t.Fatalf("SLP parse = %#v", slp)
	}
	encoded, err := json.Marshal(SanitizedRows(ParseResult{Nodes: append(socks.Nodes, slp.Nodes...)}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "slp-token-secret") || strings.Contains(string(encoded), "socks-secret") ||
		strings.Contains(string(encoded), "user") || !strings.Contains(string(encoded), `"secret_set":true`) {
		t.Fatalf("sanitized rows leaked or omitted secret state: %s", encoded)
	}
	if strings.Contains(socks.Nodes[0].String(), "user") || strings.Contains(socks.Nodes[0].String(), "socks-secret") {
		t.Fatalf("Candidate.String() leaked credentials: %s", socks.Nodes[0].String())
	}
}

func TestParseRejectsMalformedDuplicateAndControlData(t *testing.T) {
	raw := "vpn.example|bad-port|user|password|2026-01-01\n" +
		"vpn.example|1701|user|password|not-a-date\n" +
		"vpn.example|1701|user|bad\tsecret\n" +
		"vpn.example|1701|user|password\n" +
		"VPN.EXAMPLE.|1701|user|other-password\n"
	result := Parse(model.ProtocolL2TP, raw)
	want := map[int]string{1: ErrorInvalidPort, 2: ErrorInvalidExpiry, 3: ErrorInvalidCharacter, 5: ErrorDuplicate}
	if len(result.Errors) != len(want) {
		t.Fatalf("errors = %#v", result.Errors)
	}
	for _, issue := range result.Errors {
		if want[issue.Line] != issue.Code || issue.Message == "" {
			t.Fatalf("unexpected issue = %#v", issue)
		}
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Line != 4 {
		t.Fatalf("accepted nodes = %#v", result.Nodes)
	}
}

func TestParseRejectsMoreThanSixtyRecords(t *testing.T) {
	lines := make([]string, 61)
	for index := range lines {
		lines[index] = "vpn-" + twoDigits(index) + ".example|user|password"
	}
	result := Parse(model.ProtocolL2TP, strings.Join(lines, "\n"))
	if len(result.Nodes) != 0 || len(result.Errors) != 1 || result.Errors[0].Code != ErrorCapacityExceeded {
		t.Fatalf("61-record result = %#v", result)
	}
}

func TestParseRejectsInputLargerThanOneMiB(t *testing.T) {
	result := Parse(model.ProtocolL2TP, strings.Repeat("x", MaxImportBytes+1))
	if len(result.Nodes) != 0 || len(result.Errors) != 1 || result.Errors[0].Code != ErrorRequestTooLarge {
		t.Fatalf("oversize result = %#v", result)
	}
}

func twoDigits(value int) string {
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)})
}

func mustDate(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		t.Fatal(err)
	}
	return &parsed
}
