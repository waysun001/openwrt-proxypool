package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"

	"proxypoold/internal/platform"
)

type WANStatusSource struct {
	runner platform.CommandRunner
}

func NewWANStatusSource(runner platform.CommandRunner) *WANStatusSource {
	return &WANStatusSource{runner: runner}
}

func (source *WANStatusSource) Available(ctx context.Context) (bool, error) {
	if source == nil || source.runner == nil {
		return false, errors.New("WAN status source is unavailable")
	}
	output, err := source.runner.Run(ctx, "/bin/ubus", "call", "network.interface.wan", "status")
	if err != nil {
		return false, errors.New("WAN status query failed")
	}
	var status struct {
		Up        bool   `json:"up"`
		Pending   bool   `json:"pending"`
		Available bool   `json:"available"`
		L3Device  string `json:"l3_device"`
		IPv4      []struct {
			Address string `json:"address"`
		} `json:"ipv4-address"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return false, errors.New("WAN status response is invalid")
	}
	if !status.Up || status.Pending || !status.Available || status.L3Device == "" {
		return false, nil
	}
	for _, candidate := range status.IPv4 {
		address, err := netip.ParseAddr(candidate.Address)
		if err == nil && address.Is4() && address.IsGlobalUnicast() && !address.IsLoopback() && !address.IsLinkLocalUnicast() {
			return true, nil
		}
	}
	return false, nil
}

var _ platform.WANStatusSource = (*WANStatusSource)(nil)
