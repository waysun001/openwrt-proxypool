// Package importer parses legacy ProxyPool node lists without exposing their
// credentials to HTTP or LuCI preview responses.
package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"proxypoold/internal/model"
)

const (
	MaxImportBytes   = 1 << 20
	MaxImportRecords = 60

	ErrorInvalidProtocol  = "invalid_protocol"
	ErrorInvalidFields    = "invalid_fields"
	ErrorInvalidServer    = "invalid_server"
	ErrorInvalidPort      = "invalid_port"
	ErrorInvalidExpiry    = "invalid_expiry"
	ErrorInvalidCharacter = "invalid_character"
	ErrorInvalidSecret    = "invalid_secret"
	ErrorDuplicate        = "duplicate"
	ErrorCapacityExceeded = "capacity_exceeded"
	ErrorRequestTooLarge  = "request_too_large"
)

type Candidate struct {
	Line         int
	Protocol     model.Protocol
	Server       string
	Port         uint16
	Username     string `json:"-"`
	Password     string `json:"-"`
	SLPToken     string `json:"-"`
	SLPTransport string
	ExpiresAt    *time.Time
}

func (candidate Candidate) String() string {
	return fmt.Sprintf("importer.Candidate{Line:%d Protocol:%q Server:%q Port:%d Username:<redacted> Password:<redacted> SLPToken:<redacted> SLPTransport:%q ExpiresAt:%v}",
		candidate.Line, candidate.Protocol, candidate.Server, candidate.Port, candidate.SLPTransport, candidate.ExpiresAt)
}

func (candidate Candidate) GoString() string { return candidate.String() }

