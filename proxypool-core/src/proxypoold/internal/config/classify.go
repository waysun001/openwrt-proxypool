package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"proxypoold/internal/model"
)

// ConfigState is the read-only startup classification of an on-disk UCI file.
type ConfigState string

const (
	ConfigReady             ConfigState = "ready"
	ConfigMigrationRequired ConfigState = "migration_required"
	ConfigInvalid           ConfigState = "invalid_config"
)

// StartupClass is the only secret-free init selection emitted by
// proxypoolctl. Unknown is deliberately distinct from a declared but invalid
// V2 file so init never falls back to the mutating legacy backend.
type StartupClass string

const (
	StartupV1              StartupClass = "v1"
	StartupV2Shadow        StartupClass = "v2_shadow"
	StartupV2ShadowInvalid StartupClass = "v2_shadow_invalid"
	StartupUnknown         StartupClass = "unknown"
)

// Inspection intentionally keeps the decoded model private. This prevents a
// startup classification (which may contain credentials) from being embedded
// directly in control responses or logs.
type Inspection struct {
	state      ConfigState
	desired    model.DesiredConfig
	ready      bool
	declaredV2 bool
}

func (inspection Inspection) State() ConfigState { return inspection.state }

func (inspection Inspection) StartupClass() StartupClass {
	if inspection.ready {
		return StartupV2Shadow
	}
	if inspection.state == ConfigMigrationRequired {
		return StartupV1
	}
	if inspection.declaredV2 {
		return StartupV2ShadowInvalid
	}
	return StartupUnknown
}

// Desired returns an isolated copy only for a strict, fully validated V2 file.
func (inspection Inspection) Desired() (model.DesiredConfig, bool) {
	if !inspection.ready {
		return model.DesiredConfig{}, false
	}
	return cloneConfig(inspection.desired), true
}

func (inspection Inspection) String() string {
	if inspection.ready {
		return fmt.Sprintf("config.Inspection{State:%q Desired:<redacted>}", inspection.state)
	}
	return fmt.Sprintf("config.Inspection{State:%q Desired:<unavailable>}", inspection.state)
}

func (inspection Inspection) GoString() string { return inspection.String() }

func (inspection Inspection) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, inspection.String())
}

// InspectFile reads without opening the configuration for write and never
// returns raw parser or filesystem errors to the caller.
func InspectFile(path string) Inspection {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Inspection{state: ConfigInvalid}
	}
	return Classify(contents)
}

// InspectEnabledFile returns the effective global enabled flag only for a
// structurally recognized V1 file or a fully validated V2 file. It never
// exposes any other configuration value.
func InspectEnabledFile(path string) (bool, bool) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	inspection := Classify(contents)
	if inspection.ready {
		return inspection.desired.Global.Enabled, true
	}
	if inspection.state != ConfigMigrationRequired {
		return false, false
	}
	sections, err := parseUCI(bytes.NewReader(contents))
	if err != nil {
		return false, false
	}
	for _, section := range sections {
		if section.kind == "global" && section.name == "global" {
			return section.options["enabled"] != "0", true
		}
	}
	return false, false
}

// Classify distinguishes strict V2 from the structurally known V1 UCI shape.
// Anything else is invalid; in particular a V2 declaration never falls back
// to a legacy interpretation after strict decoding fails.
func Classify(contents []byte) Inspection {
	if len(contents) == 0 || !utf8.Valid(contents) {
		return Inspection{state: ConfigInvalid}
	}
	if desired, err := Decode(bytes.NewReader(contents)); err == nil {
		return Inspection{state: ConfigReady, desired: desired, ready: true}
	}
	sections, err := parseUCI(bytes.NewReader(contents))
	if err != nil || len(sections) == 0 {
		return Inspection{state: ConfigInvalid}
	}
	if declaresV2(sections) {
		return Inspection{state: ConfigInvalid, declaredV2: true}
	}
	if !validLegacyV1(sections) {
		return Inspection{state: ConfigInvalid}
	}
	return Inspection{state: ConfigMigrationRequired}
}

func declaresV2(sections []*uciSection) bool {
	declared := false
	for _, section := range sections {
		if section.kind == "global" {
			if schema, exists := section.options["schema_version"]; exists {
				if schema != "2" {
					return false
				}
				declared = true
			}
			if backend, exists := section.options["runtime_backend"]; exists {
				switch backend {
				case "v2_shadow":
					declared = true
				case "v1":
					// A schema-2 file cannot activate V1, but the schema marker
					// still makes it a safe diagnostic-shadow candidate.
				default:
					return false
				}
			}
		}
	}
	for _, section := range sections {
		if section.kind == "node" || section.kind == "device" {
			declared = true
		}
	}
	return declared
}

var legacyGlobalOptions = []string{"enabled", "runtime_backend", "max_clients", "log_level", "lease_days", "lease_used"}
var legacyClientOptions = []string{"enabled", "name", "type", "server", "port", "username", "password", "expiry", "slp_token", "slp_transport", "slp_obfs", "slp_obfs_key", "slp_insecure"}
var legacyClientLists = []string{"bind_ip"}

func validLegacyV1(sections []*uciSection) bool {
	globalSeen := false
	legacyMarkerSeen := false
	clientNames := make(map[string]struct{})
	for _, section := range sections {
		switch section.kind {
		case "global":
			if globalSeen || section.name != "global" || len(section.lists) != 0 || !onlyKeys(section.options, legacyGlobalOptions) {
				return false
			}
			if backend, exists := section.options["runtime_backend"]; exists && backend != "v1" {
				return false
			}
			for _, marker := range []string{"max_clients", "log_level", "lease_days", "lease_used"} {
				if _, exists := section.options[marker]; exists {
					legacyMarkerSeen = true
				}
			}
			globalSeen = true
		case "client":
			if !safeUCISectionName(section.name) || !onlyKeys(section.options, legacyClientOptions) || !onlyKeys(section.lists, legacyClientLists) {
				return false
			}
			if _, exists := clientNames[section.name]; exists {
				return false
			}
			clientNames[section.name] = struct{}{}
			legacyMarkerSeen = true
		default:
			return false
		}
	}
	return globalSeen && legacyMarkerSeen
}
