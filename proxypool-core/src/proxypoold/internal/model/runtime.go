package model

import (
	"net/netip"
	"time"
)

type RuntimeState string

const (
	StateDisabled   RuntimeState = "disabled"
	StateQueued     RuntimeState = "queued"
	StateStarting   RuntimeState = "starting"
	StateValidating RuntimeState = "validating"
	StateOnline     RuntimeState = "online"
	StateDegraded   RuntimeState = "degraded"
	StateStopping   RuntimeState = "stopping"
	StateFailed     RuntimeState = "failed"
	StateBackoff    RuntimeState = "backoff"
	StateRecovering RuntimeState = "recovering"
)

type NodeRuntime struct {
	State     RuntimeState
	LastError *CodeError
	RetryAt   time.Time
}

type DeviceRuntime struct {
	NodeID      string
	AllowedIPv4 netip.Addr
}
