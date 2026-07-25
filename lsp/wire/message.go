// Package wire is the pure part of langer's LSP client: Content-Length
// framing, the union decoders real servers force on us, UTF-16 position
// conversion, and normalisation into protocol types.
//
// Nothing here starts a goroutine or does I/O beyond an injected io.ReadWriter,
// which is what makes the bug-dense half of the LSP client exhaustively
// table-testable. All LSP wire vocabulary — MarkupContent, LocationLink,
// SymbolInformation — stops at this package boundary.
package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Message is a decoded JSON-RPC 2.0 frame.
//
// ID is json.RawMessage because it is string-or-number on the wire and real
// servers send both; correlate strictly by its bytes, never by a parsed int.
type Message struct {
	JSONRPC string
	ID      json.RawMessage
	Method  string
	Params  json.RawMessage
	Result  json.RawMessage
	Error   *RPCError
}

// RPCError is the JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// IsRequest reports whether m expects a response: it names a method AND
// carries an id. A method with no id is a notification and must never be
// answered.
func (m Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification reports whether m is a method call with no id.
func (m Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse reports whether m answers a request we sent.
func (m Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// wireMessage is the on-the-wire shape. Decoding is permissive; encoding is
// exact, because the difference between an omitted result and a null result is
// the difference between a valid response and a protocol violation.
type wireMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  *json.RawMessage `json:"params,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// UnmarshalJSON decodes a frame.
func (m *Message) UnmarshalJSON(data []byte) error {
	var w wireMessage
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.JSONRPC = w.JSONRPC
	m.Method = w.Method
	m.Error = w.Error
	m.ID = derefRaw(w.ID)
	m.Params = derefRaw(w.Params)
	m.Result = derefRaw(w.Result)
	return nil
}

// MarshalJSON encodes a frame.
//
// Three shapes, and getting them wrong deadlocks a session:
//   - notification: method, optional params, NO id;
//   - request:      method, optional params, id;
//   - response:     id and exactly one of result (possibly null) or error.
func (m Message) MarshalJSON() ([]byte, error) {
	w := wireMessage{JSONRPC: "2.0", Method: m.Method, Error: m.Error}
	if m.JSONRPC != "" {
		w.JSONRPC = m.JSONRPC
	}
	if len(m.ID) > 0 {
		id := m.ID
		w.ID = &id
	}
	if len(m.Params) > 0 {
		params := m.Params
		w.Params = &params
	}

	if m.Method == "" && len(m.ID) > 0 && m.Error == nil {
		// A response. An omitted result key is not a valid JSON-RPC response,
		// so an empty Result must serialise as an explicit null.
		result := m.Result
		if len(result) == 0 {
			result = json.RawMessage("null")
		}
		w.Result = &result
	}
	return json.Marshal(w)
}

func derefRaw(p *json.RawMessage) json.RawMessage {
	if p == nil {
		return nil
	}
	return json.RawMessage(*p)
}

// isNullOrEmpty reports whether raw carries no usable value. Servers use
// absent, null and [] interchangeably for "nothing".
func isNullOrEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
