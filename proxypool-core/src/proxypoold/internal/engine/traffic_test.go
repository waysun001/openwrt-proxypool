package engine

import (
	"fmt"
	"testing"
	"time"

	"proxypoold/internal/platform"
)

func TestTrafficTrackerAccumulatesCurrentSessionWithRealIntervals(t *testing.T) {
	tracker := newTrafficTracker()
	epoch := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	tracker.Begin("node_a", 7, "l2tp-ppv20001")
	if got := tracker.Snapshot("node_a"); got != (TrafficSnapshot{}) {
		t.Fatalf("initial snapshot = %#v", got)
	}
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 100, TXBytes: 200}, epoch)
	first := tracker.Snapshot("node_a")
	if first.DownloadBytes != 0 || first.UploadBytes != 0 || first.DownloadBytesPerSecond != 0 || first.UploadBytesPerSecond != 0 || first.SampledAt != epoch.Format(time.RFC3339Nano) {
		t.Fatalf("first sample = %#v", first)
	}
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 350, TXBytes: 300}, epoch.Add(2*time.Second))
	second := tracker.Snapshot("node_a")
	if second.DownloadBytes != 250 || second.UploadBytes != 100 || second.DownloadBytesPerSecond != 125 || second.UploadBytesPerSecond != 50 {
		t.Fatalf("second sample = %#v", second)
	}
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 600, TXBytes: 550}, epoch.Add(4500*time.Millisecond))
	third := tracker.Snapshot("node_a")
	if third.DownloadBytes != 500 || third.UploadBytes != 350 || third.DownloadBytesPerSecond != 100 || third.UploadBytesPerSecond != 100 {
		t.Fatalf("fractional interval sample = %#v", third)
	}
}

func TestTrafficTrackerHandlesCounterResetAndReadGap(t *testing.T) {
	tracker := newTrafficTracker()
	epoch := time.Unix(2_000_000_000, 0).UTC()
	tracker.Begin("node_a", 7, "ppp0")
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 100, TXBytes: 200}, epoch)
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 200, TXBytes: 300}, epoch.Add(time.Second))
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 10, TXBytes: 20}, epoch.Add(2*time.Second))
	reset := tracker.Snapshot("node_a")
	if reset.DownloadBytes != 100 || reset.UploadBytes != 100 || reset.DownloadBytesPerSecond != 0 || reset.UploadBytesPerSecond != 0 {
		t.Fatalf("counter reset snapshot = %#v", reset)
	}
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 30, TXBytes: 70}, epoch.Add(3*time.Second))
	afterReset := tracker.Snapshot("node_a")
	if afterReset.DownloadBytes != 120 || afterReset.UploadBytes != 150 || afterReset.DownloadBytesPerSecond != 20 || afterReset.UploadBytesPerSecond != 50 {
		t.Fatalf("post-reset snapshot = %#v", afterReset)
	}

	tracker.Unavailable("node_a", 7, epoch.Add(4*time.Second))
	gap := tracker.Snapshot("node_a")
	if gap.DownloadBytes != 120 || gap.UploadBytes != 150 || gap.DownloadBytesPerSecond != 0 || gap.UploadBytesPerSecond != 0 {
		t.Fatalf("read gap snapshot = %#v", gap)
	}
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 130, TXBytes: 170}, epoch.Add(5*time.Second))
	rebaseline := tracker.Snapshot("node_a")
	if rebaseline.DownloadBytes != 120 || rebaseline.UploadBytes != 150 || rebaseline.DownloadBytesPerSecond != 0 || rebaseline.UploadBytesPerSecond != 0 {
		t.Fatalf("gap rebaseline snapshot = %#v", rebaseline)
	}
}

func TestTrafficTrackerIsGenerationSafeAndEndResets(t *testing.T) {
	tracker := newTrafficTracker()
	epoch := time.Unix(2_000_000_000, 0).UTC()
	tracker.Begin("node_a", 7, "ppp0")
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 100, TXBytes: 200}, epoch)
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 200, TXBytes: 300}, epoch.Add(time.Second))

	tracker.Begin("node_a", 8, "ppp1")
	if got := tracker.Snapshot("node_a"); got != (TrafficSnapshot{}) {
		t.Fatalf("new generation retained old traffic: %#v", got)
	}
	tracker.Sample("node_a", 7, platform.InterfaceCounters{RXBytes: 900, TXBytes: 900}, epoch.Add(2*time.Second))
	if got := tracker.Snapshot("node_a"); got != (TrafficSnapshot{}) {
		t.Fatalf("old generation changed traffic: %#v", got)
	}
	tracker.End("node_a", 7)
	tracker.Sample("node_a", 8, platform.InterfaceCounters{RXBytes: 10, TXBytes: 20}, epoch.Add(3*time.Second))
	if got := tracker.Snapshot("node_a"); got.SampledAt == "" {
		t.Fatalf("old generation End removed current session: %#v", got)
	}
	tracker.End("node_a", 8)
	if got := tracker.Snapshot("node_a"); got != (TrafficSnapshot{}) {
		t.Fatalf("End retained traffic: %#v", got)
	}
}

func TestTrafficTrackerKeepsSixtyIndependentNodes(t *testing.T) {
	tracker := newTrafficTracker()
	epoch := time.Unix(2_000_000_000, 0).UTC()
	for index := 1; index <= 60; index++ {
		nodeID := fmt.Sprintf("node_%02d", index)
		tracker.Begin(nodeID, uint64(index), fmt.Sprintf("ppp%d", index))
		tracker.Sample(nodeID, uint64(index), platform.InterfaceCounters{RXBytes: 100, TXBytes: 200}, epoch)
		tracker.Sample(nodeID, uint64(index), platform.InterfaceCounters{RXBytes: uint64(100 + index), TXBytes: uint64(200 + 2*index)}, epoch.Add(time.Second))
	}
	for index := 1; index <= 60; index++ {
		nodeID := fmt.Sprintf("node_%02d", index)
		got := tracker.Snapshot(nodeID)
		if got.DownloadBytes != uint64(index) || got.UploadBytes != uint64(2*index) {
			t.Fatalf("%s snapshot = %#v", nodeID, got)
		}
	}
}
