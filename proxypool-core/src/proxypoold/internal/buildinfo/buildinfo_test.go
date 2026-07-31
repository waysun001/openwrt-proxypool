package buildinfo_test

import (
	"strings"
	"testing"

	"proxypoold/internal/buildinfo"
)

func TestSchemaVersionIsV2(t *testing.T) {
	if buildinfo.SchemaVersion != 2 {
		t.Fatalf("SchemaVersion=%d want 2", buildinfo.SchemaVersion)
	}
	if strings.TrimSpace(buildinfo.Version) == "" {
		t.Fatal("Version must not be empty")
	}
}
