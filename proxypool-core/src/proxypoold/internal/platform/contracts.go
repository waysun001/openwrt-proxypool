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

type InputCommandRunner interface {
	CommandRunner
	RunInput(context.Context, []byte, string, ...string) ([]byte, error)
}

type DeviceSource interface {
	List(context.Context) ([]DiscoveredDevice, error)
}

type LeaseManager interface {
	Apply(context.Context, model.Device, uint64) error
	Remove(context.Context, model.Device, uint64) error
}

type InterfaceCounters struct {
	RXBytes uint64
	TXBytes uint64
}

type InterfaceTrafficReader interface {
	ReadInterfaceCounters(interfaceName string) (InterfaceCounters, error)
}

// WANStatusSource returns the authoritative routed-uplink state. An error is
// treated as unavailable so new sessions remain fail-closed.
type WANStatusSource interface {
	Available(context.Context) (bool, error)
}

// LeaseProjectionManager atomically replaces a complete set of owned DHCP
// reservations. It is separate from LeaseManager so existing single-device
// integrations retain their API contract.
type LeaseProjectionManager interface {
	LeaseManager
	Replace(context.Context, []model.Device, []model.Device, uint64) error
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
	RemoteAddress   string
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

// SessionGateVerifier is implemented by gates whose live dataplane state can
// be re-proved without mutating it. The scheduler treats any failed proof as a
// fail-closed session failure and revokes authorization before reconnecting.
type SessionGateVerifier interface {
	Verify(context.Context, NodeRequest, Session) error
}

type AuthorizationLease struct {
	NodeID       string
	MAC          string
	IPv4         netip.Addr
	PolicyID     uint16
	Generation   uint64
	Protocol     model.Protocol
	Interface    string
	RedirectPort uint16
	ExpiresAt    time.Time
}

type Authorizer interface {
	Publish(context.Context, AuthorizationLease) error
	RevokeNode(context.Context, string, uint64) error
	RevokeAll(context.Context) error
}

type RouteLease struct {
	NodeID     string
	PolicyID   uint16
	Generation uint64
	Interface  string
}

type RouteManager interface {
	Install(context.Context, RouteLease) error
	Verify(context.Context, RouteLease) error
	Remove(context.Context, RouteLease) error
}
