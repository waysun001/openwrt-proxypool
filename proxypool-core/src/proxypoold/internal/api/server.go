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
	"syscall"
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
	MaxHandlers                                                int
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
	lock, err := acquireEndpointLock(path + ".lock")
	if err != nil {
		return errors.New("control socket is already owned")
	}
	defer lock.Close()
	if err := removeStaleSocket(path); err != nil {
		return err
	}
	listener, err := listenUnixPrivate(path)
	if err != nil {
		return fmt.Errorf("listen control socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	created, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("inspect created control socket: %w", err)
	}
	defer listener.Close()
	defer cleanupCreatedSocket(path, created)
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
	maxHandlers := s.MaxHandlers
	if maxHandlers <= 0 {
		maxHandlers = maxConnections
	}
	sem := make(chan struct{}, maxConnections)
	handlerSem := make(chan struct{}, maxHandlers)
	methods := cloneMethods(s.Methods)
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
			if ctx.Err() != nil {
				activeMu.Unlock()
				<-sem
				_ = conn.Close()
				workers.Done()
				continue
			}
			active[conn] = struct{}{}
			activeMu.Unlock()
			go func() {
				defer workers.Done()
				defer func() { <-sem }()
				defer conn.Close()
				defer func() { activeMu.Lock(); delete(active, conn); activeMu.Unlock() }()
				s.serveConn(ctx, conn, readTimeout, writeTimeout, handlerTimeout, handlerSem, methods)
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
		conn, probeErr := (&net.Dialer{Timeout: 100 * time.Millisecond}).Dial("unix", path)
		if probeErr == nil {
			_ = conn.Close()
			return errors.New("refusing to replace active control socket")
		}
		return errors.New("refusing to replace non-socket control path")
	}
	conn, probeErr := (&net.Dialer{Timeout: 100 * time.Millisecond}).Dial("unix", path)
	if probeErr == nil {
		_ = conn.Close()
		return errors.New("refusing to replace active control socket")
	}
	if !errors.Is(probeErr, syscall.ECONNREFUSED) {
		return errors.New("refusing to replace uncertain control socket")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale control socket: %w", err)
	}
	return nil
}

func cleanupCreatedSocket(path string, created os.FileInfo) {
	current, err := os.Lstat(path)
	if err == nil && os.SameFile(created, current) && (current.Mode()&os.ModeSocket != 0 || runtime.GOOS == "windows") {
		_ = os.Remove(path)
	}
}

func cloneMethods(methods map[string]struct{}) map[string]struct{} {
	if methods == nil {
		methods = defaultMethods
	}
	copy := make(map[string]struct{}, len(methods))
	for method := range methods {
		copy[method] = struct{}{}
	}
	return copy
}

func (s *Server) serveConn(parent context.Context, conn net.Conn, readTimeout, writeTimeout, handlerTimeout time.Duration, handlerSem chan struct{}, methods map[string]struct{}) {
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
	if _, known := methods[req.Method]; !known {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = writeResponse(conn, errorResponse(id, "unknown_method", messageFor("unknown_method")))
		return
	}
	select {
	case handlerSem <- struct{}{}:
	default:
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = writeResponse(conn, errorResponse(id, "server_busy", "control server is busy"))
		return
	}
	callCtx, cancel := context.WithTimeout(parent, handlerTimeout)
	defer cancel()
	result := make(chan Response, 1)
	go func() {
		defer func() { <-handlerSem }()
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
		response = normalizeResponse(response, id)
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
	return writeAll(w, append(encoded, '\n'))
}

var errNoWriteProgress = errors.New("write made no progress")

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
			return errNoWriteProgress
		}
	}
	return nil
}

func normalizeResponse(response Response, id string) Response {
	response.Version, response.ID = ProtocolVersion, id
	hasResult, hasError := response.Result != nil, response.Error != nil
	if hasResult == hasError || (hasResult && !json.Valid(response.Result)) || (hasError && (response.Error.Code == "" || response.Error.Message == "")) {
		return errorResponse(id, "internal_error", messageFor("internal_error"))
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > MaxFrameSize {
		return errorResponse(id, "internal_error", messageFor("internal_error"))
	}
	return response
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
