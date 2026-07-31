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

// Inspection intentionally keeps the decoded model private. This prevents a
// startup classification (which may contain credentials) from being embedded
// directly in control responses or logs.
type Inspection struct {
	state   ConfigState
	desired model.DesiredConfig
	ready   bool
}

func (inspection Inspection) State() ConfigState { return inspection.state }

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
	if err != nil || len(sections) == 0 || declaresV2(sections) || !validLegacyV1(sections) {
		return Inspection{state: ConfigInvalid}
	}
	return Inspection{state: ConfigMigrationRequired}
}

func declaresV2(sections []*uciSection) bool {
	for _, section := range sections {
		if section.kind == "node" || section.kind == "device" {
			return true
		}
		if section.kind == "global" {
			if _, exists := section.options["schema_version"]; exists {
				return true
			}
			if backend, exists := section.options["runtime_backend"]; exists && backend != "v1" {
				return true
			}
		}
	}
	return false
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
