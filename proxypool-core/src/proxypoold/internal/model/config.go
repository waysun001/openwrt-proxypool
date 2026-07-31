package model

import (
	"net/netip"
	"time"
)

type Protocol string

const (
	ProtocolL2TP   Protocol = "l2tp"
	ProtocolSOCKS5 Protocol = "socks5"
	ProtocolSLP    Protocol = "slp"
)

type DesiredConfig struct {
	SchemaVersion int
	Revision      uint64
	Global        GlobalConfig
	Nodes         map[string]Node
	Devices       map[string]Device
}

type GlobalConfig struct {
	Enabled          bool
	RuntimeBackend   string
	MaxNodes         int
	LANDevice        string
	ManagementPorts  []uint16
	L2TPConcurrency  int
	ProxyConcurrency int
	ConnectTimeout   time.Duration
	StopTimeout      time.Duration
	DoHEndpoints     []DoHEndpoint
}

type Node struct {
	ID, Name     string
	Protocol     Protocol
	Enabled      bool
	Server       string
	Port         uint16
	Username     string
	Password     string `json:"-"`
	SLPToken     string `json:"-"`
	SLPTransport string
	SLPObfs      bool
	SLPObfsKey   string `json:"-"`
	SLPInsecure  bool
	ExpiresAt    *time.Time
	PolicyID     uint16
	Revision     uint64
}

type DoHEndpoint struct {
	URL, BootstrapIP, ServerName string
}

type Device struct {
	ID, MAC, Hostname string
	FixedIPv4         netip.Addr
	NodeID            string
	Enabled           bool
}
