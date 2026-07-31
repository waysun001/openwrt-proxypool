package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Client performs one local Unix-socket protocol exchange per Call.
type Client struct {
	Path    string
	Timeout time.Duration
}

const defaultClientTimeout = 10 * time.Second

func (c *Client) Call(ctx context.Context, request Request) (Response, error) {
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > MaxFrameSize {
		return Response{}, errors.New("invalid local control request")
	}
	if _, err := ParseRequest(encoded); err != nil {
		return Response{}, errors.New("invalid local control request")
	}
	path := c.Path
	if path == "" {
		path = DefaultSocketPath
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultClientTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(callCtx, "unix", path)
	if err != nil {
		return Response{}, fmt.Errorf("connect control socket: %w", err)
	}
	defer conn.Close()
	finished := make(chan struct{})
	go func() {
		select {
		case <-callCtx.Done():
			_ = conn.Close()
		case <-finished:
		}
	}()
	defer close(finished)
	if deadline, ok := callCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return Response{}, errors.New("set control deadline")
		}
	}
	if err := writeAll(conn, append(encoded, '\n')); err != nil {
		return Response{}, errors.New("write control request")
	}
	frame, err := readFrame(conn)
	if err != nil {
		return Response{}, errors.New("invalid control response")
	}
	response, err := parseResponse(frame)
	if err != nil || response.Version != ProtocolVersion || response.ID != request.ID {
		return Response{}, errors.New("invalid control response")
	}
	return response, nil
}
