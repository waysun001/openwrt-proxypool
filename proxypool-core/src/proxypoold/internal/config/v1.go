package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"proxypoold/internal/model"
)

// ExportV1 emits the legacy client subset needed for rollback. It is
// credential-bearing by design and must only be written to a root-owned file.
func ExportV1(writer io.Writer, cfg model.DesiredConfig) error {
	if writer == nil || model.Validate(cfg) != nil {
		return errors.New("V1 export input is invalid")
	}
	var output strings.Builder
	writeSection(&output, "global", "global")
	writeOption(&output, "enabled", boolText(cfg.Global.Enabled))
	writeOption(&output, "max_clients", strconv.Itoa(cfg.Global.MaxNodes))

	bindings := make(map[string][]string, len(cfg.Nodes))
	for _, device := range cfg.Devices {
		if device.NodeID != "" {
			bindings[device.NodeID] = append(bindings[device.NodeID], device.FixedIPv4.String())
		}
	}
	for _, pending := range cfg.PendingBindings {
		bindings[pending.NodeID] = append(bindings[pending.NodeID], pending.LegacyIPv4.String())
	}
	for _, addresses := range bindings {
		sort.Strings(addresses)
	}

	for _, id := range sortedNodeIDs(cfg.Nodes) {
		node := cfg.Nodes[id]
		output.WriteByte('\n')
		writeSection(&output, "client", node.ID)
		writeOption(&output, "enabled", boolText(node.Enabled))
		writeOption(&output, "name", node.Name)
		writeOption(&output, "type", string(node.Protocol))
		writeOption(&output, "server", node.Server)
		writeOption(&output, "port", strconv.FormatUint(uint64(node.Port), 10))
		writeOption(&output, "username", node.Username)
		writeOption(&output, "password", node.Password)
		if node.ExpiresAt != nil {
			writeOption(&output, "expiry", node.ExpiresAt.UTC().Format("2006-01-02"))
		}
		writeOption(&output, "slp_token", node.SLPToken)
		writeOption(&output, "slp_transport", node.SLPTransport)
		writeOption(&output, "slp_obfs", boolText(node.SLPObfs))
		writeOption(&output, "slp_obfs_key", node.SLPObfsKey)
		writeOption(&output, "slp_insecure", boolText(node.SLPInsecure))
		for _, address := range bindings[node.ID] {
			writeList(&output, "bind_ip", address)
		}
	}
	if _, err := io.WriteString(writer, output.String()); err != nil {
		return errors.New("V1 export write failed")
	}
	return nil
}

type V1ExportResult struct {
	SHA256   string
	Nodes    int
	Bindings int
}

func ExportV1File(ctx context.Context, configPath, outputPath string) (V1ExportResult, error) {
	if contextError(ctx) != nil || !filepath.IsAbs(configPath) || !filepath.IsAbs(outputPath) || filepath.Clean(configPath) == filepath.Clean(outputPath) {
		return V1ExportResult{}, errors.New("V1 export file request is invalid")
	}
	cfg, err := NewStore(configPath).Load()
	if err != nil {
		return V1ExportResult{}, errors.New("V2 export source is unavailable")
	}
	var encoded bytes.Buffer
	if err := ExportV1(&encoded, cfg); err != nil {
		return V1ExportResult{}, err
	}
	if contextError(ctx) != nil {
		return V1ExportResult{}, errors.New("V1 export was cancelled")
	}
	if err := writePrivateAtomic(outputPath, encoded.Bytes()); err != nil {
		return V1ExportResult{}, errors.New("V1 export persistence failed")
	}
	return V1ExportResult{
		SHA256: sha256Text(encoded.Bytes()), Nodes: len(cfg.Nodes), Bindings: len(cfg.Devices) + len(cfg.PendingBindings),
	}, nil
}
