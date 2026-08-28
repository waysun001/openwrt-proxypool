package openwrt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"proxypoold/internal/model"
	"proxypoold/internal/platform"
)

const (
	authorizationManifestSchema = 1
	authorizationTTL            = "20s"
	nftPath                     = "/usr/sbin/nft"
)

var safeOwnershipID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var safeInterface = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
var authorizationLAN = netip.MustParsePrefix("192.168.9.0/24")

type authorizationManifest struct {
	SchemaVersion int                         `json:"schema_version"`
	Nodes         []authorizationManifestNode `json:"nodes"`
}

type authorizationManifestNode struct {
	NodeID     string                        `json:"node_id"`
	Generation uint64                        `json:"generation"`
	Leases     []platform.AuthorizationLease `json:"leases"`
}

type Authorizer struct {
	mu           sync.Mutex
	runner       platform.InputCommandRunner
	manifestPath string
}

func NewAuthorizer(runner platform.InputCommandRunner, manifestPath string) *Authorizer {
	return &Authorizer{runner: runner, manifestPath: manifestPath}
}

func (authorizer *Authorizer) Publish(ctx context.Context, lease platform.AuthorizationLease) error {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	lease, err := normalizeAuthorizationLease(lease)
	if err != nil || !authorizer.valid() {
		return errors.New("authorization lease is invalid")
	}
	manifest, err := authorizer.loadManifest()
	if err != nil {
		return err
	}
	nodeIndex := manifestNodeIndex(manifest, lease.NodeID)
	if nodeIndex >= 0 && lease.Generation < manifest.Nodes[nodeIndex].Generation {
		return errors.New("authorization generation is stale")
	}
	transaction := authorizer.renderAuthorizationPublish(ctx, lease)
	if _, err := authorizer.runner.RunInput(ctx, transaction, nftPath, "-c", "-f", "-"); err != nil {
		return errors.New("authorization transaction validation failed")
	}
	if _, err := authorizer.runner.RunInput(ctx, transaction, nftPath, "-f", "-"); err != nil {
		return errors.New("authorization transaction failed")
	}
	if err := authorizer.verifyLease(ctx, lease); err != nil {
		_ = authorizer.revokeLease(ctx, lease)
		return errors.New("authorization read-back failed")
	}
	if nodeIndex < 0 {
		manifest.Nodes = append(manifest.Nodes, authorizationManifestNode{NodeID: lease.NodeID, Generation: lease.Generation, Leases: []platform.AuthorizationLease{lease}})
	} else if lease.Generation > manifest.Nodes[nodeIndex].Generation {
		manifest.Nodes[nodeIndex] = authorizationManifestNode{NodeID: lease.NodeID, Generation: lease.Generation, Leases: []platform.AuthorizationLease{lease}}
	} else {
		upsertManifestLease(&manifest.Nodes[nodeIndex], lease)
	}
	if err := authorizer.saveManifest(manifest); err != nil {
		_ = authorizer.revokeLease(ctx, lease)
		return err
	}
	return nil
}

func (authorizer *Authorizer) RevokeNode(ctx context.Context, nodeID string, generation uint64) error {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	if !authorizer.valid() || !safeOwnershipID.MatchString(nodeID) || generation == 0 {
		return errors.New("authorization revocation is invalid")
	}
	manifest, err := authorizer.loadManifest()
	if err != nil {
		return err
	}
	index := manifestNodeIndex(manifest, nodeID)
	if index < 0 || generation < manifest.Nodes[index].Generation {
		return nil
	}
	if err := authorizer.revokeLeases(ctx, manifest.Nodes[index].Leases); err != nil {
		return err
	}
	manifest.Nodes = append(manifest.Nodes[:index], manifest.Nodes[index+1:]...)
	return authorizer.saveManifest(manifest)
}

func (authorizer *Authorizer) RevokeAll(ctx context.Context) error {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	if !authorizer.valid() {
		return errors.New("authorization revocation is invalid")
	}
	transaction := []byte(strings.Join([]string{
		"flush map inet proxypool_guard v2_policy_marks",
		"flush set inet proxypool_guard v2_dns_clients",
		"flush set inet proxypool_guard v2_l2tp_paths",
		"flush set inet proxypool_guard v2_l2tp_return_paths",
		"flush set inet proxypool_guard v2_tcp_redirects",
		"flush map inet proxypool_guard v2_tcp_redirect_ports",
		"flush set inet proxypool_guard v2_proxy_uploads",
		"flush set inet proxypool_guard v2_proxy_downloads",
		"",
	}, "\n"))
	if _, err := authorizer.runner.RunInput(ctx, transaction, nftPath, "-f", "-"); err != nil {
		return errors.New("authorization revoke-all failed")
	}
	return authorizer.saveManifest(authorizationManifest{SchemaVersion: authorizationManifestSchema})
}

func (authorizer *Authorizer) valid() bool {
	return authorizer != nil && authorizer.runner != nil && filepath.IsAbs(authorizer.manifestPath) && filepath.Base(authorizer.manifestPath) != "."
}

