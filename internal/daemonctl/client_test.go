package daemonctl

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/fingerskier/langer/protocol"
)

// scriptedPeer answers requests however a test tells it to, over a real socket
// pair.
type scriptedPeer struct {
	t      *testing.T
	server net.Conn
	codec  *protocol.Codec

	mu       sync.Mutex
	received []*protocol.Request
}

func newScriptedPeer(t *testing.T, handle func(p *scriptedPeer, req *protocol.Request)) *Client {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	peer := &scriptedPeer{t: t, server: serverConn, codec: protocol.NewCodec(serverConn)}

	go func() {
		for {
			req, err := peer.codec.ReadRequest()
			if err != nil {
				return
			}
			peer.mu.Lock()
			peer.received = append(peer.received, req)
			peer.mu.Unlock()
			handle(peer, req)
		}
	}()

	client := newClient(clientConn, "/repo", testLogger(t))
	t.Cleanup(func() {
		_ = client.Close()
		_ = serverConn.Close()
	})
	return client
}

func (p *scriptedPeer) answer(req *protocol.Request, result any) {
	resp, err := protocol.NewResponse(req.ID, result)
	if err != nil {
		p.t.Errorf("NewResponse: %v", err)
		return
	}
	_ = p.codec.WriteResponse(resp)
}

// TestClientDemuxesOutOfOrderResponses: the daemon answers requests
// concurrently, so responses arrive in whatever order the handlers finish. A
// client that assumes FIFO hands one caller another's answer.
func TestClientDemuxesOutOfOrderResponses(t *testing.T) {
	var (
		mu      sync.Mutex
		queued  []*protocol.Request
		release = make(chan struct{})
	)

	client := newScriptedPeer(t, func(p *scriptedPeer, req *protocol.Request) {
		mu.Lock()
		queued = append(queued, req)
		n := len(queued)
		batch := append([]*protocol.Request(nil), queued...)
		mu.Unlock()

		if n < 3 {
			return
		}
		// Answer them backwards, once all three have arrived.
		go func() {
			<-release
			for i := len(batch) - 1; i >= 0; i-- {
				p.answer(batch[i], protocol.IndexStatusResult{Root: string(rune('A' + i))})
			}
		}()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := make([]string, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := client.IndexStatus(ctx, protocol.IndexStatusParams{
				Session: protocol.SessionID(string(rune('a' + i))), Workspace: "ws",
			})
			if err != nil {
				t.Errorf("IndexStatus %d: %v", i, err)
				return
			}
			results[i] = out.Root
		}(i)
	}

	// Wait for all three to be in flight, then let the peer answer.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(queued)
		mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	seen := map[string]bool{}
	for i, got := range results {
		if got == "" {
			t.Fatalf("call %d got no result", i)
		}
		if seen[got] {
			t.Fatalf("two calls got the same answer %q: %v", got, results)
		}
		seen[got] = true
	}
}

// TestClientFailsPendingCallsWhenTheDaemonDies: a caller left to time out
// cannot tell a dead daemon from a slow one, and SPEC §3.6 admits no
// unstructured error.
func TestClientFailsPendingCallsWhenTheDaemonDies(t *testing.T) {
	killed := make(chan struct{})
	client := newScriptedPeer(t, func(p *scriptedPeer, _ *protocol.Request) {
		close(killed)
		_ = p.server.Close() // the daemon dies mid-request
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.IndexStatus(ctx, protocol.IndexStatusParams{Session: "alice", Workspace: "ws"})
	<-killed

	if err == nil {
		t.Fatal("the call succeeded against a dead daemon")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the call waited %v instead of failing when the connection died", elapsed)
	}
	if code := protocol.AsError(err).Code; code != protocol.ErrNotReady {
		t.Errorf("a dead daemon reported %s, want %s so the caller retries into a fresh one", code, protocol.ErrNotReady)
	}
}

// TestClientPropagatesStructuredErrors: a code that does not survive the socket
// is a code an agent can never act on.
func TestClientPropagatesStructuredErrors(t *testing.T) {
	client := newScriptedPeer(t, func(p *scriptedPeer, req *protocol.Request) {
		_ = p.codec.WriteResponse(protocol.NewErrorResponse(req.ID,
			protocol.NewError(protocol.ErrServerCrashed, "typescript died").WithRetryAfterMS(250)))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.GetHover(ctx, protocol.PositionParams{
		DocumentParams: protocol.DocumentParams{Session: "alice", Workspace: "ws", Path: "a.ts"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	got := protocol.AsError(err)
	if got.Code != protocol.ErrServerCrashed {
		t.Errorf("code = %s, want %s", got.Code, protocol.ErrServerCrashed)
	}
	if got.RetryAfterMS != 250 {
		t.Errorf("retry_after_ms = %d, want 250", got.RetryAfterMS)
	}
	if got.Message != "typescript died" {
		t.Errorf("message = %q", got.Message)
	}
}

// TestClientCancellationIsStructured: a cancelled context must not surface as a
// bare context error across the boundary.
func TestClientCancellationIsStructured(t *testing.T) {
	client := newScriptedPeer(t, func(*scriptedPeer, *protocol.Request) {
		// Never answer.
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.IndexStatus(ctx, protocol.IndexStatusParams{Session: "alice", Workspace: "ws"})
	if err == nil {
		t.Fatal("expected an error")
	}
	got := protocol.AsError(err)
	if got.Code != protocol.ErrNotReady {
		t.Errorf("code = %s, want %s", got.Code, protocol.ErrNotReady)
	}
	if got.RetryAfterMS == 0 {
		t.Error("a timed-out call carried no retry hint")
	}
}

// TestClientCloseIsIdempotent: EndSession-on-disconnect calls it, and so does
// the caller.
func TestClientCloseIsIdempotent(t *testing.T) {
	client := newScriptedPeer(t, func(*scriptedPeer, *protocol.Request) {})
	for i := 0; i < 3; i++ {
		if err := client.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Error("Done was never closed")
	}
}

// TestClientImplementsService is the seam mcp/ programs against.
func TestClientImplementsService(t *testing.T) {
	var _ protocol.Service = (*Client)(nil)
}
