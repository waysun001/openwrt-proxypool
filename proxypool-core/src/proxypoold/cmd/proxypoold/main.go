package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"proxypoold/internal/api"
	"proxypoold/internal/buildinfo"
	"proxypoold/internal/config"
	"proxypoold/internal/engine"
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

	var handler api.Handler
	methods := map[string]struct{}{"status.get": {}}
	if options.mode == "shadow" {
		shadow := engine.NewShadow(options.configPath, nil)
		shadow.Start()
		handler = shadow
	} else {
		controller, err := engine.NewController(
			config.NewStore(options.configPath),
			engine.NewRuntimeStore(options.statePath),
			engine.NewMachine(nil),
			engine.NewJobStore(),
		)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "proxypoold: live control initialization failed")
			return 1
		}
		handler = controller
		methods = map[string]struct{}{
			"status.get": {}, "device.list": {}, "device.bind": {}, "device.unbind": {},
			"node.action": {}, "job.get": {}, "job.list": {}, "system.events": {},
		}
	}
	server := &api.Server{
		Path:    options.socketPath,
		Handler: handler,
		Methods: methods,
	}
	if err := server.Serve(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "proxypoold: control service failed")
		return 1
	}
	return 0
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
