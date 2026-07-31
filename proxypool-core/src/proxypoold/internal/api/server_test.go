package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"proxypoold/internal/api"
)

type recordingHandler struct {
	calls atomic.Int32
	fn    func(context.Context, api.Request) api.Response
}

func (h *recordingHandler) Handle(ctx context.Context, req api.Request) api.Response {
	h.calls.Add(1)
	if h.fn != nil {
		return h.fn(ctx, req)
	}
	return api.Response{Result: json.RawMessage(`{"status":"ok"}`)}
}

func startServer(t *testing.T, handler api.Handler, configure func(*api.Server)) (string, context.CancelFunc) {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	s := &api.Server{Path: path, Handler: handler, ReadTimeout: time.Second, WriteTimeout: time.Second, HandlerTimeout: time.Second}
	if configure != nil {
		configure(s)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Lstat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("server did not create socket")
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Serve() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve did not stop")
		}
	})
	return path, cancel
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "pp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func exchange(t *testing.T, path string, payload []byte) api.Response {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer c.Close()
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 0, 256)
	buf := make([]byte, 256)
	for {
		n, err := c.Read(buf)
		data = append(data, buf[:n]...)
		if bytes.Contains(data, []byte{'\n'}) {
			break
		}
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
	}
	if !bytes.HasSuffix(data, []byte{'\n'}) {
		t.Fatalf("response missing newline")
	}
	var response api.Response
	if err := json.Unmarshal(bytes.TrimSpace(data), &response); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	return response
}

func assertError(t *testing.T, response api.Response, id, code string) {
	t.Helper()
	if response.Version != api.ProtocolVersion || response.ID != id || response.Error == nil || response.Error.Code != code {
		t.Fatalf("response = %#v, want version=%d id=%q error=%q", response, api.ProtocolVersion, id, code)
	}
}

func TestServerGoodRequestAndSocketMode(t *testing.T) {
	h := &recordingHandler{}
	path, _ := startServer(t, h, nil)
	response := exchange(t, path, []byte(`{"version":1,"id":"good","method":"status.get","params":{}}`+"\n"))
	if response.Version != api.ProtocolVersion || response.ID != "good" || string(response.Result) != `{"status":"ok"}` || response.Error != nil {
		t.Fatalf("response = %#v", response)
	}
	if h.calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1", h.calls.Load())
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("socket mode = %o, want 0600", got)
		}
	}
}

func TestServerRejectsInvalidRequestsWithoutCallingHandler(t *testing.T) {
	cases := []struct {
		name, payload, id, code string
	}{
		{"unknown version", `{"version":2,"id":"v","method":"status.get","params":{}}` + "\n", "v", "unsupported_version"},
		{"unknown method", `{"version":1,"id":"m","method":"not.a.method","params":{}}` + "\n", "m", "unknown_method"},
		{"malformed", `{"version":1,"id":"broken",` + "\n", "broken", "invalid_request"},
		{"unknown field", `{"version":1,"id":"u","method":"status.get","params":{},"extra":1}` + "\n", "u", "invalid_request"},
		{"duplicate field", `{"version":1,"id":"first","id":"second","method":"status.get","params":{}}` + "\n", "second", "invalid_request"},
		{"trailing JSON", `{"version":1,"id":"t","method":"status.get","params":{}} {}` + "\n", "t", "invalid_request"},
		{"missing newline", `{"version":1,"id":"n","method":"status.get","params":{}}`, "n", "invalid_request"},
		{"second message", `{"version":1,"id":"one","method":"status.get","params":{}}` + "\n" + `{"version":1,"id":"two","method":"status.get","params":{}}` + "\n", "one", "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingHandler{}
			path, _ := startServer(t, h, nil)
			response := exchange(t, path, []byte(tc.payload))
			assertError(t, response, tc.id, tc.code)
			if h.calls.Load() != 0 {
				t.Fatalf("handler calls = %d, want 0", h.calls.Load())
			}
		})
	}
}

func TestServerRequestSizeBoundary(t *testing.T) {
	base := `{"version":1,"id":"size","method":"status.get","params":{"padding":"` + strings.Repeat("x", api.MaxFrameSize-80) + `"}}`
	if len(base) > api.MaxFrameSize {
		t.Fatal("test payload unexpectedly too large")
	}
	exact := base + strings.Repeat(" ", api.MaxFrameSize-len(base)) + "\n"
	h := &recordingHandler{}
	path, _ := startServer(t, h, nil)
	response := exchange(t, path, []byte(exact))
	if response.Error != nil || h.calls.Load() != 1 {
		t.Fatalf("exact max response=%#v calls=%d", response, h.calls.Load())
	}

	h = &recordingHandler{}
	path, _ = startServer(t, h, nil)
	response = exchange(t, path, []byte(exact[:len(exact)-1]+"x\n"))
	assertError(t, response, "size", "request_too_large")
	if h.calls.Load() != 0 {
		t.Fatalf("handler calls = %d, want 0", h.calls.Load())
	}
}

