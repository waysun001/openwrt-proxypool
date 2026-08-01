package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"proxypoold/internal/api"
	"proxypoold/internal/buildinfo"
	"proxypoold/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type ubusExecutor func(context.Context, string, ...string) ([]byte, error)

// run is kept separate from main so command-line protocol guarantees can be tested.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithUbus(args, stdin, stdout, stderr, executeUbus)
}

func runWithUbus(args []string, stdin io.Reader, stdout, stderr io.Writer, execute ubusExecutor) int {
	if len(args) == 1 && args[0] == "--version" {
		if err := writeAll(stdout, []byte(buildinfo.Version+"\n")); err != nil {
			return 1
		}
		return 0
	}
	if len(args) > 0 && args[0] == "classify" {
		return runClassify(args, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "select-backend" {
		return runSelectBackend(args, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "config-enabled" {
		return runConfigEnabled(args, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "procd-state" {
		return runProcdState(args, stdout, stderr, execute)
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

func executeUbus(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func runProcdState(args []string, stdout, stderr io.Writer, execute ubusExecutor) int {
	if (len(args) != 3 && len(args) != 5) || args[1] != "--service" || args[2] != "proxypool" || (len(args) == 5 && (args[3] != "--instance" || args[4] == "")) {
		_, _ = fmt.Fprintln(stderr, "usage: proxypoolctl procd-state --service proxypool [--instance TOKEN]")
		return 2
	}
	instance := ""
	if len(args) == 5 {
		instance = args[4]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := execute(ctx, "/bin/ubus", "call", "service", "list", `{"name":"proxypool"}`)
	state := "unknown"
	if err == nil {
		state = inspectProcdState(response, "proxypool", instance)
	}
	if err := writeAll(stdout, []byte(state+"\n")); err != nil {
		_, _ = fmt.Fprintln(stderr, "procd state output failed")
		return 1
	}
	if state == "unknown" {
		_, _ = fmt.Fprintln(stderr, "procd state query failed")
		return 1
	}
	return 0
}

func inspectProcdState(data []byte, service, instance string) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return "unknown"
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "unknown"
	}

	root, ok := document.(map[string]any)
	if !ok {
		return "unknown"
	}
	serviceValue, exists := root[service]
	if !exists {
		return "absent"
	}
	serviceObject, ok := serviceValue.(map[string]any)
	if !ok {
		return "unknown"
	}
	instancesValue, exists := serviceObject["instances"]
	if !exists {
		return "unknown"
	}
	instances, ok := instancesValue.(map[string]any)
	if !ok {
		return "unknown"
	}
	for _, value := range instances {
		if _, ok := value.(map[string]any); !ok {
			return "unknown"
		}
	}
	if instance == "" {
		if len(instances) == 0 {
			return "absent"
		}
		return "present"
	}
	instanceValue, exists := instances[instance]
	if !exists {
		return "absent"
	}
	instanceObject := instanceValue.(map[string]any)
	runningValue, exists := instanceObject["running"]
	if !exists {
		return "unknown"
	}
	running, ok := runningValue.(bool)
	if !ok {
		return "unknown"
	}
	if running {
		return "running"
	}
	return "present"
}

func runClassify(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[1] != "--config" || args[2] == "" {
		_, _ = fmt.Fprintln(stderr, "usage: proxypoolctl classify --config PATH")
		return 2
	}
	class := config.InspectFile(args[2]).StartupClass()
	if err := writeAll(stdout, []byte(string(class)+"\n")); err != nil {
		return 1
	}
	if class == config.StartupUnknown {
		_, _ = fmt.Fprintln(stderr, "configuration classification failed")
		return 1
	}
	return 0
}

func runSelectBackend(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[1] != "--config" || args[2] == "" {
		_, _ = fmt.Fprintln(stderr, "usage: proxypoolctl select-backend --config PATH")
		return 2
	}
	selection := config.InspectRuntimeSelectorFile(args[2])
	if err := writeAll(stdout, []byte(string(selection)+"\n")); err != nil {
		return 1
	}
	if selection == config.RuntimeSelectionUnknown {
		_, _ = fmt.Fprintln(stderr, "runtime selector classification failed")
		return 1
	}
	return 0
}

func runConfigEnabled(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[1] != "--config" || args[2] == "" {
		_, _ = fmt.Fprintln(stderr, "usage: proxypoolctl config-enabled --config PATH")
		return 2
	}
	enabled, ok := config.InspectEnabledFile(args[2])
	if !ok {
		_, _ = fmt.Fprintln(stderr, "configuration enabled-state inspection failed")
		return 1
	}
	value := "0\n"
	if enabled {
		value = "1\n"
	}
	if err := writeAll(stdout, []byte(value)); err != nil {
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
