package wire_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/fingerskier/langer/lsp/wire"
)

type rw struct {
	io.Reader
	io.Writer
}

func TestFramerReadsContentLengthFrame(t *testing.T) {
	t.Parallel()
	body := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	raw := "Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body

	f := wire.NewFramer(rw{Reader: strings.NewReader(raw), Writer: io.Discard})
	m, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(m.ID) != "1" {
		t.Errorf("ID = %s, want 1", m.ID)
	}
	if string(m.Result) != `{"ok":true}` {
		t.Errorf("Result = %s", m.Result)
	}
}

// Real servers send Content-Type headers, extra headers, and varying case.
func TestFramerToleratesExtraHeaders(t *testing.T) {
	t.Parallel()
	body := `{"jsonrpc":"2.0","method":"ping"}`
	raw := "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n" +
		"content-length: " + itoa(len(body)) + "\r\n" +
		"\r\n" + body

	f := wire.NewFramer(rw{Reader: strings.NewReader(raw), Writer: io.Discard})
	m, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Method != "ping" {
		t.Errorf("Method = %q, want ping", m.Method)
	}
}

func TestFramerReadsBackToBackFrames(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"a"}`,
		`{"jsonrpc":"2.0","method":"b"}`,
		`{"jsonrpc":"2.0","method":"c"}`,
	} {
		buf.WriteString("Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body)
	}

	f := wire.NewFramer(rw{Reader: &buf, Writer: io.Discard})
	for _, want := range []string{"a", "b", "c"} {
		m, err := f.Read()
		if err != nil {
			t.Fatalf("Read %q: %v", want, err)
		}
		if m.Method != want {
			t.Fatalf("Method = %q, want %q", m.Method, want)
		}
	}
	if _, err := f.Read(); err != io.EOF {
		t.Fatalf("Read after the last frame = %v, want io.EOF", err)
	}
}

// Content-Length counts BYTES, not characters. A frame whose payload contains
// non-BMP text must not be truncated.
func TestFramerCountsBytesNotRunes(t *testing.T) {
	t.Parallel()
	body := `{"jsonrpc":"2.0","method":"log","params":{"text":"🚀🚀🚀🚀🚀🚀🚀🚀"}}`
	raw := "Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body

	f := wire.NewFramer(rw{Reader: strings.NewReader(raw), Writer: io.Discard})
	m, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var params struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	if params.Text != "🚀🚀🚀🚀🚀🚀🚀🚀" {
		t.Fatalf("text = %q", params.Text)
	}
}

func TestFramerRejectsMissingContentLength(t *testing.T) {
	t.Parallel()
	f := wire.NewFramer(rw{Reader: strings.NewReader("X-Nothing: 1\r\n\r\n{}"), Writer: io.Discard})
	if _, err := f.Read(); err == nil {
		t.Fatal("Read accepted a frame with no Content-Length")
	}
}

func TestFramerRejectsAbsurdContentLength(t *testing.T) {
	t.Parallel()
	f := wire.NewFramer(rw{Reader: strings.NewReader("Content-Length: 999999999\r\n\r\n{}"), Writer: io.Discard})
	if _, err := f.Read(); err == nil {
		t.Fatal("Read accepted an oversized Content-Length")
	}
}

func TestFramerTruncatedBodyIsAnError(t *testing.T) {
	t.Parallel()
	f := wire.NewFramer(rw{Reader: strings.NewReader("Content-Length: 100\r\n\r\n{\"a\":1}"), Writer: io.Discard})
	if _, err := f.Read(); err == nil {
		t.Fatal("Read accepted a truncated body")
	}
}

func TestFramerEOFBeforeAnyHeader(t *testing.T) {
	t.Parallel()
	f := wire.NewFramer(rw{Reader: strings.NewReader(""), Writer: io.Discard})
	if _, err := f.Read(); err != io.EOF {
		t.Fatalf("Read on an empty stream = %v, want io.EOF", err)
	}
}

func TestFramerWriteRoundTrip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	out := wire.NewFramer(rw{Reader: strings.NewReader(""), Writer: &buf})

	if err := out.Write(wire.Message{
		ID:     json.RawMessage(`7`),
		Method: "textDocument/definition",
		Params: json.RawMessage(`{"x":1}`),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "Content-Length: ") {
		t.Fatalf("frame does not start with Content-Length: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\r\n\r\n") {
		t.Fatalf("frame has no header terminator: %q", buf.String())
	}

	in := wire.NewFramer(rw{Reader: bytes.NewReader(buf.Bytes()), Writer: io.Discard})
	m, err := in.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %q, want 2.0", m.JSONRPC)
	}
	if m.Method != "textDocument/definition" || string(m.ID) != "7" {
		t.Errorf("round trip lost fields: %+v", m)
	}
}

// A response carrying a null result must serialise "result":null, not omit the
// key — an omitted result is not a valid JSON-RPC response and some servers
// treat it as a protocol error.
func TestFramerWritesExplicitNullResult(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	f := wire.NewFramer(rw{Reader: strings.NewReader(""), Writer: &buf})
	if err := f.Write(wire.Message{ID: json.RawMessage(`"abc"`)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	body := buf.String()[strings.Index(buf.String(), "\r\n\r\n")+4:]
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	raw, ok := decoded["result"]
	if !ok {
		t.Fatalf("response has no result key: %s", body)
	}
	if string(raw) != "null" {
		t.Fatalf("result = %s, want null", raw)
	}
	if _, ok := decoded["method"]; ok {
		t.Fatalf("response carries a method key: %s", body)
	}
}

// A notification has no id at all; sending "id":null makes servers answer it.
func TestFramerNotificationOmitsID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	f := wire.NewFramer(rw{Reader: strings.NewReader(""), Writer: &buf})
	if err := f.Write(wire.Message{Method: "initialized", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	body := buf.String()[strings.Index(buf.String(), "\r\n\r\n")+4:]
	if strings.Contains(body, `"id"`) {
		t.Fatalf("notification carries an id: %s", body)
	}
	if strings.Contains(body, `"result"`) {
		t.Fatalf("notification carries a result: %s", body)
	}
}

// Request ids are string-or-number on the wire; real servers send both.
func TestFramerPreservesStringIDs(t *testing.T) {
	t.Parallel()
	body := `{"jsonrpc":"2.0","id":"req-42","method":"client/registerCapability","params":{}}`
	f := wire.NewFramer(rw{Reader: strings.NewReader("Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body), Writer: io.Discard})
	m, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(m.ID) != `"req-42"` {
		t.Fatalf("ID = %s, want \"req-42\"", m.ID)
	}
}

func TestFramerDecodesRPCError(t *testing.T) {
	t.Parallel()
	body := `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"Unhandled method"}}`
	f := wire.NewFramer(rw{Reader: strings.NewReader("Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body), Writer: io.Discard})
	m, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Error == nil {
		t.Fatal("Error is nil")
	}
	if m.Error.Code != -32601 || m.Error.Message != "Unhandled method" {
		t.Fatalf("Error = %+v", m.Error)
	}
}

// Writes are serialised internally: two goroutines must never interleave frames
// on the wire.
func TestFramerWritesAreConcurrencySafe(t *testing.T) {
	t.Parallel()
	var buf lockedBuffer
	f := wire.NewFramer(rw{Reader: strings.NewReader(""), Writer: &buf})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = f.Write(wire.Message{Method: "note", Params: json.RawMessage(`{"i":` + itoa(i) + `}`)})
		}(i)
	}
	wg.Wait()

	in := wire.NewFramer(rw{Reader: bytes.NewReader(buf.Bytes()), Writer: io.Discard})
	for i := 0; i < 32; i++ {
		if _, err := in.Read(); err != nil {
			t.Fatalf("frame %d unreadable — writes interleaved: %v", i, err)
		}
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.buf.Bytes()...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
