package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/buildinfo"
	"proxypoold/internal/config"
	"proxypoold/internal/dnsproxy"
	"proxypoold/internal/engine"
	"proxypoold/internal/live"
	"proxypoold/internal/model"
	"proxypoold/internal/platform"
	openwrtplatform "proxypoold/internal/platform/openwrt"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, buildinfo.Version)
		return 0
	}

	options, ok := parseOptions(args)
	if !ok {
		printUsage(stderr)
		return 2
	}
	select {
	case <-ctx.Done():
		return 0
	default:
	}
	endpointLease, err := api.AcquireEndpointLease(options.socketPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "proxypoold: control endpoint is already owned")
		return 1
	}
	defer endpointLease.Close()

	if options.mode == "shadow" {
		shadow := engine.NewShadow(options.configPath, nil)
		shadow.Start()
		server := &api.Server{Path: options.socketPath, Handler: shadow, Lease: endpointLease, Methods: map[string]struct{}{"status.get": {}}}
		if err := server.Serve(ctx); err != nil {
			_, _ = fmt.Fprintln(stderr, "proxypoold: control service failed")
			return 1
		}
		return 0
	}
	if err := runLive(ctx, options, endpointLease); err != nil {
		_, _ = fmt.Fprintln(stderr, "proxypoold: live service failed")
		return 1
	}
	return 0
}

func runLive(ctx context.Context, options daemonOptions, endpointLease *api.EndpointLease) error {
	if err := ensurePrivateStateParent(options.statePath); err != nil {
		return err
	}
	desiredStore := config.NewStore(options.configPath)
	desired, err := desiredStore.Load()
	if err != nil || !desired.Global.Enabled {
		return errors.New("live desired configuration is unavailable")
	}
	runner := openwrtplatform.NewRunner(10 * time.Second)
	inventory := openwrtplatform.NewDeviceInventory("/tmp/dhcp.leases", runner)
	leaseManager := openwrtplatform.NewLeaseManager(
		runner, inventory, netip.MustParsePrefix("192.168.9.0/24"), netip.MustParseAddr("192.168.9.1"),
	)
	controller, err := engine.NewController(
		desiredStore, engine.NewRuntimeStore(options.statePath), engine.NewMachine(nil), engine.NewJobStore(),
		engine.WithDeviceServices(inventory, leaseManager),
	)
	if err != nil {
		return errors.New("live control initialization failed")
	}
	resolver, err := bootstrapResolver(desired.Global.DoHEndpoints)
	if err != nil {
		return err
	}
	dnsServer := dnsproxy.NewServer(netip.MustParseAddrPort("192.168.9.1:53"))
	l2tp := openwrtplatform.NewL2TPAdapter(runner, resolver, "")
	routeGate := live.NewRouteGate(openwrtplatform.NewRouteManager(runner))
	dnsFactory := func(_ model.Node, session platform.Session, endpoint model.DoHEndpoint) (dnsproxy.NodeChannel, error) {
		transport, err := openwrtplatform.NewDoHTransport(endpoint, session.Interface)
		if err != nil {
			return nil, err
		}
		return dnsproxy.NewDoHChannel(endpoint, transport)
	}
	dnsGate := live.NewDNSGate(desiredStore, dnsServer, dnsFactory)
	authorizer := openwrtplatform.NewAuthorizer(runner, "/var/run/proxypool/authorization.json")
	authorizationGate := live.NewAuthorizationGate(desiredStore, authorizer)
	scheduler := engine.NewScheduler(controller, l2tp, engine.SchedulerConfig{
		L2TPConcurrency: desired.Global.L2TPConcurrency, ProxyConcurrency: desired.Global.ProxyConcurrency,
		ConnectTimeout: desired.Global.ConnectTimeout, StopTimeout: desired.Global.StopTimeout,
	}, routeGate, dnsGate, authorizationGate)
	controller.AttachScheduler(scheduler)
	server := &api.Server{
		Path: options.socketPath, Handler: controller, Lease: endpointLease,
		Methods: map[string]struct{}{
			"status.get": {}, "device.list": {}, "device.bind": {}, "device.unbind": {},
			"node.action": {}, "import.preview": {}, "import.commit": {}, "job.get": {}, "job.list": {}, "system.events": {}, "system.interface_event": {},
		},
	}
	return serveLive(ctx, controller, scheduler, dnsServer, authorizationGate, authorizer, server, desired.Global.StopTimeout)
}

