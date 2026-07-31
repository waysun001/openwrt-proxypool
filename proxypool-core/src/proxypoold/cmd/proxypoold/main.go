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

	shadow := engine.NewShadow(options.configPath, nil)
	shadow.Start()
	server := &api.Server{
		Path:    options.socketPath,
		Handler: shadow,
		Methods: map[string]struct{}{"status.get": {}},
	}
	if err := server.Serve(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "proxypoold: shadow control service failed")
		return 1
	}
	return 0
}

type daemonOptions struct {
	configPath string
	socketPath string
}

func parseOptions(args []string) (daemonOptions, bool) {
	options := daemonOptions{configPath: "/etc/config/proxypool", socketPath: api.DefaultSocketPath}
	seenShadow, seenConfig, seenSocket := false, false, false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--shadow":
			if seenShadow {
				return daemonOptions{}, false
			}
			seenShadow = true
		case "--config":
			if seenConfig || index+1 >= len(args) || args[index+1] == "" {
				return daemonOptions{}, false
			}
			seenConfig = true
			index++
			options.configPath = args[index]
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
	return options, seenShadow
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: proxypoold --shadow [--config PATH] [--socket PATH]")
}
