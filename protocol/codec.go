package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// MaxFrameBytes bounds one newline-delimited JSON frame. A confused peer must
// not be able to make the daemon buffer without limit.
const MaxFrameBytes = 8 << 20 // 8 MiB

// maxBlankFrames bounds how many blank lines a peer may send in a row before
// they stop counting as keepalives and become a protocol failure. It sits far
// above anything a keepalive scheme could emit between two real messages — the
// daemon sunsets after 30 idle minutes, so 65536 of them cannot happen by
// accident — and far below anything that costs more than a moment to read.
const maxBlankFrames = 1 << 16

// ErrFrameTooLarge marks a frame this codec refuses because it exceeds
// MaxFrameBytes.
//
// It is matchable with errors.Is so a caller can turn the refusal into an
// ANSWER. A response the peer never receives leaves it waiting out its own
// deadline and then being told to retry something that will fail identically
// every time; the daemon knows exactly what went wrong and must say so.
var ErrFrameTooLarge = errors.New("frame exceeds the IPC frame limit")

// frameTooLargeError is the structured refusal, tagged so errors.Is finds
// ErrFrameTooLarge while AsError still recovers the SPEC §3.6 *Error.
type frameTooLargeError struct{ err *Error }

func (e *frameTooLargeError) Error() string        { return e.err.Error() }
func (e *frameTooLargeError) Unwrap() error        { return e.err }
func (e *frameTooLargeError) Is(target error) bool { return target == ErrFrameTooLarge }

func errFrameTooLarge() error {
	return &frameTooLargeError{err: NewErrorf(ErrInternal, "frame exceeds the %d byte limit", MaxFrameBytes)}
}

// Codec frames Requests and Responses as newline-delimited JSON over a socket
// (SPEC §3.5).
//
// Reads are called ONLY from a connection's single reader goroutine. Writes are
// safe for concurrent use: the implementation serialises them internally so two
// request handlers can never interleave frames on the wire.
type Codec struct {
	rw io.ReadWriteCloser
	br *bufio.Reader

	mu           sync.Mutex // guards writes only
	writeTimeout time.Duration
}

// deadliner is the part of net.Conn that lets a write be bounded in time.
type deadliner interface{ SetWriteDeadline(time.Time) error }

// NewCodec wraps a connection.
func NewCodec(rw io.ReadWriteCloser) *Codec {
	return &Codec{rw: rw, br: bufio.NewReaderSize(rw, 64<<10)}
}

// SetWriteTimeout bounds how long one frame write may block, when the
// underlying connection supports deadlines. Zero — the default — means no
// bound.
//
// A peer that stops reading otherwise parks the writer for ever: the socket
// buffer fills, Write never returns, and the goroutine holding it never gets to
// release the capacity it owns on everybody else's behalf. Nothing on the far
// side of a local socket can legitimately take seconds to accept one frame, so
// a timeout here only fires for a peer that has stopped participating.
func (c *Codec) SetWriteTimeout(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeTimeout = d
}

// ReadRequest reads one request frame.
//
// io.EOF is returned unwrapped so a connection loop can tell "the peer hung up"
// from "the peer sent nonsense"; every other failure is a structured SPEC §3.6
// error.
func (c *Codec) ReadRequest() (*Request, error) {
	line, err := c.readFrame()
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, NewErrorf(ErrInternal, "malformed request frame: %v", err)
	}
	return &req, nil
}

// ReadResponse reads one response frame.
func (c *Codec) ReadResponse() (*Response, error) {
	line, err := c.readFrame()
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, NewErrorf(ErrInternal, "malformed response frame: %v", err)
	}
	return &resp, nil
}

// readFrame returns one NON-EMPTY line without its terminator, refusing
// anything over MaxFrameBytes.
//
// The blank-line skip is a LOOP, deliberately, and never a tail call. Recursing
// once per blank line let a peer that sends nothing but newlines drive this
// function into itself until the goroutine stack was exhausted — and a stack
// overflow is a runtime throw, not a panic: recover cannot catch it, the
// per-connection goroutine cannot be isolated, and the WHOLE daemon process
// dies, taking every other client's language servers and warm state with it.
// A few megabytes of '\n' was enough.
func (c *Codec) readFrame() ([]byte, error) {
	blanks := 0
	for {
		frame, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if len(frame) > 0 {
			return frame, nil
		}
		// A blank line is a keepalive, not a message. Skip it — but not for
		// ever: past the bound this is a peer generating work, not a peer
		// staying alive.
		blanks++
		if blanks > maxBlankFrames {
			return nil, NewErrorf(ErrInternal,
				"peer sent %d blank frames in a row without a message", blanks)
		}
	}
}

// readLine reads up to and including the next newline and returns it trimmed of
// its terminator, so a blank line comes back as an empty slice.
func (c *Codec) readLine() ([]byte, error) {
	var frame []byte
	for {
		chunk, err := c.br.ReadSlice('\n')
		if len(frame)+len(chunk) > MaxFrameBytes {
			return nil, errFrameTooLarge()
		}
		frame = append(frame, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // a long line: keep accumulating up to the cap
		}
		if errors.Is(err, io.EOF) && len(frame) == 0 {
			return nil, io.EOF
		}
		if errors.Is(err, io.EOF) {
			// A final frame with no newline. Accept it rather than discarding a
			// complete message because the peer closed promptly.
			break
		}
		return nil, err
	}

	// Trim the terminator and tolerate CRLF.
	for len(frame) > 0 && (frame[len(frame)-1] == '\n' || frame[len(frame)-1] == '\r') {
		frame = frame[:len(frame)-1]
	}
	return frame, nil
}

// WriteRequest writes one request frame. Safe for concurrent use.
func (c *Codec) WriteRequest(r *Request) error { return c.writeJSON(r) }

// WriteResponse writes one response frame. Safe for concurrent use.
func (c *Codec) WriteResponse(r *Response) error { return c.writeJSON(r) }

func (c *Codec) writeJSON(v any) error {
	// Marshal outside the lock: an expensive result must not stall the other
	// handlers waiting to write.
	raw, err := json.Marshal(v)
	if err != nil {
		return NewErrorf(ErrInternal, "encoding frame: %v", err)
	}
	if len(raw)+1 > MaxFrameBytes {
		return errFrameTooLarge()
	}
	raw = append(raw, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	if dl, ok := c.rw.(deadliner); ok && c.writeTimeout > 0 {
		_ = dl.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		defer func() { _ = dl.SetWriteDeadline(time.Time{}) }()
	}
	if _, err := c.rw.Write(raw); err != nil {
		return err
	}
	return nil
}

// Close closes the underlying connection, which is what unblocks a reader
// parked in ReadRequest.
func (c *Codec) Close() error { return c.rw.Close() }

// CheckVersion compares a peer's declared protocol version against Version. A
// mismatch yields an INTERNAL error whose message names both versions; the
// caller turns that into the SPEC §3.1 drain-and-restart.
func CheckVersion(peer int) *Error {
	if peer == Version {
		return nil
	}
	return NewErrorf(ErrInternal,
		"protocol version mismatch: peer speaks %d, this binary speaks %d", peer, Version)
}
