package xpc

import (
	"errors"
	"sync/atomic"
)

// A Message is a message a peer sent on its own initiative, passed to the
// function set with [WithMessageHandler]. It is valid only until that
// function returns.
type Message struct {
	d         atomic.Pointer[decoder] // nil once the handler returned
	off, next int                     // where the message's dictionary starts
}

// Decode stores the message in the value v points to. It may be called
// more than once.
func (m *Message) Decode(v any) error {
	d := m.d.Load()
	if d == nil {
		return errors.New("xpc: Message.Decode called after the handler returned")
	}
	d.off, d.next = m.off, m.next
	return d.decodeMessage(v)
}
