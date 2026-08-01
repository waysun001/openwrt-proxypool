package importer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"proxypoold/internal/model"
)

func TestPreviewSkipsExistingNaturalKeyAndCommitConsumesOnce(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ids := []string{"preview-one"}
	manager := New(WithClock(func() time.Time { return now }), WithIDSource(func() string { id := ids[0]; ids = ids[1:]; return id }))
	desired := importTestConfig()
	preview, err := manager.Preview(context.Background(), PreviewRequest{
		Protocol: model.ProtocolL2TP, Raw: "vpn-existing.example|existing|old-password\nvpn-new.example|new-user|new-password", Base: desired,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.ID != "preview-one" || preview.Hash == "" || preview.BaseRevision != 9 || preview.Blocked || preview.Added != 1 || preview.Skipped != 1 {
		t.Fatalf("preview = %#v", preview)
	}
	if strings.Contains(preview.String(), "old-password") || strings.Contains(preview.String(), "new-password") {
		t.Fatalf("preview string leaked credentials: %s", preview.String())
	}
	nodes, err := manager.Commit(context.Background(), CommitRequest{PreviewID: preview.ID, PreviewHash: preview.Hash, ExpectedRevision: 9})
	if err != nil || len(nodes) != 1 || nodes[0].Password != "new-password" {
		t.Fatalf("Commit() nodes=%#v err=%v", nodes, err)
	}
	if _, err := manager.Commit(context.Background(), CommitRequest{PreviewID: preview.ID, PreviewHash: preview.Hash, ExpectedRevision: 9}); !isCode(err, ErrorPreviewNotFound) {
		t.Fatalf("replayed Commit() error = %v", err)
	}
}

func TestPreviewBlockingErrorsCannotCommit(t *testing.T) {
	manager := New(WithIDSource(func() string { return "blocked-preview" }))
	preview, err := manager.Preview(context.Background(), PreviewRequest{Protocol: model.ProtocolL2TP, Raw: "invalid", Base: importTestConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Blocked || len(preview.Errors) == 0 {
		t.Fatalf("preview = %#v", preview)
	}
	if _, err := manager.Commit(context.Background(), CommitRequest{PreviewID: preview.ID, PreviewHash: preview.Hash, ExpectedRevision: 9}); !isCode(err, ErrorPreviewBlocked) {
		t.Fatalf("blocked Commit() error = %v", err)
	}
}

func TestPreviewBindsHashRevisionExpiryAndCapacity(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	nextID := 0
	manager := New(WithClock(func() time.Time { return now }), WithIDSource(func() string { nextID++; return "preview-" + twoDigits(nextID) }))
	desired := importTestConfig()
	first, err := manager.Preview(context.Background(), PreviewRequest{Protocol: model.ProtocolL2TP, Raw: "vpn-new.example|user|password", Base: desired})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Commit(context.Background(), CommitRequest{PreviewID: first.ID, PreviewHash: "wrong", ExpectedRevision: 9}); !isCode(err, ErrorPreviewMismatch) {
		t.Fatalf("wrong hash error = %v", err)
	}
	if _, err := manager.Commit(context.Background(), CommitRequest{PreviewID: first.ID, PreviewHash: first.Hash, ExpectedRevision: 10}); !isCode(err, ErrorRevisionConflict) {
		t.Fatalf("wrong revision error = %v", err)
	}
	now = now.Add(PreviewLifetime + time.Second)
	if _, err := manager.Commit(context.Background(), CommitRequest{PreviewID: first.ID, PreviewHash: first.Hash, ExpectedRevision: 9}); !isCode(err, ErrorPreviewExpired) {
		t.Fatalf("expired preview error = %v", err)
	}

	now = now.Add(time.Second)
	for index := 0; index < MaxConcurrentPreviews; index++ {
		if _, err := manager.Preview(context.Background(), PreviewRequest{Protocol: model.ProtocolL2TP, Raw: "vpn-" + twoDigits(index) + ".example|user|password", Base: desired}); err != nil {
			t.Fatalf("Preview(%d) error = %v", index, err)
		}
	}
	if _, err := manager.Preview(context.Background(), PreviewRequest{Protocol: model.ProtocolL2TP, Raw: "overflow.example|user|password", Base: desired}); !isCode(err, ErrorPreviewCapacity) {
		t.Fatalf("preview capacity error = %v", err)
	}
}

func TestPreviewRejectsTotalNodeCapacityAndMergeAllocatesStableIdentity(t *testing.T) {
	desired := importTestConfig()
	for index := 2; index <= 60; index++ {
		id := "existing-" + twoDigits(index)
		desired.Nodes[id] = model.Node{ID: id, Name: "Existing " + twoDigits(index), Protocol: model.ProtocolL2TP, Enabled: true, Server: "existing-" + twoDigits(index) + ".example", Port: 1701, Username: "user", Password: "password", PolicyID: uint16(index), Revision: 9}
	}
	manager := New(WithIDSource(func() string { return "capacity-preview" }))
	preview, err := manager.Preview(context.Background(), PreviewRequest{Protocol: model.ProtocolL2TP, Raw: "new.example|user|password", Base: desired})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Blocked || len(preview.Errors) != 1 || preview.Errors[0].Code != ErrorCapacityExceeded {
		t.Fatalf("capacity preview = %#v", preview)
	}

	base := importTestConfig()
	parsed := Parse(model.ProtocolL2TP, "vpn-new.example|new-user|new-password")
	merged, nodeIDs, err := Merge(base, parsed.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeIDs) != 1 || len(merged.Nodes) != 2 {
		t.Fatalf("Merge() nodeIDs=%#v nodes=%#v", nodeIDs, merged.Nodes)
	}
	node := merged.Nodes[nodeIDs[0]]
	if node.ID == "" || node.Name == "" || node.PolicyID != 2 || node.Revision != 0 || node.Password != "new-password" || !node.Enabled {
		t.Fatalf("merged node = %#v", node)
	}
	if err := model.Validate(merged); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}
}

func importTestConfig() model.DesiredConfig {
	return model.DesiredConfig{
		SchemaVersion: 2,
		Revision:      9,
		Global:        model.GlobalConfig{Enabled: true, RuntimeBackend: "v2_live", MaxNodes: 60, LANDevice: "br-lan", ManagementPorts: []uint16{80, 443}, L2TPConcurrency: 4, ProxyConcurrency: 8, ConnectTimeout: 30 * time.Second, StopTimeout: 20 * time.Second, DoHEndpoints: []model.DoHEndpoint{{URL: "https://dns.example/dns-query", BootstrapIP: "192.0.2.53", ServerName: "dns.example"}}},
		Nodes: map[string]model.Node{
			"existing": {ID: "existing", Name: "Existing", Protocol: model.ProtocolL2TP, Enabled: true, Server: "vpn-existing.example", Port: 1701, Username: "existing", Password: "password", PolicyID: 1, Revision: 7},
		},
		Devices: map[string]model.Device{},
	}
}

func isCode(err error, code string) bool {
	var coded *model.CodeError
	return errors.As(err, &coded) && coded.Code == code
}
