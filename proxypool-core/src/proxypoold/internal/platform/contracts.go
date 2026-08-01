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

// NodeRequest carries one scheduler-owned attempt. Callers must treat the
// embedded node as secret-bearing configuration and never log the value.
type NodeRequest struct {
	Node       model.Node
	JobID      string
	Generation uint64
}

// Session is the credential-free ownership evidence returned by a protocol
// adapter. A partially populated session is still sufficient for best-effort
// cleanup after a failed or timed-out Start.
type Session struct {
	NodeID          string
	Generation      uint64
	Protocol        model.Protocol
	Interface       string
	LocalPort       uint16
	StartedAt       time.Time
	OwnershipDigest string
}

// NodeAdapter owns protocol processes/interfaces. Start must not publish the
// node as usable; that is the scheduler's job after Probe and every gate pass.
type NodeAdapter interface {
	Start(context.Context, NodeRequest) (Session, error)
	Probe(context.Context, NodeRequest, Session) error
	Stop(context.Context, NodeRequest, Session) error
}

// SessionGate installs one fail-closed dataplane prerequisite such as routes,
// DNS, or client authorization. Open calls are ordered; cleanup is reversed.
type SessionGate interface {
	Open(context.Context, NodeRequest, Session) error
	Close(context.Context, NodeRequest, Session) error
}
