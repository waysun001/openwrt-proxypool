package openwrt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"proxypoold/internal/platform"
)

const (
	ipPath              = "/sbin/ip"
	policyTableBase     = uint32(100000)
	policyPriorityBase  = uint32(200000)
	proxypoolRouteProto = uint32(186)
	policyMarkMask      = uint32(0x00ffffff)
)

type RouteManager struct {
	runner platform.CommandRunner
}

func NewRouteManager(runner platform.CommandRunner) *RouteManager {
	return &RouteManager{runner: runner}
}

func (manager *RouteManager) Install(ctx context.Context, lease platform.RouteLease) error {
	if err := validateRouteLease(manager, lease); err != nil {
		return err
	}
	state, err := manager.inspect(ctx, lease)
	if err != nil {
		return err
	}
	if state.foreign {
		return errors.New("policy route ownership conflicts")
	}
	if state.ruleExact && state.routeExact {
		return nil
	}
	table := policyTable(lease.PolicyID)
	mark := policyMark(lease.PolicyID)
	routeAdded := false
	ruleAdded := false
	if !state.routeExact {
		if _, err := manager.runner.Run(ctx, ipPath, "-4", "route", "add", "table", fmt.Sprint(table), "default", "dev", lease.Interface, "proto", fmt.Sprint(proxypoolRouteProto)); err != nil {
			return errors.New("policy route install failed")
		}
		routeAdded = true
	}
	if !state.ruleExact {
		if _, err := manager.runner.Run(ctx, ipPath, "-4", "rule", "add", "pref", fmt.Sprint(policyPriority(lease.PolicyID)), "fwmark", fmt.Sprintf("0x%08x/0x%08x", mark, policyMarkMask), "lookup", fmt.Sprint(table)); err != nil {
			if routeAdded {
				_, _ = manager.runner.Run(context.Background(), ipPath, "-4", "route", "del", "table", fmt.Sprint(table), "default", "dev", lease.Interface, "proto", fmt.Sprint(proxypoolRouteProto))
			}
			return errors.New("policy rule install failed")
		}
		ruleAdded = true
	}
	if err := manager.Verify(ctx, lease); err != nil {
		if ruleAdded {
			_, _ = manager.runner.Run(context.Background(), ipPath, "-4", "rule", "del", "pref", fmt.Sprint(policyPriority(lease.PolicyID)), "fwmark", fmt.Sprintf("0x%08x/0x%08x", mark, policyMarkMask), "lookup", fmt.Sprint(table))
		}
		if routeAdded {
			_, _ = manager.runner.Run(context.Background(), ipPath, "-4", "route", "del", "table", fmt.Sprint(table), "default", "dev", lease.Interface, "proto", fmt.Sprint(proxypoolRouteProto))
		}
		return err
	}
	return nil
}

func (manager *RouteManager) Verify(ctx context.Context, lease platform.RouteLease) error {
	if err := validateRouteLease(manager, lease); err != nil {
		return err
	}
	state, err := manager.inspect(ctx, lease)
	if err != nil {
		return err
	}
	if state.foreign || !state.ruleExact || !state.routeExact {
		return errors.New("policy route verification failed")
	}
	return nil
}

func (manager *RouteManager) Remove(ctx context.Context, lease platform.RouteLease) error {
	if err := validateRouteLease(manager, lease); err != nil {
		return err
	}
	state, err := manager.inspect(ctx, lease)
	if err != nil {
		return err
	}
	if state.foreign {
		return errors.New("policy route ownership conflicts")
	}
	table := policyTable(lease.PolicyID)
	mark := policyMark(lease.PolicyID)
	if state.ruleExact {
		if _, err := manager.runner.Run(ctx, ipPath, "-4", "rule", "del", "pref", fmt.Sprint(policyPriority(lease.PolicyID)), "fwmark", fmt.Sprintf("0x%08x/0x%08x", mark, policyMarkMask), "lookup", fmt.Sprint(table)); err != nil {
			return errors.New("policy rule removal failed")
		}
	}
	if state.routeExact {
		if _, err := manager.runner.Run(ctx, ipPath, "-4", "route", "del", "table", fmt.Sprint(table), "default", "dev", lease.Interface, "proto", fmt.Sprint(proxypoolRouteProto)); err != nil {
			return errors.New("policy route removal failed")
		}
	}
	return nil
}

