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
		if err := writeAll(stdout, []byte(buildinfo.Version+"\n")); err != nil {
			return 1
		}
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
	data, err := io.ReadAll(io.LimitReader(stdin, api.MaxFrameSize+3))
	if err != nil || len(data) > api.MaxFrameSize+2 {
		_, _ = fmt.Fprintln(stderr, "invalid control request input")
		return 2
	}
	if bytes.HasSuffix(data, []byte("\r\n")) {
		data = data[:len(data)-2]
	} else if bytes.HasSuffix(data, []byte("\n")) {
		data = data[:len(data)-1]
	}
	if len(data) > api.MaxFrameSize {
		_, _ = fmt.Fprintln(stderr, "invalid control request input")
		return 2
	}
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
	if err := writeAll(stdout, append(encoded, '\n')); err != nil {
		_, _ = fmt.Fprintln(stderr, "control response output failed")
		return 1
	}
	return 0
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
