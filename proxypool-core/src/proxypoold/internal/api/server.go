package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const DefaultSocketPath = "/var/run/proxypoold.sock"

var defaultMethods = map[string]struct{}{
	"status.get": {}, "node.save": {}, "node.delete": {}, "node.action": {}, "device.list": {}, "device.bind": {}, "device.unbind": {}, "import.preview": {}, "import.commit": {}, "job.get": {}, "job.list": {}, "system.activate": {}, "system.events": {}, "diagnostics.create": {},
}

// Handler is deliberately small so business methods can remain independently testable.
type Handler interface {
	Handle(context.Context, Request) Response
}

// Server serves one bounded request per Unix connection.
type Server struct {
	Path                                                       string
	Handler                                                    Handler
	ReadTimeout, WriteTimeout, HandlerTimeout, ShutdownTimeout time.Duration
	MaxConnections                                             int
	// Methods overrides the stable method set. A nil map uses the protocol's stable set.
	Methods map[string]struct{}
}

func (s *Server) Serve(ctx context.Context) error {
	if s.Handler == nil {
		return errors.New("api server requires a handler")
	}
	path := s.Path
	if path == "" {
		path = DefaultSocketPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen control socket: %w", err)
	}
	defer func() { _ = listener.Close(); _ = os.Remove(path) }()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("chmod control socket: %w", err)
		}
	}
	readTimeout, writeTimeout, handlerTimeout, shutdownTimeout := s.timeouts()
	maxConnections := s.MaxConnections
	if maxConnections <= 0 {
		maxConnections = 64
	}
	sem := make(chan struct{}, maxConnections)
	var workers sync.WaitGroup
	var activeMu sync.Mutex
	active := make(map[net.Conn]struct{})
	stopWatch, watchDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = listener.Close()
			activeMu.Lock()
			for conn := range active {
				_ = conn.Close()
			}
			activeMu.Unlock()
		case <-stopWatch:
		}
	}()
	defer func() {
		close(stopWatch)
		<-watchDone
		activeMu.Lock()
		for conn := range active {
			_ = conn.Close()
		}
		activeMu.Unlock()
		waitWorkers(&workers, shutdownTimeout)
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept control socket: %w", err)
		}
		select {
		case sem <- struct{}{}:
			workers.Add(1)
			activeMu.Lock()
			active[conn] = struct{}{}
			activeMu.Unlock()
			go func() {
				defer workers.Done()
				defer func() { <-sem }()
				defer conn.Close()
				defer func() { activeMu.Lock(); delete(active, conn); activeMu.Unlock() }()
				s.serveConn(ctx, conn, readTimeout, writeTimeout, handlerTimeout)
			}()
		default:
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			_ = writeResponse(conn, errorResponse("", "server_busy", "control server is busy"))
			_ = conn.Close()
		}
	}
}

func (s *Server) timeouts() (time.Duration, time.Duration, time.Duration, time.Duration) {
	read, write, handler, shutdown := s.ReadTimeout, s.WriteTimeout, s.HandlerTimeout, s.ShutdownTimeout
	if read <= 0 {
		read = 5 * time.Second
	}
	if write <= 0 {
		write = 5 * time.Second
	}
	if handler <= 0 {
		handler = 10 * time.Second
	}
	if shutdown <= 0 {
		shutdown = 5 * time.Second
	}
	return read, write, handler, shutdown
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect control socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("refusing to replace non-socket control path")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale control socket: %w", err)
	}
	return nil
}

func (s *Server) serveConn(parent context.Context, conn net.Conn, readTimeout, writeTimeout, handlerTimeout time.Duration) {
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	frame, err := readFrame(conn)
	if err != nil {
		code := "invalid_request"
		if errors.Is(err, errFrameTooLarge) {
			code = "request_too_large"
		}
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = writeResponse(conn, errorResponse(requestID(frame), code, messageFor(code)))
		return
	}
	id := requestID(frame)
	req, err := ParseRequest(frame)
	if err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = writeResponse(conn, errorResponse(id, "invalid_request", messageFor("invalid_request")))
		return
	}
	id = req.ID
	if req.Version != ProtocolVersion {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = writeResponse(conn, errorResponse(id, "unsupported_version", messageFor("unsupported_version")))
		return
	}
	if !s.knownMethod(req.Method) {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = writeResponse(conn, errorResponse(id, "unknown_method", messageFor("unknown_method")))
		return
	}
	callCtx, cancel := context.WithTimeout(parent, handlerTimeout)
	defer cancel()
	result := make(chan Response, 1)
	go func() {
		defer func() {
			if recover() != nil {
				result <- errorResponse(id, "internal_error", messageFor("internal_error"))
			}
		}()
		result <- s.Handler.Handle(callCtx, req)
	}()
	var response Response
	select {
	case response = <-result:
		response.Version, response.ID = ProtocolVersion, id
	case <-callCtx.Done():
		response = errorResponse(id, "operation_timeout", messageFor("operation_timeout"))
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = writeResponse(conn, response)
}

func (s *Server) knownMethod(method string) bool {
	methods := s.Methods
	if methods == nil {
		methods = defaultMethods
	}
	_, ok := methods[method]
	return ok
}
func waitWorkers(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

var errFrameTooLarge = errors.New("frame too large")

func readFrame(conn net.Conn) ([]byte, error) {
	// One byte of bounded look-ahead rejects a second frame delivered alongside a maximum-sized first frame.
	reader := bufio.NewReader(io.LimitReader(conn, MaxFrameSize+2))
	line, err := reader.ReadBytes('\n')
	if len(line) > MaxFrameSize+1 {
		return line, errFrameTooLarge
	}
	if err != nil || len(line) == 0 || line[len(line)-1] != '\n' {
		return line, errInvalidRequest
	}
	frame := line[:len(line)-1]
	if len(frame) > MaxFrameSize || reader.Buffered() != 0 {
		if len(frame) > MaxFrameSize {
			return frame, errFrameTooLarge
		}
		return frame, errInvalidRequest
	}
	return frame, nil
}

func writeResponse(w io.Writer, response Response) error {
	response.Version = ProtocolVersion
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(encoded) > MaxFrameSize {
		return errFrameTooLarge
	}
	_, err = w.Write(append(encoded, '\n'))
	return err
}
func errorResponse(id, code, message string) Response {
	return Response{Version: ProtocolVersion, ID: id, Error: &Error{Code: code, Message: message}}
}
func messageFor(code string) string {
	switch code {
	case "invalid_request":
		return "request must be one valid protocol JSON object"
	case "request_too_large":
		return "request exceeds maximum frame size"
	case "unsupported_version":
		return "unsupported protocol version"
	case "unknown_method":
		return "unknown control method"
	case "operation_timeout":
		return "operation timed out"
	case "internal_error":
		return "internal control error"
	default:
		return "control protocol error"
	}
}
