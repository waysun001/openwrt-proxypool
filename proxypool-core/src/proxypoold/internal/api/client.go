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
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, fmt.Errorf("connect control socket: %w", err)
	}
	defer conn.Close()
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-finished:
		}
	}()
	defer close(finished)
	if timeout := c.Timeout; timeout > 0 {
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return Response{}, errors.New("set control deadline")
		}
	} else if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return Response{}, errors.New("set control deadline")
		}
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
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