func bootstrapResolver(endpoints []model.DoHEndpoint) (*dnsproxy.HostResolver, error) {
	channels := make([]dnsproxy.NodeChannel, 0, len(endpoints))
	for _, endpoint := range endpoints {
		transport, err := openwrtplatform.NewBootstrapDoHTransport(endpoint)
		if err != nil {
			return nil, errors.New("bootstrap DNS transport is invalid")
		}
		channel, err := dnsproxy.NewDoHChannel(endpoint, transport)
		if err != nil {
			return nil, errors.New("bootstrap DNS channel is invalid")
		}
		channels = append(channels, channel)
	}
	if len(channels) == 0 {
		return nil, errors.New("bootstrap DNS endpoint is missing")
	}
	return dnsproxy.NewHostResolver(channels...), nil
}

func serveLive(ctx context.Context, controller *engine.Controller, scheduler *engine.Scheduler, dnsServer *dnsproxy.Server, authorizationGate *live.AuthorizationGate, authorizer platform.Authorizer, server *api.Server, stopTimeout time.Duration) error {
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	dnsDone := make(chan error, 1)
	go func() { dnsDone <- dnsServer.Run(serviceCtx) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-dnsDone:
		return err
	case <-dnsServer.Ready():
	}
	if err := controller.ReconcileStartup(serviceCtx); err != nil {
		return errors.New("startup reconciliation failed")
	}
	schedulerDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { schedulerDone <- scheduler.Run(serviceCtx) }()
	go func() { serverDone <- server.Serve(serviceCtx) }()
	go runPendingBindingLearner(serviceCtx, controller, 5*time.Second)
	var serviceErr error
	schedulerStopped := false
	select {
	case <-ctx.Done():
	case serviceErr = <-dnsDone:
	case serviceErr = <-schedulerDone:
		schedulerStopped = true
	case serviceErr = <-serverDone:
	}
	cancel()
	cleanupTimeout := stopTimeout
	if cleanupTimeout <= 0 {
		cleanupTimeout = 30 * time.Second
	}
	authorizationGate.StopRenewals()
	revokeCtx, revokeCancel := context.WithTimeout(context.Background(), cleanupTimeout)
	revokeErr := authorizer.RevokeAll(revokeCtx)
	revokeCancel()
	var drainErr error
	if !schedulerStopped {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), cleanupTimeout)
		select {
		case <-schedulerDone:
		case <-drainCtx.Done():
			drainErr = errors.New("live scheduler shutdown timed out")
		}
		drainCancel()
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cleanupTimeout)
	shutdownErr := scheduler.Shutdown(shutdownCtx)
	shutdownCancel()
	if drainErr != nil && shutdownErr == nil {
		shutdownErr = drainErr
	}
	return liveServiceResult(ctx.Err(), serviceErr, shutdownErr, revokeErr)
}

func runPendingBindingLearner(ctx context.Context, controller *engine.Controller, interval time.Duration) {
	if controller == nil || interval <= 0 {
		return
	}
	_ = controller.LearnPendingBindings(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = controller.LearnPendingBindings(ctx)
		}
	}
}

func liveServiceResult(parentErr, serviceErr, shutdownErr, revokeErr error) error {
	if shutdownErr != nil || revokeErr != nil {
		return errors.New("live cleanup failed")
	}
	if parentErr != nil {
		return nil
	}
	return serviceErr
}

func ensurePrivateStateParent(path string) error {
	if !filepath.IsAbs(path) || filepath.Base(path) == "." {
		return errors.New("runtime state path is invalid")
	}
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return errors.New("runtime state directory creation failed")
		}
		info, err = os.Lstat(directory)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime state directory is unsafe")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return errors.New("runtime state directory permission failed")
	}
	return nil
}

type daemonOptions struct {
	configPath string
	statePath  string
	socketPath string
	mode       string
}

func parseOptions(args []string) (daemonOptions, bool) {
	options := daemonOptions{configPath: "/etc/config/proxypool", socketPath: api.DefaultSocketPath}
	seenShadow, seenLive, seenConfig, seenState, seenSocket := false, false, false, false, false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--shadow":
			if seenShadow || seenLive {
				return daemonOptions{}, false
			}
			seenShadow = true
			options.mode = "shadow"
		case "--live":
			if seenLive || seenShadow {
				return daemonOptions{}, false
			}
			seenLive = true
			options.mode = "live"
		case "--config":
			if seenConfig || index+1 >= len(args) || args[index+1] == "" {
				return daemonOptions{}, false
			}
			seenConfig = true
			index++
			options.configPath = args[index]
		case "--state":
			if seenState || index+1 >= len(args) || args[index+1] == "" {
				return daemonOptions{}, false
			}
			seenState = true
			index++
			options.statePath = args[index]
		case "--socket":
			if seenSocket || index+1 >= len(args) || args[index+1] == "" {
				return daemonOptions{}, false
			}
			seenSocket = true
			index++
			options.socketPath = args[index]
		default:
			return daemonOptions{}, false
		}
	}
	if seenShadow {
		return options, !seenState
	}
	return options, seenLive && seenState
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: proxypoold (--shadow | --live --state PATH) [--config PATH] [--socket PATH]")
}
