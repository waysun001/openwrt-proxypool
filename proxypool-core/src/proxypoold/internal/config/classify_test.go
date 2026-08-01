package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyRecognizesStrictV2(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("testdata", "v2-valid.uci"))
	if err != nil {
		t.Fatal(err)
	}

	inspection := Classify(contents)
	if inspection.State() != ConfigReady {
		t.Fatalf("state = %q, want %q", inspection.State(), ConfigReady)
	}
	desired, ok := inspection.Desired()
	if !ok {
		t.Fatal("ready inspection did not expose desired configuration")
	}
	if desired.SchemaVersion != 2 || desired.Revision != 9 || len(desired.Nodes) != 2 || len(desired.Devices) != 2 {
		t.Fatalf("unexpected desired summary: schema=%d revision=%d nodes=%d devices=%d", desired.SchemaVersion, desired.Revision, len(desired.Nodes), len(desired.Devices))
	}
}

func TestClassifyRecognizesOnlyStructuredLegacyV1(t *testing.T) {
	legacy := []byte(`# a mention in a comment is not a V2 declaration: option schema_version '2'
config global 'global'
	option enabled '1'
	option max_clients '60'
	option log_level 'info'
	option lease_days '360'
	option lease_used '12'

config client 'old_l2tp'
	option enabled '1'
	option name 'old node'
	option type 'l2tp'
	option server 'vpn.example.com'
	option port '1701'
	option username 'alice'
	option password 'legacy-secret'
	option expiry ''
	list bind_ip '192.168.9.10'
`)

	inspection := Classify(legacy)
	if inspection.State() != ConfigMigrationRequired {
		t.Fatalf("state = %q, want %q", inspection.State(), ConfigMigrationRequired)
	}
	if _, ok := inspection.Desired(); ok {
		t.Fatal("legacy configuration must not be presented as decoded V2 desired state")
	}
}

func TestClassifyRecognizesCompleteLuCILegacyClientShape(t *testing.T) {
	legacy := []byte(`config global 'global'
	option enabled '1'
	option max_clients '60'
	option log_level 'info'
	option lease_days '360'
	option lease_used '12'

config client 'luci_client'
	option enabled '1'
	option name 'LuCI node'
	option type 'slp'
	option server 'proxy.example.com'
	option port '443'
	option username 'alice'
	option password 'legacy-password'
	option expiry '2027-01-01'
	option remark 'office router'
	option slp_token 'legacy-token'
	option slp_transport 'quic'
	option slp_obfs '1'
	option slp_obfs_key 'legacy-obfs-key'
	option slp_insecure '0'
	list bind_ip '192.168.9.10'
`)

	if got := Classify(legacy).State(); got != ConfigMigrationRequired {
		t.Fatalf("state = %q, want %q", got, ConfigMigrationRequired)
	}
}

func TestClassifyRecognizesExplicitV1BackendAsLegacy(t *testing.T) {
	legacy := []byte("config global 'global'\n\toption enabled '1'\n\toption runtime_backend 'v1'\n\toption max_clients '60'\n")
	if got := Classify(legacy).State(); got != ConfigMigrationRequired {
		t.Fatalf("state = %q, want %q", got, ConfigMigrationRequired)
	}
}

func TestClassifyRejectsEmptyUnknownAndDeclaredButBrokenV2(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "generic global is not genuine v1", data: []byte("config global 'global'\n\toption enabled '1'\n")},
		{name: "unknown section", data: []byte("config mystery 'x'\n\toption enabled '1'\n")},
		{name: "unknown legacy option", data: []byte("config global 'global'\n\toption enabled '1'\n\toption surprise '1'\n")},
		{name: "unknown legacy client option", data: []byte("config global 'global'\n\toption max_clients '60'\nconfig client 'old'\n\toption future_option '1'\n")},
		{name: "malformed uci", data: []byte("config global 'global\n")},
		{name: "unknown schema", data: []byte("config global 'global'\n\toption schema_version '99'\n")},
		{name: "v2 marker without schema", data: []byte("config global 'global'\n\toption runtime_backend 'v2_shadow'\n")},
		{name: "declared v2 missing required fields", data: []byte("config global 'global'\n\toption schema_version '2'\n\toption revision '7'\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := Classify(test.data)
			if inspection.State() != ConfigInvalid {
				t.Fatalf("state = %q, want %q", inspection.State(), ConfigInvalid)
			}
			if _, ok := inspection.Desired(); ok {
				t.Fatal("invalid configuration exposed desired state")
			}
		})
	}
}

func TestInspectFileIsReadOnly(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("testdata", "v2-valid.uci"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "proxypool")
	if err := os.WriteFile(path, contents, 0o640); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	inspection := InspectFile(path)
	if inspection.State() != ConfigReady {
		t.Fatalf("state = %q, want %q", inspection.State(), ConfigReady)
	}

	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatal("read-only inspection changed configuration bytes")
	}
	if beforeInfo.Mode() != afterInfo.Mode() {
		t.Fatalf("mode changed from %v to %v", beforeInfo.Mode(), afterInfo.Mode())
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("mtime changed from %v to %v", beforeInfo.ModTime(), afterInfo.ModTime())
	}
	desired, ok := inspection.Desired()
	if !ok || desired.Revision != 9 {
		t.Fatalf("revision changed or disappeared: ok=%t revision=%d", ok, desired.Revision)
	}
}

func TestInspectFileReadFailureIsSanitizedInvalidState(t *testing.T) {
	inspection := InspectFile(filepath.Join(t.TempDir(), "missing"))
	if inspection.State() != ConfigInvalid {
		t.Fatalf("state = %q, want %q", inspection.State(), ConfigInvalid)
	}
	if got := inspection.String(); got != "config.Inspection{State:\"invalid_config\" Desired:<unavailable>}" {
		t.Fatalf("String() = %q", got)
	}
}

func TestInspectEnabledFileUsesOnlyRecognizedConfigShapes(t *testing.T) {
	dir := t.TempDir()
	v2Contents, err := os.ReadFile(filepath.Join("testdata", "v2-valid.uci"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		contents string
		want     bool
		wantOK   bool
	}{
		{name: "legacy default enabled", contents: "config global 'global'\n\toption max_clients '60'\n", want: true, wantOK: true},
		{name: "legacy disabled", contents: "config global 'global'\n\toption enabled '0'\n\toption max_clients '60'\n", wantOK: true},
		{name: "strict V2 disabled", contents: strings.Replace(string(v2Contents), "option enabled '1'", "option enabled '0'", 1), wantOK: true},
		{name: "unknown shape", contents: "config mystery 'x'\n\toption enabled '1'\n"},
		{name: "declared invalid V2", contents: "config global 'global'\n\toption schema_version '2'\n\toption enabled '0'\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(test.name, " ", "-"))
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok := InspectEnabledFile(path)
			if got != test.want || ok != test.wantOK {
				t.Fatalf("enabled=%t,%t want %t,%t", got, ok, test.want, test.wantOK)
			}
		})
	}
}
