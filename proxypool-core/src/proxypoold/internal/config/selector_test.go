package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyRuntimeSelectorIsStrict(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     RuntimeSelection
	}{
		{name: "v1", contents: "config global 'global'\n\toption runtime_backend 'v1'\n", want: RuntimeSelectionV1},
		{name: "v2 shadow", contents: "config global 'global'\n\toption runtime_backend 'v2_shadow'\n", want: RuntimeSelectionV2Shadow},
		{name: "unknown backend", contents: "config global 'global'\n\toption runtime_backend 'surprise'\n", want: RuntimeSelectionUnknown},
		{name: "extra option", contents: "config global 'global'\n\toption runtime_backend 'v1'\n\toption enabled '1'\n", want: RuntimeSelectionUnknown},
		{name: "extra section", contents: "config global 'global'\n\toption runtime_backend 'v1'\nconfig other 'x'\n\toption value 'x'\n", want: RuntimeSelectionUnknown},
		{name: "wrong section name", contents: "config global 'runtime'\n\toption runtime_backend 'v1'\n", want: RuntimeSelectionUnknown},
		{name: "list", contents: "config global 'global'\n\tlist runtime_backend 'v1'\n", want: RuntimeSelectionUnknown},
		{name: "empty", contents: "", want: RuntimeSelectionUnknown},
		{name: "malformed", contents: "config global 'global\n", want: RuntimeSelectionUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyRuntimeSelector([]byte(test.contents)); got != test.want {
				t.Fatalf("selection=%q want %q", got, test.want)
			}
		})
	}
}

func TestInspectRuntimeSelectorFileDistinguishesMissingFromUnknown(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if got := InspectRuntimeSelectorFile(missing); got != RuntimeSelectionMissing {
		t.Fatalf("missing selection=%q", got)
	}
	directory := filepath.Join(dir, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := InspectRuntimeSelectorFile(directory); got != RuntimeSelectionUnknown {
		t.Fatalf("directory selection=%q", got)
	}
}
