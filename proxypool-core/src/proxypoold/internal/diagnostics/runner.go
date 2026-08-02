package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
)

type ExecRunner struct{}

func (ExecRunner) RunBounded(ctx context.Context, limit int, name string, args ...string) ([]byte, bool, error) {
	if ctx == nil || limit < 1 {
		return nil, false, errors.New("bounded command request is invalid")
	}
	output := &limitedWriter{remaining: limit}
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = output
	command.Stderr = io.Discard
	err := command.Run()
	return output.Bytes(), output.truncated, err
}

type limitedWriter struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (writer *limitedWriter) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > writer.remaining {
		value = value[:writer.remaining]
		writer.truncated = true
	}
	_, _ = writer.buffer.Write(value)
	writer.remaining -= len(value)
	return original, nil
}

func (writer *limitedWriter) Bytes() []byte {
	return append([]byte(nil), writer.buffer.Bytes()...)
}