func TestServerTimeoutAndPanicAreStructured(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		h := &recordingHandler{fn: func(ctx context.Context, _ api.Request) api.Response {
			<-ctx.Done()
			return api.Response{}
		}}
		path, _ := startServer(t, h, func(s *api.Server) { s.HandlerTimeout = 10 * time.Millisecond })
		assertError(t, exchange(t, path, []byte(`{"version":1,"id":"slow","method":"status.get","params":{}}`+"\n")), "slow", "operation_timeout")
	})
	t.Run("panic", func(t *testing.T) {
		h := &recordingHandler{fn: func(context.Context, api.Request) api.Response { panic("no secret here") }}
		path, _ := startServer(t, h, nil)
		assertError(t, exchange(t, path, []byte(`{"version":1,"id":"panic","method":"status.get","params":{}}`+"\n")), "panic", "internal_error")
	})
}

func TestServerPreservesNonSocketPathsAndCleansOwnSocket(t *testing.T) {
	t.Run("stale socket", func(t *testing.T) {
		path := filepath.Join(shortTempDir(t), "control.sock")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- (&api.Server{Path: path, Handler: &recordingHandler{}}).Serve(ctx) }()
		for i := 0; i < 100 && !fileExists(path); i++ {
			time.Sleep(time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("regular", func(t *testing.T) {
		path := filepath.Join(shortTempDir(t), "control.sock")
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := (&api.Server{Path: path, Handler: &recordingHandler{}}).Serve(context.Background())
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("Serve error = %v", err)
		}
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != "keep" {
			t.Fatalf("path changed: %q %v", contents, err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(shortTempDir(t), "target")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(filepath.Dir(target), "control.sock")
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		err := (&api.Server{Path: path, Handler: &recordingHandler{}}).Serve(context.Background())
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("Serve error = %v", err)
		}
		contents, err := os.ReadFile(target)
		if err != nil || string(contents) != "keep" {
			t.Fatalf("target changed: %q %v", contents, err)
		}
	})
	t.Run("cancel cleanup", func(t *testing.T) {
		path := filepath.Join(shortTempDir(t), "control.sock")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- (&api.Server{Path: path, Handler: &recordingHandler{}}).Serve(ctx) }()
		for i := 0; i < 100 && !fileExists(path); i++ {
			time.Sleep(time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if fileExists(path) {
			t.Fatal("owned socket remains after cancellation")
		}
	})
}

func fileExists(path string) bool { _, err := os.Lstat(path); return err == nil }

func TestClientValidatesResponseAndNeverFormatsRequestParams(t *testing.T) {
	secret := json.RawMessage(`{"password":"super-secret"}`)
	req := api.Request{Version: 1, ID: "client", Method: "status.get", Params: secret}
	if strings.Contains(req.String(), "super-secret") || strings.Contains(fmt.Sprintf("%#v", req), "super-secret") || strings.Contains(fmt.Sprintf("%+v", req), "super-secret") {
		t.Fatal("request formatter exposed secret")
	}

	path := filepath.Join(shortTempDir(t), "peer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte(`{"version":1,"id":"wrong","result":{}}` + "\n"))
	}()
	_, err = (&api.Client{Path: path, Timeout: time.Second}).Call(context.Background(), req)
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("Call error = %v", err)
	}
}

func TestClientCancellationAndMalformedResponse(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		path := filepath.Join(shortTempDir(t), "peer.sock")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				defer conn.Close()
				time.Sleep(time.Second)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_, err = (&api.Client{Path: path}).Call(ctx, api.Request{Version: 1, ID: "cancel", Method: "status.get", Params: json.RawMessage(`{}`)})
		if err == nil {
			t.Fatal("Call unexpectedly succeeded after cancellation")
		}
	})
	t.Run("unknown response field", func(t *testing.T) {
		path := filepath.Join(shortTempDir(t), "peer.sock")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			buf := make([]byte, 1024)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte(`{"version":1,"id":"client","result":{},"extra":true}` + "\n"))
		}()
		_, err = (&api.Client{Path: path, Timeout: time.Second}).Call(context.Background(), api.Request{Version: 1, ID: "client", Method: "status.get", Params: json.RawMessage(`{}`)})
		if err == nil {
			t.Fatal("Call accepted unknown response field")
		}
	})
}
