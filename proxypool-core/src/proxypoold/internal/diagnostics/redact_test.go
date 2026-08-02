package diagnostics

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestRedactorRemovesKnownSecretsAndCommonCredentialForms(t *testing.T) {
	known := []string{"node-password-DO-NOT-RETURN", "slp-token-DO-NOT-RETURN"}
	redactor := NewRedactor(known)
	input := strings.Join([]string{
		`option password 'node-password-DO-NOT-RETURN'`,
		`{"token":"slp-token-DO-NOT-RETURN","password":"json-secret"}`,
		`https://alice:url-password@example.invalid/path`,
		`Cookie: sysauth=browser-session-secret`,
		`Authorization: Bearer bearer-secret`,
		base64.StdEncoding.EncodeToString([]byte(known[0])),
		"-----BEGIN PRIVATE KEY-----\nprivate-key-material\n-----END PRIVATE KEY-----",
	}, "\n")

	output := string(redactor.Redact([]byte(input)))
	for _, forbidden := range []string{
		known[0], known[1], "json-secret", "url-password", "browser-session-secret",
		"bearer-secret", base64.StdEncoding.EncodeToString([]byte(known[0])), "private-key-material",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("redaction leaked %q in %q", forbidden, output)
		}
	}
	if !strings.Contains(output, "<redacted>") {
		t.Fatalf("redaction marker missing from %q", output)
	}
}

func TestRedactorDoesNotMutateInputAndRemovesTinyKnownSecrets(t *testing.T) {
	input := []byte("state=online argv=xy password=long-enough-secret token_count=4")
	original := append([]byte(nil), input...)
	output := NewRedactor([]string{"xy", "long-enough-secret"}).Redact(input)
	if string(input) != string(original) {
		t.Fatal("redactor mutated caller input")
	}
	if strings.Contains(string(output), "long-enough-secret") || strings.Contains(string(output), "xy") || !strings.Contains(string(output), "state=online") {
		t.Fatalf("unexpected redaction output %q", output)
	}
}

func TestRedactorRemovesEncodedAndEscapedKnownSecretVariants(t *testing.T) {
	secret := `p@ss/+?'"\<>&`
	jsonValue, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	variants := []string{
		secret,
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawStdEncoding.EncodeToString([]byte(secret)),
		base64.URLEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)),
		url.QueryEscape(secret),
		url.PathEscape(secret),
		strings.ToLower(url.QueryEscape(secret)),
		mixedPercentCase(url.QueryEscape(secret)),
		string(jsonValue[1 : len(jsonValue)-1]),
		strings.ReplaceAll(secret, `'`, `'\''`),
	}
	output := string(NewRedactor([]string{secret}).Redact([]byte(strings.Join(variants, "\n"))))
	for _, variant := range variants {
		if variant != "" && strings.Contains(output, variant) {
			t.Fatalf("encoded known secret variant leaked %q in %q", variant, output)
		}
	}
}

func mixedPercentCase(value string) string {
	bytes := []byte(value)
	lower := false
	for index := 0; index+2 < len(bytes); index++ {
		if bytes[index] != '%' {
			continue
		}
		if lower {
			for offset := 1; offset <= 2; offset++ {
				if bytes[index+offset] >= 'A' && bytes[index+offset] <= 'F' {
					bytes[index+offset] += 'a' - 'A'
				}
			}
		}
		lower = !lower
		index += 2
	}
	return string(bytes)
}

func TestRedactorConsumesEscapedJSONAndQuotedUCISecretValues(t *testing.T) {
	input := []byte("{\"password\":\"abc\\\"suffix\"}\noption token 'value with spaces'\npassword: plain\nX-API-Key: header-secret\n folded-secret\n-----BEGIN PRIVATE KEY-----\ntruncated-key-material")
	output := string(NewRedactor(nil).Redact(input))
	for _, forbidden := range []string{`abc\"suffix`, "value with spaces", "plain", "header-secret", "folded-secret", "truncated-key-material"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("context redaction leaked %q in %q", forbidden, output)
		}
	}
}
