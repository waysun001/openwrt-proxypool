package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"proxypoold/internal/api"
)

func TestRunVersion(t *testing.T) {
	var out, err bytes.Buffer
	if code := run([]string{"--version"}, bytes.NewReader(nil), &out, &err); code != 0 || out.Len() == 0 || err.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), err.String())
	}
}

func TestRunCallSeparatesResponseAndDiagnostics(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "pp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "ctl.sock")
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
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte(`{"version":1,"id":"cli","result":{"ok":true}}` + "\n"))
	}()
	var out, stderr bytes.Buffer
	code := run([]string{"call", "--socket", path}, bytes.NewBufferString(`{"version":1,"id":"cli","method":"status.get","params":{}}`), &out, &stderr)
	if code != 0 || stderr.Len() != 0 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
	var response map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil || response["id"] != "cli" {
		t.Fatalf("stdout invalid: %v %q", err, out.String())
	}
}

func TestRunCallRejectsMalformedAndOversizeWithoutEchoingInput(t *testing.T) {
	for _, input := range []string{`{"version":1,"id":"x","method":"status.get","params":{},"extra":1}`, `{"version":1,"id":"x","method":"status.get","params":{"password":"cli-secret"}}` + string(bytes.Repeat([]byte("x"), 1<<20))} {
		var out, stderr bytes.Buffer
		code := run([]string{"call"}, bytes.NewBufferString(input), &out, &stderr)
		if code == 0 || out.Len() != 0 || bytes.Contains(stderr.Bytes(), []byte("cli-secret")) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
		}
	}
}

func TestRunCallRequiresOneObject(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run([]string{"call"}, bytes.NewBufferString(`{"version":1,"id":"a","method":"status.get","params":{}} {}`), &out, &stderr)
	if code == 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
	_ = context.Background()
	_ = os.ErrNotExist
	_ = time.Second
}

func TestRunCallRejectsUnknownOption(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := run([]string{"call", "--bad"}, bytes.NewBufferString(`{}`), &out, &stderr); code == 0 || out.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
}

func TestRunCallAcceptsExactMaxPayloadWithTerminator(t *testing.T) {
	base := `{"version":1,"id":"max","method":"status.get","params":{"padding":"` + string(bytes.Repeat([]byte("x"), 64)) + `"}}`
	input := base + string(bytes.Repeat([]byte(" "), api.MaxFrameSize-len(base)))
	if len(input) != api.MaxFrameSize {
		t.Fatalf("payload size=%d", len(input))
	}
	for _, suffix := range []string{"\n", "\r\n"} {
		var out, stderr bytes.Buffer
		code := run([]string{"call", "--socket", "missing.sock"}, bytes.NewBufferString(input+suffix), &out, &stderr)
		if code != 1 || out.Len() != 0 || bytes.Contains(stderr.Bytes(), []byte("padding")) {
			t.Fatalf("suffix=%q code=%d stdout=%q stderr=%q", suffix, code, out.String(), stderr.String())
		}
	}
	var out, stderr bytes.Buffer
	if code := run([]string{"call"}, bytes.NewBufferString(input+"x\n"), &out, &stderr); code != 2 || out.Len() != 0 {
		t.Fatalf("oversize code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
}

type shortWriter struct{ limit int }

func (w shortWriter) Write(p []byte) (int, error) {
	if w.limit == 0 {
		return 0, nil
	}
	if len(p) > w.limit {
		return w.limit, nil
	}
	return len(p), nil
}

func TestRunFailsWhenStdoutCannotMakeProgress(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--version"}, bytes.NewReader(nil), shortWriter{}, &stderr); code == 0 {
		t.Fatal("version succeeded with zero-progress stdout")
	}
}
