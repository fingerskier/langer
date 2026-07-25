package daemon

import (
	"encoding/json"
	"sync"

	"github.com/fingerskier/langer/protocol"
)

// sessionSet records the sessions seen on one connection.
//
// SPEC §4.2 requires that a session's state is dropped when it disconnects, and
// the daemon — which owns that state — can only observe a disconnect at the
// transport. So every connection remembers which sessions spoke over it and
// ends them when it closes, whether or not the client remembered to.
type sessionSet struct {
	mu  sync.Mutex
	ids map[protocol.SessionID]struct{}
}

func newSessionSet() *sessionSet {
	return &sessionSet{ids: map[protocol.SessionID]struct{}{}}
}

func (s *sessionSet) note(id protocol.SessionID) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[id] = struct{}{}
}

func (s *sessionSet) forget(id protocol.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, id)
}

func (s *sessionSet) all() []protocol.SessionID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.SessionID, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}

// sessionEnvelope is the one field every params struct shares. Peeking at it
// before dispatch lets the connection track sessions generically instead of
// switching on the method.
type sessionEnvelope struct {
	Session protocol.SessionID `json:"session_id"`
}

// sessionOf extracts the session id from raw params.
//
// A missing session id is INTERNAL, not a silent default: SPEC §4.2's overlay
// isolation and this daemon's cleanup-on-disconnect both key on it, and a
// client that forgets it would get another caller's state.
func sessionOf(method string, raw json.RawMessage) (protocol.SessionID, error) {
	var envelope sessionEnvelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "", protocol.NewErrorf(protocol.ErrInternal, "decoding %s params: %v", method, err)
		}
	}
	if envelope.Session == "" {
		return "", protocol.NewErrorf(protocol.ErrInternal, "%s: session_id is required", method)
	}
	return envelope.Session, nil
}
