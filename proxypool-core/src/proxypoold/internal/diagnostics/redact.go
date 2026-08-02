package diagnostics

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const redactionMarker = "<redacted>"

var (
	privateKeyPattern         = regexp.MustCompile(`(?s)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?(?:-----END [^-\r\n]*PRIVATE KEY-----|\z)`)
	urlUserPattern            = regexp.MustCompile(`(?i)(https?://[^/\s:@]+:)[^@\s/]+@`)
	headerPattern             = regexp.MustCompile(`(?im)^(\s*(?:cookie|set-cookie|authorization|proxy-authorization|x-auth-token|x-api-key)\s*:\s*).*(?:\r?\n[\t ]+.*)*$`)
	jsonSecretPattern         = regexp.MustCompile(`(?i)("(?:password|passwd|token|secret|cookie|authorization|slp_token|obfs_key|api_key)"\s*:\s*")((?:\\.|[^"\\])*)(")`)
	singleQuotedSecretPattern = regexp.MustCompile(`(?i)(\b(?:password|passwd|token|secret|cookie|authorization|slp_token|obfs_key|api_key)\b\s*(?:=|:|\s)\s*)'[^'\r\n]*'`)
	doubleQuotedSecretPattern = regexp.MustCompile(`(?i)(\b(?:password|passwd|token|secret|cookie|authorization|slp_token|obfs_key|api_key)\b\s*(?:=|:|\s)\s*)"(?:\\.|[^"\\])*"`)
	textSecretPattern         = regexp.MustCompile(`(?i)(\b(?:password|passwd|token|secret|cookie|authorization|slp_token|obfs_key|api_key)\b\s*(?:=|:|\s)\s*)[^\s,'"}\r\n]+`)
)

type Redactor struct {
	known        []knownReplacement
	knownEncoded []encodedReplacement
}

type knownReplacement struct {
	value []byte
	mask  []byte
}

type encodedReplacement struct {
	pattern *regexp.Regexp
	mask    []byte
}

func NewRedactor(secrets []string) *Redactor {
	unique := make(map[string]struct{}, len(secrets)*3)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		addKnownVariant(unique, secret)
		addKnownVariant(unique, base64.StdEncoding.EncodeToString([]byte(secret)))
		addKnownVariant(unique, base64.RawStdEncoding.EncodeToString([]byte(secret)))
		addKnownVariant(unique, base64.URLEncoding.EncodeToString([]byte(secret)))
		addKnownVariant(unique, base64.RawURLEncoding.EncodeToString([]byte(secret)))
		addKnownVariant(unique, url.QueryEscape(secret))
		addKnownVariant(unique, lowerPercentEscapes(url.QueryEscape(secret)))
		addKnownVariant(unique, url.PathEscape(secret))
		addKnownVariant(unique, lowerPercentEscapes(url.PathEscape(secret)))
		if encoded, err := json.Marshal(secret); err == nil && len(encoded) >= 2 {
			addKnownVariant(unique, string(encoded[1:len(encoded)-1]))
		}
		addKnownVariant(unique, strings.ReplaceAll(secret, `'`, `'\''`))
		addKnownVariant(unique, strings.ReplaceAll(strings.ReplaceAll(secret, `\`, `\\`), `"`, `\"`))
	}
	values := make([]string, 0, len(unique))
	for value := range unique {
		values = append(values, value)
	}
	sort.Slice(values, func(left, right int) bool { return len(values[left]) > len(values[right]) })
	redactor := &Redactor{known: make([]knownReplacement, 0, len(values))}
	for _, value := range values {
		redactor.known = append(redactor.known, knownReplacement{value: []byte(value), mask: bytes.Repeat([]byte{'*'}, len(value))})
	}
	encodedPatterns := make(map[string]struct{}, len(secrets)*2)
	for _, secret := range secrets {
		for _, encoded := range []string{url.QueryEscape(secret), url.PathEscape(secret)} {
			if secret != "" && strings.Contains(encoded, "%") {
				encodedPatterns[encoded] = struct{}{}
			}
		}
	}
	for encoded := range encodedPatterns {
		redactor.knownEncoded = append(redactor.knownEncoded, encodedReplacement{
			pattern: regexp.MustCompile(`(?i)` + regexp.QuoteMeta(encoded)),
			mask:    bytes.Repeat([]byte{'*'}, len(encoded)),
		})
	}
	return redactor
}

func (redactor *Redactor) Redact(input []byte) []byte {
	output := append([]byte(nil), input...)
	if redactor != nil {
		for _, secret := range redactor.known {
			output = bytes.ReplaceAll(output, secret.value, secret.mask)
		}
		for _, encoded := range redactor.knownEncoded {
			output = encoded.pattern.ReplaceAll(output, encoded.mask)
		}
	}
	output = privateKeyPattern.ReplaceAll(output, []byte(redactionMarker))
	output = urlUserPattern.ReplaceAll(output, []byte(`${1}`+redactionMarker+`@`))
	output = headerPattern.ReplaceAll(output, []byte(`${1}`+redactionMarker))
	output = jsonSecretPattern.ReplaceAll(output, []byte(`${1}`+redactionMarker+`${3}`))
	output = singleQuotedSecretPattern.ReplaceAll(output, []byte(`${1}'`+redactionMarker+`'`))
	output = doubleQuotedSecretPattern.ReplaceAll(output, []byte(`${1}"`+redactionMarker+`"`))
	output = textSecretPattern.ReplaceAll(output, []byte(`${1}`+redactionMarker))
	return output
}

func addKnownVariant(values map[string]struct{}, value string) {
	if value != "" {
		values[value] = struct{}{}
	}
}

func lowerPercentEscapes(value string) string {
	bytes := []byte(value)
	for index := 0; index+2 < len(bytes); index++ {
		if bytes[index] != '%' {
			continue
		}
		for offset := 1; offset <= 2; offset++ {
			if bytes[index+offset] >= 'A' && bytes[index+offset] <= 'F' {
				bytes[index+offset] += 'a' - 'A'
			}
		}
		index += 2
	}
	return string(bytes)
}
