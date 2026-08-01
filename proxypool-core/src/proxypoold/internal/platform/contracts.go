package platform

import (
	"context"
	"net/netip"
	"time"

	"proxypoold/internal/model"
)

type DiscoveredDevice struct {
	ID        string     `json:"id"`
	MAC       string     `json:"mac"`
	IPv4      netip.Addr `json:"ipv4"`
	Hostname  string     `json:"hostname,omitempty"`
	Ingress   string     `json:"ingress"`
	LastSeen  time.Time  `json:"last_seen"`
	Confirmed bool       `json:"confirmed"`
}

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type DeviceSource interface {
	List(context.Context) ([]DiscoveredDevice, error)
}

type LeaseManager interface {
	Apply(context.Context, model.Device, uint64) error
	Remove(context.Context, model.Device, uint64) error
}