type routeState struct {
	ruleExact  bool
	routeExact bool
	foreign    bool
}

func (manager *RouteManager) inspect(ctx context.Context, lease platform.RouteLease) (routeState, error) {
	rulesOutput, err := manager.runner.Run(ctx, ipPath, "-4", "-j", "rule", "show")
	if err != nil {
		return routeState{}, errors.New("policy rule inspection failed")
	}
	routesOutput, err := manager.runner.Run(ctx, ipPath, "-4", "-j", "route", "show", "table", "all")
	if err != nil {
		return routeState{}, errors.New("policy route inspection failed")
	}
	state, err := inspectRules(rulesOutput, lease)
	if err != nil {
		return routeState{}, err
	}
	routeExact, routeForeign, err := inspectRoutes(routesOutput, lease)
	if err != nil {
		return routeState{}, err
	}
	state.routeExact = routeExact
	state.foreign = state.foreign || routeForeign
	return state, nil
}

func inspectRules(contents []byte, lease platform.RouteLease) (routeState, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	var rules []map[string]any
	if err := decoder.Decode(&rules); err != nil || len(rules) > 4096 {
		return routeState{}, errors.New("policy rule output is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return routeState{}, errors.New("policy rule output is invalid")
	}
	wantTable := uint64(policyTable(lease.PolicyID))
	wantPriority := uint64(policyPriority(lease.PolicyID))
	wantMark := uint64(policyMark(lease.PolicyID))
	state := routeState{}
	for _, rule := range rules {
		table, tableSet := jsonUint(rule["table"])
		priority, prioritySet := jsonUint(rule["priority"])
		mark, markSet := jsonUint(rule["fwmark"])
		mask, maskSet := jsonUint(rule["fwmask"])
		collides := (tableSet && table == wantTable) || (prioritySet && priority == wantPriority) || (markSet && mark == wantMark)
		if !collides {
			continue
		}
		exact := tableSet && table == wantTable && prioritySet && priority == wantPriority && markSet && mark == wantMark && maskSet && mask == uint64(policyMarkMask)
		if exact && !state.ruleExact {
			state.ruleExact = true
		} else {
			state.foreign = true
		}
	}
	return state, nil
}

func inspectRoutes(contents []byte, lease platform.RouteLease) (exact, foreign bool, err error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	var routes []map[string]any
	if err := decoder.Decode(&routes); err != nil || len(routes) > 64 {
		return false, false, errors.New("policy route output is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false, false, errors.New("policy route output is invalid")
	}
	wantTable := uint64(policyTable(lease.PolicyID))
	for _, route := range routes {
		destination, _ := route["dst"].(string)
		device, _ := route["dev"].(string)
		table, tableSet := jsonUint(route["table"])
		protocol, protocolSet := jsonUint(route["protocol"])
		if !tableSet || table != wantTable {
			continue
		}
		isExact := destination == "default" && device == lease.Interface && protocolSet && protocol == uint64(proxypoolRouteProto)
		if isExact && !exact {
			exact = true
		} else {
			foreign = true
		}
	}
	return exact, foreign, nil
}

func jsonUint(value any) (uint64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(string(typed), 0, 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 0, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func validateRouteLease(manager *RouteManager, lease platform.RouteLease) error {
	if manager == nil || manager.runner == nil || !safeOwnershipID.MatchString(lease.NodeID) || lease.PolicyID == 0 || lease.Generation == 0 || !safeInterface.MatchString(lease.Interface) || !strings.HasPrefix(lease.Interface, "l2tp-ppv2") {
		return errors.New("policy route lease is invalid")
	}
	return nil
}

func policyMark(policyID uint16) uint32     { return uint32(0x005a0000) | uint32(policyID) }
func policyTable(policyID uint16) uint32    { return policyTableBase + uint32(policyID) }
func policyPriority(policyID uint16) uint32 { return policyPriorityBase + uint32(policyID) }

var _ platform.RouteManager = (*RouteManager)(nil)
