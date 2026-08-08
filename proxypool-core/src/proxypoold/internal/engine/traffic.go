package engine

import (
	"sync"
	"time"

	"proxypoold/internal/platform"
)

type TrafficSnapshot struct {
	DownloadBytes          uint64 `json:"download_bytes"`
	UploadBytes            uint64 `json:"upload_bytes"`
	DownloadBytesPerSecond uint64 `json:"download_bytes_per_second"`
	UploadBytesPerSecond   uint64 `json:"upload_bytes_per_second"`
	SampledAt              string `json:"sampled_at,omitempty"`
}

type trafficSession struct {
	generation    uint64
	interfaceName string
	lastCounters  platform.InterfaceCounters
	lastSample    time.Time
	hasBaseline   bool
	snapshot      TrafficSnapshot
}

type trafficTracker struct {
	mu       sync.RWMutex
	sessions map[string]trafficSession
}

func newTrafficTracker() *trafficTracker {
	return &trafficTracker{sessions: make(map[string]trafficSession)}
}

func (tracker *trafficTracker) Begin(nodeID string, generation uint64, interfaceName string) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.sessions[nodeID] = trafficSession{generation: generation, interfaceName: interfaceName}
}

func (tracker *trafficTracker) End(nodeID string, generation uint64) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	session, exists := tracker.sessions[nodeID]
	if exists && session.generation == generation {
		delete(tracker.sessions, nodeID)
	}
}

func (tracker *trafficTracker) Sample(nodeID string, generation uint64, counters platform.InterfaceCounters, sampledAt time.Time) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	session, exists := tracker.sessions[nodeID]
	if !exists || session.generation != generation {
		return
	}
	session.snapshot.SampledAt = formatTrafficSampleTime(sampledAt)
	if !session.hasBaseline || !sampledAt.After(session.lastSample) || counters.RXBytes < session.lastCounters.RXBytes || counters.TXBytes < session.lastCounters.TXBytes {
		session.lastCounters = counters
		session.lastSample = sampledAt
		session.hasBaseline = true
		session.snapshot.DownloadBytesPerSecond = 0
		session.snapshot.UploadBytesPerSecond = 0
		tracker.sessions[nodeID] = session
		return
	}
	downloadDelta := counters.RXBytes - session.lastCounters.RXBytes
	uploadDelta := counters.TXBytes - session.lastCounters.TXBytes
	interval := sampledAt.Sub(session.lastSample)
	session.snapshot.DownloadBytes = saturatingAdd(session.snapshot.DownloadBytes, downloadDelta)
	session.snapshot.UploadBytes = saturatingAdd(session.snapshot.UploadBytes, uploadDelta)
	session.snapshot.DownloadBytesPerSecond = bytesPerSecond(downloadDelta, interval)
	session.snapshot.UploadBytesPerSecond = bytesPerSecond(uploadDelta, interval)
	session.lastCounters = counters
	session.lastSample = sampledAt
	tracker.sessions[nodeID] = session
}

func (tracker *trafficTracker) Unavailable(nodeID string, generation uint64, sampledAt time.Time) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	session, exists := tracker.sessions[nodeID]
	if !exists || session.generation != generation {
		return
	}
	session.hasBaseline = false
	session.snapshot.DownloadBytesPerSecond = 0
	session.snapshot.UploadBytesPerSecond = 0
	session.snapshot.SampledAt = formatTrafficSampleTime(sampledAt)
	tracker.sessions[nodeID] = session
}

func (tracker *trafficTracker) Snapshot(nodeID string) TrafficSnapshot {
	if tracker == nil {
		return TrafficSnapshot{}
	}
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	return tracker.sessions[nodeID].snapshot
}

func bytesPerSecond(bytes uint64, interval time.Duration) uint64 {
	if interval <= 0 {
		return 0
	}
	rate := float64(bytes) / interval.Seconds()
	maximum := ^uint64(0)
	if rate >= float64(maximum) {
		return maximum
	}
	return uint64(rate)
}

func saturatingAdd(left, right uint64) uint64 {
	maximum := ^uint64(0)
	if maximum-left < right {
		return maximum
	}
	return left + right
}

func formatTrafficSampleTime(sampledAt time.Time) string {
	if sampledAt.IsZero() {
		return ""
	}
	return sampledAt.UTC().Round(0).Format(time.RFC3339Nano)
}
