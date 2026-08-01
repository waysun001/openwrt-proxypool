package config

import (
	"bytes"
	"errors"
	"os"
)

type RuntimeSelection string

const (
	RuntimeSelectionMissing  RuntimeSelection = "missing"
	RuntimeSelectionV1       RuntimeSelection = "v1"
	RuntimeSelectionV2Shadow RuntimeSelection = "v2_shadow"
	RuntimeSelectionUnknown  RuntimeSelection = "unknown"
)

func InspectRuntimeSelectorFile(path string) RuntimeSelection {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeSelectionMissing
	}
	if err != nil {
		return RuntimeSelectionUnknown
	}
	return ClassifyRuntimeSelector(contents)
}

func ClassifyRuntimeSelector(contents []byte) RuntimeSelection {
	sections, err := parseUCI(bytes.NewReader(contents))
	if err != nil || len(sections) != 1 {
		return RuntimeSelectionUnknown
	}
	section := sections[0]
	if section.kind != "global" || section.name != "global" || len(section.lists) != 0 || len(section.options) != 1 {
		return RuntimeSelectionUnknown
	}
	switch section.options["runtime_backend"] {
	case "v1":
		return RuntimeSelectionV1
	case "v2_shadow":
		return RuntimeSelectionV2Shadow
	default:
		return RuntimeSelectionUnknown
	}
}
