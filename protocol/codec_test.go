package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestCodecSkipsBlankKeepaliveLines pins the tolerance a peer may rely on: a
// blank line between frames is not a message.
func TestCodecSkipsBlankKeepaliveLines(t *testing.T) {
	stream := "\n\r\n\n" + `{"version":1,"id":7,"method":"index_status"}` + "\n"
	c := NewCodec(nopCloser{strings.NewReader(stream)})

	req, err := c.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest after blank lines: %v", err)
	}
	if req.ID != 7 || req.Method != "index_status" {
		t.Errorf("read %+v, want the request that followed the blank lines", req)
	}
}

// endlessNewlines is a peer that sends newlines, never sends a message, and
// never hangs up.
//
// The "never hangs up" half is what makes it the real adversary. A FINITE flood
// out of a strings.Reader cannot prove anything about this bug: the read ends in
// io.EOF, AsError renders that as INTERNAL, and the assertion below passes
// whether readFrame loops or recurses — 1,000 newlines satisfy it just as well
// as 4,000,000. (4,000,000 also simply fits: at ~32 bytes of frame per blank
// line, the 1 GiB goroutine stack does not overflow until ~33M of them.) A peer
// under no obligation to stop is the case the daemon actually has to survive.
type endlessNewlines struct{}

func (endlessNewlines) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = '\n'
	}
	return len(p), nil
}
func (endlessNewlines) Write(p []byte) (int, error) { return len(p), nil }
func (endlessNewlines) Close() error                { return nil }

// TestCodecSurvivesANewlineFlood is the regression for a peer that sends
// nothing but newlines.
//
// readFrame used to skip a blank line by CALLING ITSELF. Against a peer that
// keeps sending, that recursion has no bottom: it exhausts the goroutine stack
// and the runtime THROWS `fatal error: stack overflow`. A throw is not a panic —
// recover cannot catch it, the per-connection goroutine cannot be isolated, and
// the WHOLE daemon process dies, taking every other client's language servers
// and warm state with it, because one peer wrote whitespace to a socket it can
// already reach.
//
// Verified: with the recursive implementation restored, this test does not fail,
// it CRASHES the test binary at codec.go readFrame → readFrame → …
//
// The requirement is therefore not merely "an error" — io.EOF would satisfy that
// — but that the codec RETURNS, and returns a structured refusal that names the
// reason, so the daemon logs a protocol error instead of dying.
func TestCodecSurvivesANewlineFlood(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		c := NewCodec(endlessNewlines{})
		_, err := c.ReadRequest()
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ReadRequest never returned on an endless newline stream; " +
			"the blank-line skip is unbounded and the peer decides when it ends")
	}

	if err == nil {
		t.Fatal("a newline flood produced a request")
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("the flood ended in EOF; this peer never hangs up, so the codec " +
			"itself must be what stops it")
	}
	got := AsError(err)
	if got.Code != ErrInternal {
		t.Errorf("a newline flood gave %s, want %s", got.Code, ErrInternal)
	}
	if !strings.Contains(got.Message, "blank") {
		t.Errorf("the refusal %q does not say the peer sent blank frames; "+
			"an operator cannot tell this from any other INTERNAL", got.Message)
	}
}

// TestCodecWriteRefusalIsIdentifiable: a caller must be able to tell "this
// result does not fit in a frame" from "this socket is broken", because the
// first has an answer it can still send and the second does not.
func TestCodecWriteRefusalIsIdentifiable(t *testing.T) {
	var sink syncBuffer
	c := NewCodec(nopCloser{&sink})

	err := c.WriteResponse(&Response{ID: 1, Result: []byte(`"` + strings.Repeat("x", MaxFrameBytes) + `"`)})
	if err == nil {
		t.Fatal("WriteResponse accepted an oversize frame")
	}
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("errors.Is(err, ErrFrameTooLarge) = false for %v", err)
	}
	if code := AsError(err).Code; code != ErrInternal {
		t.Errorf("oversize write gave %s, want %s", code, ErrInternal)
	}
	if n := len(sink.String()); n != 0 {
		t.Errorf("a refused frame put %d bytes on the wire", n)
	}
}

// TestCodecWriteTimeoutBoundsAStalledPeer: a peer that stops reading must not
// be able to park the writer for ever. Without a bound the socket buffer fills,
// Write never returns, and whatever shared capacity that goroutine holds is
// pinned for the lifetime of the process.
func TestCodecWriteTimeoutBoundsAStalledPeer(t *testing.T) {
	left, right := net.Pipe() // an unbuffered conn: nobody reads `right`
	defer left.Close()
	defer right.Close()

	c := NewCodec(left)
	c.SetWriteTimeout(100 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- c.WriteRequest(NewRequest(1, MethodIndexStatus, nil)) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a write to a peer that never reads succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WriteRequest to a stalled peer never returned; the deadline is not applied")
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
