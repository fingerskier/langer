package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// pipe is an in-memory ReadWriteCloser pair standing in for a socket.
type pipeEnd struct {
	io.Reader
	io.Writer
	closeOnce sync.Once
	onClose   func()
}

func (p *pipeEnd) Close() error {
	p.closeOnce.Do(func() {
		if p.onClose != nil {
			p.onClose()
		}
	})
	return nil
}

func newPipe() (*pipeEnd, *pipeEnd) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	a := &pipeEnd{Reader: ar, Writer: aw, onClose: func() { _ = aw.Close(); _ = ar.Close() }}
	b := &pipeEnd{Reader: br, Writer: bw, onClose: func() { _ = bw.Close(); _ = br.Close() }}
	return a, b
}

func TestCodecRoundTripsRequestsAndResponses(t *testing.T) {
	clientEnd, serverEnd := newPipe()
	client := NewCodec(clientEnd)
	server := NewCodec(serverEnd)
	defer client.Close()
	defer server.Close()

	go func() {
		req, err := server.ReadRequest()
		if err != nil {
			t.Errorf("ReadRequest: %v", err)
			return
		}
		resp, err := NewResponse(req.ID, LocationsResult{Locations: []Location{}})
		if err != nil {
			t.Errorf("NewResponse: %v", err)
			return
		}
		if err := server.WriteResponse(resp); err != nil {
			t.Errorf("WriteResponse: %v", err)
		}
	}()

	params, err := json.Marshal(PositionParams{DocumentParams: DocumentParams{Session: "s1", Workspace: "w1", Path: "src/user.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteRequest(NewRequest(7, MethodGetDefinition, params)); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}

	resp, err := client.ReadResponse()
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.ID != 7 {
		t.Errorf("response ID = %d, want 7", resp.ID)
	}
	if resp.Version != Version {
		t.Errorf("response Version = %d, want %d", resp.Version, Version)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
}

// TestCodecFramesAreNewlineDelimited pins the wire format SPEC §3.5 names.
func TestCodecFramesAreNewlineDelimited(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec(nopCloser{&buf})

	if err := c.WriteRequest(NewRequest(1, MethodIndexStatus, nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRequest(NewRequest(2, MethodIndexStatus, nil)); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.Count(out, "\n") != 2 {
		t.Errorf("want exactly one newline per frame, got %q", out)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.Contains(line, "\n") {
			t.Errorf("a frame contains an embedded newline: %q", line)
		}
		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Errorf("frame %q is not standalone JSON: %v", line, err)
		}
	}
}

// TestCodecWritesAreSafeForConcurrentUse: two handlers answering at once must
// never interleave bytes on the wire.
func TestCodecWritesAreSafeForConcurrentUse(t *testing.T) {
	var buf syncBuffer
	c := NewCodec(nopCloser{&buf})

	const writers = 16
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			resp, err := NewResponse(int64(i), SymbolsResult{Symbols: make([]Symbol, 8)})
			if err != nil {
				t.Errorf("NewResponse: %v", err)
				return
			}
			if err := c.WriteResponse(resp); err != nil {
				t.Errorf("WriteResponse: %v", err)
			}
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != writers {
		t.Fatalf("got %d frames, want %d", len(lines), writers)
	}
	seen := map[int64]bool{}
	for _, line := range lines {
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("interleaved frame %q: %v", line, err)
		}
		seen[resp.ID] = true
	}
	if len(seen) != writers {
		t.Errorf("got %d distinct ids, want %d", len(seen), writers)
	}
}

// TestCodecRejectsOversizeFrames: an unbounded read is a memory-exhaustion bug
// waiting for a confused client.
func TestCodecRejectsOversizeFrames(t *testing.T) {
	huge := strings.Repeat("x", MaxFrameBytes+1)
	c := NewCodec(nopCloser{strings.NewReader(`{"version":1,"id":1,"method":"` + huge + `"}` + "\n")})

	_, err := c.ReadRequest()
	if err == nil {
		t.Fatal("ReadRequest accepted an oversize frame")
	}
	if code := AsError(err).Code; code != ErrInternal {
		t.Errorf("oversize frame gave %s, want %s", code, ErrInternal)
	}
}

func TestCodecRejectsMalformedJSON(t *testing.T) {
	c := NewCodec(nopCloser{strings.NewReader("{not json}\n")})
	_, err := c.ReadRequest()
	if err == nil {
		t.Fatal("ReadRequest accepted malformed JSON")
	}
	if code := AsError(err).Code; code != ErrInternal {
		t.Errorf("malformed frame gave %s, want %s", code, ErrInternal)
	}
}

// TestCodecReadReportsEOFUnwrapped lets a connection loop distinguish "the peer
// hung up" from "the peer sent nonsense".
func TestCodecReadReportsEOFUnwrapped(t *testing.T) {
	c := NewCodec(nopCloser{strings.NewReader("")})
	if _, err := c.ReadRequest(); !errors.Is(err, io.EOF) {
		t.Errorf("ReadRequest on a closed stream = %v, want io.EOF", err)
	}
}

// TestCheckVersion is the SPEC §3.1 drain-and-restart trigger.
func TestCheckVersion(t *testing.T) {
	if err := CheckVersion(Version); err != nil {
		t.Errorf("CheckVersion(%d) = %v, want nil", Version, err)
	}

	err := CheckVersion(Version + 1)
	if err == nil {
		t.Fatalf("CheckVersion(%d) = nil, want a mismatch error", Version+1)
	}
	if err.Code != ErrInternal {
		t.Errorf("mismatch code = %s, want %s", err.Code, ErrInternal)
	}
	// The message must name both sides or the operator cannot tell which half
	// is stale.
	for _, want := range []string{"1", "2"} {
		if !strings.Contains(err.Message, want) {
			t.Errorf("mismatch message %q does not name version %s", err.Message, want)
		}
	}
}

func TestCodecCloseUnblocksAReader(t *testing.T) {
	a, b := newPipe()
	client := NewCodec(a)
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		_, err := client.ReadRequest()
		done <- err
	}()

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-done; err == nil {
		t.Error("ReadRequest returned nil after Close")
	}
}

type nopCloser struct{ rw interface{} }

func (n nopCloser) Read(p []byte) (int, error) {
	r, ok := n.rw.(io.Reader)
	if !ok {
		return 0, io.EOF
	}
	return r.Read(p)
}

func (n nopCloser) Write(p []byte) (int, error) {
	w, ok := n.rw.(io.Writer)
	if !ok {
		return len(p), nil
	}
	return w.Write(p)
}

func (n nopCloser) Close() error { return nil }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