type LineError struct {
	Line    int    `json:"line"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ParseResult struct {
	Nodes  []Candidate
	Errors []LineError
}

type SanitizedRow struct {
	Line         int            `json:"line"`
	Protocol     model.Protocol `json:"protocol"`
	Server       string         `json:"server"`
	Port         uint16         `json:"port"`
	SLPTransport string         `json:"slp_transport,omitempty"`
	ExpiresAt    string         `json:"expires_at,omitempty"`
	SecretSet    bool           `json:"secret_set"`
}

func SanitizedRows(result ParseResult) []SanitizedRow {
	rows := make([]SanitizedRow, 0, len(result.Nodes))
	for _, candidate := range result.Nodes {
		row := SanitizedRow{
			Line: candidate.Line, Protocol: candidate.Protocol, Server: candidate.Server, Port: candidate.Port,
			SLPTransport: candidate.SLPTransport, SecretSet: candidate.Password != "" || candidate.SLPToken != "",
		}
		if candidate.ExpiresAt != nil {
			row.ExpiresAt = candidate.ExpiresAt.Format("2006-01-02")
		}
		rows = append(rows, row)
	}
	return rows
}

func Parse(protocol model.Protocol, raw string) ParseResult {
	if len(raw) > MaxImportBytes {
		return ParseResult{Errors: []LineError{{Code: ErrorRequestTooLarge, Message: "导入内容超过 1 MiB 限制"}}}
	}
	if protocol != model.ProtocolL2TP && protocol != model.ProtocolSOCKS5 && protocol != model.ProtocolSLP {
		return ParseResult{Errors: []LineError{{Code: ErrorInvalidProtocol, Message: "导入协议不受支持"}}}
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	records := 0
	for _, line := range lines {
		if strings.TrimSpace(strings.TrimSuffix(line, "\r")) != "" {
			records++
		}
	}
	if records > MaxImportRecords {
		return ParseResult{Errors: []LineError{{Code: ErrorCapacityExceeded, Message: "单次最多导入 60 个节点"}}}
	}

	result := ParseResult{Nodes: make([]Candidate, 0, records)}
	seen := make(map[string]struct{}, records)
	for index, source := range lines {
		lineNumber := index + 1
		source = strings.TrimSuffix(source, "\r")
		if strings.TrimSpace(source) == "" {
			continue
		}
		if hasControl(source) {
			result.Errors = append(result.Errors, lineError(lineNumber, ErrorInvalidCharacter, "字段包含不允许的控制字符"))
			continue
		}
		candidate, issue := parseLine(protocol, source, lineNumber)
		if issue != nil {
			result.Errors = append(result.Errors, *issue)
			continue
		}
		key := naturalKey(candidate)
		if _, exists := seen[key]; exists {
			result.Errors = append(result.Errors, lineError(lineNumber, ErrorDuplicate, "导入内容包含重复节点"))
			continue
		}
		seen[key] = struct{}{}
		result.Nodes = append(result.Nodes, candidate)
	}
	return result
}

func parseLine(protocol model.Protocol, source string, line int) (Candidate, *LineError) {
	fields := strings.Split(source, "|")
	candidate := Candidate{Line: line, Protocol: protocol}
	switch protocol {
	case model.ProtocolL2TP:
		if len(fields) < 3 || len(fields) > 5 {
			return Candidate{}, issue(line, ErrorInvalidFields, "L2TP 字段数量不正确")
		}
		candidate.Server = normalizeServer(fields[0])
		candidate.Port = 1701
		switch len(fields) {
		case 3:
			candidate.Username, candidate.Password = fields[1], fields[2]
		case 4:
			if port, ok := parsePort(fields[1]); ok {
				candidate.Port, candidate.Username, candidate.Password = port, fields[2], fields[3]
			} else {
				candidate.Username, candidate.Password = fields[1], fields[2]
				expires, err := parseExpiry(fields[3])
				if err != nil {
					return Candidate{}, issue(line, ErrorInvalidExpiry, "到期日期必须使用 YYYY-MM-DD")
				}
				candidate.ExpiresAt = expires
			}
		case 5:
			port, ok := parsePort(fields[1])
			if !ok {
				return Candidate{}, issue(line, ErrorInvalidPort, "端口必须是 1 到 65535 的整数")
			}
			candidate.Port, candidate.Username, candidate.Password = port, fields[2], fields[3]
			expires, err := parseExpiry(fields[4])
			if err != nil {
				return Candidate{}, issue(line, ErrorInvalidExpiry, "到期日期必须使用 YYYY-MM-DD")
			}
			candidate.ExpiresAt = expires
		}
	case model.ProtocolSOCKS5:
		if len(fields) != 5 {
			return Candidate{}, issue(line, ErrorInvalidFields, "SOCKS5 字段数量不正确")
		}
		candidate.Server = normalizeServer(fields[0])
		port, ok := parsePort(fields[1])
		if !ok {
			return Candidate{}, issue(line, ErrorInvalidPort, "端口必须是 1 到 65535 的整数")
		}
		candidate.Port, candidate.Username, candidate.Password = port, fields[2], fields[3]
		expires, err := parseExpiry(fields[4])
		if err != nil {
			return Candidate{}, issue(line, ErrorInvalidExpiry, "到期日期必须使用 YYYY-MM-DD")
		}
		candidate.ExpiresAt = expires
	case model.ProtocolSLP:
		if len(fields) != 4 {
			return Candidate{}, issue(line, ErrorInvalidFields, "SLP 字段数量不正确")
		}
		candidate.Server = normalizeServer(fields[0])
		port, ok := parsePort(fields[1])
		if !ok {
			return Candidate{}, issue(line, ErrorInvalidPort, "端口必须是 1 到 65535 的整数")
		}
		candidate.Port, candidate.SLPToken, candidate.SLPTransport = port, fields[2], strings.ToLower(strings.TrimSpace(fields[3]))
		if candidate.SLPTransport != "quic" {
			return Candidate{}, issue(line, ErrorInvalidFields, "SLP transport 仅支持 quic")
		}
	}
	if !validServer(candidate.Server) {
		return Candidate{}, issue(line, ErrorInvalidServer, "服务器地址无效")
	}
	if !validSecret(candidate.Username, 256, true) || !validSecret(candidate.Password, 1024, true) || !validSecret(candidate.SLPToken, 4096, true) {
		return Candidate{}, issue(line, ErrorInvalidSecret, "认证字段为空、过长或包含非法字符")
	}
	switch protocol {
	case model.ProtocolL2TP:
		if candidate.Username == "" || candidate.Password == "" || strings.ContainsAny(candidate.Username, "\"\\") || strings.ContainsAny(candidate.Password, "\"\\") {
			return Candidate{}, issue(line, ErrorInvalidSecret, "L2TP 用户名或密码为空或包含不支持字符")
		}
	case model.ProtocolSOCKS5:
		if (candidate.Username == "") != (candidate.Password == "") {
			return Candidate{}, issue(line, ErrorInvalidSecret, "SOCKS5 用户名和密码必须同时填写")
		}
	case model.ProtocolSLP:
		if candidate.SLPToken == "" {
			return Candidate{}, issue(line, ErrorInvalidSecret, "SLP token 不能为空")
		}
	}
	return candidate, nil
}

func parsePort(value string) (uint16, bool) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 16)
	return uint16(parsed), err == nil && parsed > 0
}

func parseExpiry(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return nil, fmt.Errorf("invalid expiry")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func normalizeServer(value string) string {
	value = strings.TrimSpace(value)
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap().String()
	}
	return strings.ToLower(strings.TrimSuffix(value, "."))
}

func validServer(value string) bool {
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Is4() && address.IsGlobalUnicast() && !address.IsLoopback() && !address.IsLinkLocalUnicast()
	}
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return net.ParseIP(value) == nil
}

func validSecret(value string, limit int, optional bool) bool {
	if value == "" {
		return optional
	}
	return len(value) <= limit && utf8.ValidString(value) && !hasControl(value) && !strings.ContainsAny(value, "|\r\n")
}

func hasControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func naturalKey(candidate Candidate) string {
	identity := candidate.Username
	if candidate.Protocol == model.ProtocolSLP {
		digest := sha256.Sum256([]byte(candidate.SLPToken))
		identity = hex.EncodeToString(digest[:])
	}
	return string(candidate.Protocol) + "\x00" + candidate.Server + "\x00" + strconv.Itoa(int(candidate.Port)) + "\x00" + identity
}

func lineError(line int, code, message string) LineError {
	return LineError{Line: line, Code: code, Message: message}
}

func issue(line int, code, message string) *LineError {
	value := lineError(line, code, message)
	return &value
}
