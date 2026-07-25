package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// MaxFrameBytes bounds one newline-delimited JSON frame. A confused peer must
// not be able to make the daemon buffer without limit.
const MaxFrameBytes = 8 << 20 // 8 MiB

// Codec frames Requests and Responses as newline-delimited JSON over a socket
// (SPEC §3.5).
//
// Reads are called ONLY from a connection's single reader goroutine. Writes are
// safe for concurrent use: the implementation serialises them internally so two
// request handlers can never interleave frames on the wire.
type Codec struct {
	rw io.ReadWriteCloser
	br *bufio.Reader

	mu sync.Mutex // guards writes only
}

// NewCodec wraps a connection.
func NewCodec(rw io.ReadWriteCloser) *Codec {
	return &Codec{rw: rw, br: bufio.NewReaderSize(rw, 64<<10)}
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

// readFrame returns one line without its terminator, refusing anything over
// MaxFrameBytes.
func (c *Codec) readFrame() ([]byte, error) {
	var frame []byte
	for {
		chunk, err := c.br.ReadSlice('\n')
		if len(frame)+len(chunk) > MaxFrameBytes {
			return nil, NewErrorf(ErrInternal, "frame exceeds the %d byte limit", MaxFrameBytes)
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
	if len(frame) == 0 {
		// A blank line is a keepalive, not a message. Read the next frame.
		return c.readFrame()
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
		return NewErrorf(ErrInternal, "frame exceeds the %d byte limit", MaxFrameBytes)
	}
	raw = append(raw, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
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
