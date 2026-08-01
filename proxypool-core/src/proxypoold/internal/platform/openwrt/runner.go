package openwrt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultCommandTimeout = 10 * time.Second
	defaultMaxOutput      = 1 << 20
)

type commandExecutor func(context.Context, string, ...string) ([]byte, error)
type inputCommandExecutor func(context.Context, []byte, string, ...string) ([]byte, error)

type Runner struct {
	timeout   time.Duration
	maxOutput int
	execute   commandExecutor
	input     inputCommandExecutor
}

func NewRunner(timeout time.Duration) *Runner {
	return newRunner(timeout, defaultMaxOutput, executeCommand)
}

func newRunner(timeout time.Duration, maxOutput int, execute commandExecutor) *Runner {
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutput
	}
	return &Runner{timeout: timeout, maxOutput: maxOutput, execute: execute, input: executeInputCommand}
}

func (runner *Runner) RunInput(parent context.Context, input []byte, name string, args ...string) ([]byte, error) {
	if runner == nil || runner.input == nil || len(input) > runner.maxOutput || !validCommand(name, args) {
		return nil, errors.New("command invocation is invalid")
	}
	ctx, cancel := context.WithTimeout(parent, runner.timeout)
	defer cancel()
	output, err := runner.input(ctx, append([]byte(nil), input...), name, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("command execution failed")
	}
	if len(output) > runner.maxOutput {
		return nil, errors.New("command output exceeds limit")
	}
	return append([]byte(nil), output...), nil
}

func validCommand(name string, args []string) bool {
	if !strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return false
	}
	for _, argument := range args {
		if strings.ContainsRune(argument, 0) {
			return false
		}
	}
	return true
}

func (runner *Runner) Run(parent context.Context, name string, args ...string) ([]byte, error) {
	if runner == nil || runner.execute == nil || !strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return nil, errors.New("command invocation is invalid")
	}
	for _, argument := range args {
		if strings.ContainsRune(argument, 0) {
			return nil, errors.New("command invocation is invalid")
		}
	}
	ctx, cancel := context.WithTimeout(parent, runner.timeout)
	defer cancel()
	output, err := runner.execute(ctx, name, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("command execution failed")
	}
	if len(output) > runner.maxOutput {
		return nil, errors.New("command output exceeds limit")
	}
	return append([]byte(nil), output...), nil
}

func executeCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	stdout := &boundedBuffer{limit: defaultMaxOutput + 1}
	command.Stdout = stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func executeInputCommand(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = bytes.NewReader(input)
	stdout := &boundedBuffer{limit: defaultMaxOutput + 1}
	command.Stdout = stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining < len(value) {
		buffer.overflow = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = buffer.buffer.Write(value)
	return originalLength, nil
}

func (buffer *boundedBuffer) Bytes() []byte {
	return append([]byte(nil), buffer.buffer.Bytes()...)
}