func normalizeAuthorizationLease(lease platform.AuthorizationLease) (platform.AuthorizationLease, error) {
	if !safeOwnershipID.MatchString(lease.NodeID) || lease.Generation == 0 || lease.PolicyID == 0 || !lease.IPv4.Is4() ||
		!lease.IPv4.IsValid() || !authorizationLAN.Contains(lease.IPv4) || lease.IPv4 == netip.MustParseAddr("192.168.9.1") ||
		lease.IPv4 == netip.MustParseAddr("192.168.9.0") || lease.IPv4 == netip.MustParseAddr("192.168.9.255") || !safeInterface.MatchString(lease.Interface) {
		return platform.AuthorizationLease{}, errors.New("invalid authorization lease")
	}
	mac, err := net.ParseMAC(lease.MAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || bytes.Equal(mac, []byte{0, 0, 0, 0, 0, 0}) {
		return platform.AuthorizationLease{}, errors.New("invalid authorization lease")
	}
	lease.MAC = strings.ToLower(mac.String())
	if lease.Protocol != model.ProtocolL2TP && lease.Protocol != model.ProtocolSOCKS5 && lease.Protocol != model.ProtocolSLP {
		return platform.AuthorizationLease{}, errors.New("invalid authorization lease")
	}
	if lease.Protocol == model.ProtocolL2TP && (lease.RedirectPort != 0 || !strings.HasPrefix(lease.Interface, "l2tp-ppv2")) {
		return platform.AuthorizationLease{}, errors.New("invalid authorization lease")
	}
	if lease.Protocol != model.ProtocolL2TP && lease.RedirectPort == 0 {
		return platform.AuthorizationLease{}, errors.New("invalid authorization lease")
	}
	return lease, nil
}

type nftLeaseElement struct {
	name  string
	key   string
	value string
}

func authorizationElements(lease platform.AuthorizationLease) []nftLeaseElement {
	mark := uint32(0x005a0000) | uint32(lease.PolicyID)
	elements := []nftLeaseElement{
		{name: "v2_policy_marks", key: fmt.Sprintf("%s . %s", lease.MAC, lease.IPv4), value: fmt.Sprintf("0x%08x", mark)},
		{name: "v2_dns_clients", key: fmt.Sprintf("%s . %s", lease.MAC, lease.IPv4)},
	}
	if lease.Protocol == model.ProtocolL2TP {
		elements = append(elements,
			nftLeaseElement{name: "v2_l2tp_paths", key: fmt.Sprintf("%s . %s . %q", lease.MAC, lease.IPv4, lease.Interface)},
			nftLeaseElement{name: "v2_l2tp_return_paths", key: fmt.Sprintf("%s . %q", lease.IPv4, lease.Interface)},
		)
	} else {
		elements = append(elements,
			nftLeaseElement{name: "v2_tcp_redirect_ports", key: fmt.Sprintf("%s . %s", lease.MAC, lease.IPv4), value: strconv.Itoa(int(lease.RedirectPort))},
			nftLeaseElement{name: "v2_tcp_redirects", key: fmt.Sprintf("%s . %s . %d", lease.MAC, lease.IPv4, lease.RedirectPort)},
			nftLeaseElement{name: "v2_proxy_uploads", key: fmt.Sprintf("%s . %s", lease.MAC, lease.IPv4)},
			nftLeaseElement{name: "v2_proxy_downloads", key: fmt.Sprintf("%s . %d", lease.IPv4, lease.RedirectPort)},
		)
	}
	return elements
}

func (authorizer *Authorizer) renderAuthorizationPublish(ctx context.Context, lease platform.AuthorizationLease) []byte {
	lines := make([]string, 0, 8)
	for _, element := range authorizationElements(lease) {
		if authorizer.elementExists(ctx, element) {
			lines = append(lines, fmt.Sprintf("delete element inet proxypool_guard %s { %s }", element.name, element.key))
		}
		elementText := element.key + " timeout " + authorizationTTL
		if element.value != "" {
			elementText += " : " + element.value
		}
		lines = append(lines, fmt.Sprintf("add element inet proxypool_guard %s { %s }", element.name, elementText))
	}
	return []byte(strings.Join(append(lines, ""), "\n"))
}

func (authorizer *Authorizer) elementExists(ctx context.Context, element nftLeaseElement) bool {
	_, err := authorizer.runner.Run(ctx, nftPath, "-nn", "get", "element", "inet", "proxypool_guard", element.name, "{", element.key, "}")
	return err == nil
}

func (authorizer *Authorizer) revokeLease(ctx context.Context, lease platform.AuthorizationLease) error {
	return authorizer.revokeLeases(ctx, []platform.AuthorizationLease{lease})
}

func (authorizer *Authorizer) revokeLeases(ctx context.Context, leases []platform.AuthorizationLease) error {
	if len(leases) == 0 {
		return nil
	}
	lines := make([]string, 0, len(leases)*4)
	for _, lease := range leases {
		for _, element := range authorizationElements(lease) {
			if authorizer.elementExists(ctx, element) {
				lines = append(lines, fmt.Sprintf("delete element inet proxypool_guard %s { %s }", element.name, element.key))
			}
		}
	}
	if len(lines) == 0 {
		return nil
	}
	transaction := []byte(strings.Join(append(lines, ""), "\n"))
	if _, err := authorizer.runner.RunInput(ctx, transaction, nftPath, "-f", "-"); err != nil {
		return errors.New("authorization revocation failed")
	}
	return nil
}

func (authorizer *Authorizer) verifyLease(ctx context.Context, lease platform.AuthorizationLease) error {
	for _, element := range authorizationElements(lease) {
		output, err := authorizer.runner.Run(ctx, nftPath, "-nn", "get", "element", "inet", "proxypool_guard", element.name, "{", element.key, "}")
		if err != nil {
			return err
		}
		text := strings.ToLower(string(output))
		for _, token := range []string{element.name, element.key, element.value} {
			if token != "" && !strings.Contains(text, strings.ToLower(token)) {
				return errors.New("authorization element does not match")
			}
		}
	}
	return nil
}

func manifestNodeIndex(manifest authorizationManifest, nodeID string) int {
	for index := range manifest.Nodes {
		if manifest.Nodes[index].NodeID == nodeID {
			return index
		}
	}
	return -1
}

func upsertManifestLease(node *authorizationManifestNode, lease platform.AuthorizationLease) {
	for index := range node.Leases {
		if node.Leases[index].MAC == lease.MAC && node.Leases[index].IPv4 == lease.IPv4 {
			node.Leases[index] = lease
			return
		}
	}
	node.Leases = append(node.Leases, lease)
}

func (authorizer *Authorizer) loadManifest() (authorizationManifest, error) {
	info, err := os.Lstat(authorizer.manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return authorizationManifest{SchemaVersion: authorizationManifestSchema}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return authorizationManifest{}, errors.New("authorization manifest is unsafe")
	}
	contents, err := os.ReadFile(authorizer.manifestPath)
	if err != nil || len(contents) > 1<<20 {
		return authorizationManifest{}, errors.New("authorization manifest read failed")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var manifest authorizationManifest
	if err := decoder.Decode(&manifest); err != nil || manifest.SchemaVersion != authorizationManifestSchema {
		return authorizationManifest{}, errors.New("authorization manifest is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return authorizationManifest{}, errors.New("authorization manifest is invalid")
	}
	seenNodes := make(map[string]struct{}, len(manifest.Nodes))
	for _, node := range manifest.Nodes {
		if !safeOwnershipID.MatchString(node.NodeID) || node.Generation == 0 {
			return authorizationManifest{}, errors.New("authorization manifest is invalid")
		}
		if _, exists := seenNodes[node.NodeID]; exists {
			return authorizationManifest{}, errors.New("authorization manifest is invalid")
		}
		seenNodes[node.NodeID] = struct{}{}
		seenLeases := make(map[string]struct{}, len(node.Leases))
		for _, lease := range node.Leases {
			normalized, err := normalizeAuthorizationLease(lease)
			if err != nil || normalized.NodeID != node.NodeID || normalized.Generation != node.Generation {
				return authorizationManifest{}, errors.New("authorization manifest is invalid")
			}
			key := normalized.MAC + "|" + normalized.IPv4.String()
			if _, exists := seenLeases[key]; exists {
				return authorizationManifest{}, errors.New("authorization manifest is invalid")
			}
			seenLeases[key] = struct{}{}
		}
	}
	return manifest, nil
}

func (authorizer *Authorizer) saveManifest(manifest authorizationManifest) error {
	directory := filepath.Dir(authorizer.manifestPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("authorization manifest directory failed")
	}
	sort.Slice(manifest.Nodes, func(i, j int) bool { return manifest.Nodes[i].NodeID < manifest.Nodes[j].NodeID })
	contents, err := json.Marshal(manifest)
	if err != nil {
		return errors.New("authorization manifest encode failed")
	}
	temporary, err := os.CreateTemp(directory, ".proxypool-auth-*")
	if err != nil {
		return errors.New("authorization manifest create failed")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return errors.New("authorization manifest permission failed")
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return errors.New("authorization manifest write failed")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("authorization manifest write failed")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("authorization manifest write failed")
	}
	if info, err := os.Lstat(authorizer.manifestPath); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("authorization manifest is unsafe")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("authorization manifest is unsafe")
	}
	if err := os.Rename(temporaryPath, authorizer.manifestPath); err != nil {
		return errors.New("authorization manifest publish failed")
	}
	if err := os.Chmod(authorizer.manifestPath, 0o600); err != nil {
		return errors.New("authorization manifest permission failed")
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(directory)
		if err != nil {
			return errors.New("authorization manifest directory sync failed")
		}
		err = dir.Sync()
		_ = dir.Close()
		if err != nil {
			return errors.New("authorization manifest directory sync failed")
		}
	}
	return nil
}

var _ platform.Authorizer = (*Authorizer)(nil)
