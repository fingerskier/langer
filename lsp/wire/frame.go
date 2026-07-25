package wire

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// maxFrameBytes bounds a single LSP message. A corrupt or hostile
// Content-Length must not let a server allocate unbounded memory in the bridge.
const maxFrameBytes = 32 << 20 // 32 MiB

// headerLimit bounds the header block of one frame.
const headerLimit = 64

// Framer reads and writes LSP's Content-Length framed JSON-RPC 2.0 messages.
//
// Read is called only from a connection's single reader goroutine. Write
// serialises internally, so N request goroutines can share one Framer without
// interleaving bytes on the wire.
type Framer struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
}

// NewFramer wraps rw.
func NewFramer(rw io.ReadWriter) *Framer {
	return &Framer{r: bufio.NewReaderSize(rw, 64<<10), w: rw}
}

// Read returns the next message. It reports io.EOF exactly when the stream ends
// cleanly between frames — the caller treats that as "the server exited".
func (f *Framer) Read() (Message, error) {
	length, err := f.readHeaders()
	if err != nil {
		return Message{}, err
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(f.r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Message{}, fmt.Errorf("lsp frame truncated after %d header bytes: %w", length, io.ErrUnexpectedEOF)
		}
		return Message{}, fmt.Errorf("reading lsp frame body: %w", err)
	}

	var m Message
	if err := m.UnmarshalJSON(body); err != nil {
		return Message{}, fmt.Errorf("decoding lsp frame: %w", err)
	}
	return m, nil
}

// readHeaders consumes one header block and returns the Content-Length.
func (f *Framer) readHeaders() (int, error) {
	length := -1
	for i := 0; ; i++ {
		if i > headerLimit {
			return 0, fmt.Errorf("lsp frame has more than %d headers", headerLimit)
		}
		line, err := f.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" && length < 0 && i == 0 {
				return 0, io.EOF
			}
			if errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("lsp stream ended mid-header: %w", io.ErrUnexpectedEOF)
			}
			return 0, fmt.Errorf("reading lsp header: %w", err)
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of the header block
		}

		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return 0, fmt.Errorf("malformed lsp header %q", line)
		}
		// Header names are case-insensitive; servers do vary.
		if !strings.EqualFold(strings.TrimSpace(name), "content-length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("malformed Content-Length %q: %w", value, err)
		}
		length = n
	}

	switch {
	case length < 0:
		return 0, errors.New("lsp frame has no Content-Length header")
	case length > maxFrameBytes:
		return 0, fmt.Errorf("lsp frame of %d bytes exceeds the %d byte limit", length, maxFrameBytes)
	}
	return length, nil
}

// Write frames and sends m. It is safe for concurrent use.
func (f *Framer) Write(m Message) error {
	body, err := m.MarshalJSON()
	if err != nil {
		return fmt.Errorf("encoding lsp frame: %w", err)
	}

	var buf strings.Builder
	buf.WriteString("Content-Length: ")
	buf.WriteString(strconv.Itoa(len(body)))
	buf.WriteString("\r\n\r\n")

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := io.WriteString(f.w, buf.String()); err != nil {
		return fmt.Errorf("writing lsp header: %w", err)
	}
	if _, err := f.w.Write(body); err != nil {
		return fmt.Errorf("writing lsp body: %w", err)
	}
	return nil
}
