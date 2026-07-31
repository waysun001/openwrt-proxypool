package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/buildinfo"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is kept separate from main so command-line protocol guarantees can be tested.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, buildinfo.Version)
		return 0
	}
	if len(args) == 0 || args[0] != "call" {
		_, _ = fmt.Fprintln(stderr, "usage: proxypoolctl call [--socket PATH]")
		return 2
	}
	socket := api.DefaultSocketPath
	for i := 1; i < len(args); i++ {
		if args[i] != "--socket" || i+1 >= len(args) || args[i+1] == "" {
			_, _ = fmt.Fprintln(stderr, "invalid call options")
			return 2
		}
		socket = args[i+1]
		i++
	}
	data, err := io.ReadAll(io.LimitReader(stdin, api.MaxFrameSize+1))
	if err != nil || len(data) > api.MaxFrameSize {
		_, _ = fmt.Fprintln(stderr, "invalid control request input")
		return 2
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || !json.Valid(data) {
		_, _ = fmt.Fprintln(stderr, "invalid control request input")
		return 2
	}
	request, err := api.ParseRequest(data)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "invalid control request input")
		return 2
	}
	response, err := (&api.Client{Path: socket, Timeout: 10 * time.Second}).Call(context.Background(), request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "control call failed")
		return 1
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "control response encoding failed")
		return 1
	}
	_, _ = stdout.Write(append(encoded, '\n'))
	return 0
}
