//go:build darwin && cgo

package xpc

/*
#include <stdlib.h>
#include "mach_darwin.h"
*/
import "C"

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Conn is a connection to an XPC service. It is safe for concurrent use.
type Conn struct {
	opts     options
	name     string    // the launchd service
	endpoint *Listener // or the listener in this process, in tests

	mu        sync.Mutex
	sess      *session // current check-in, nil before or after one
	cli       uint32   // our receive right for the server's messages
	pending   map[uint32]struct{}
	receiving bool // the receive loop is running
	cancelled bool
	err       error

	done chan struct{}
}

// A session is one check-in with the service. Its send right is released
// once the session has ended and no send is using it, so that its name is
// not reused under a send in flight.
type session struct {
	srv  uint32
	refs int // sends in flight, plus one while current
}

// Dial connects to the service registered with launchd under name.
//
// The connection is established lazily: if no such service exists, Dial
// still succeeds and the first message fails with [ErrConnectionInvalid].
func Dial(name string, opts ...Option) (*Conn, error) {
	// launchd names hold at most 127 bytes; a longer one, or one with a
	// NUL, would be cut short and name another service.
	if name == "" || len(name) > 127 || strings.IndexByte(name, 0) >= 0 {
		return nil, fmt.Errorf("xpc: invalid service name %q", name)
	}
	c := &Conn{name: name, pending: make(map[uint32]struct{}), done: make(chan struct{})}
	for _, opt := range opts {
		opt(&c.opts)
	}
	return c, nil
}

// acquire returns the current session, checking in first if needed. A
// reply port rp, unless 0, is registered as pending under the same lock.
func (c *Conn) acquire(rp uint32) (*session, error) {
	c.mu.Lock()
	if err := c.usableLocked(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	lost := false
	if s := c.sess; s != nil && C.goxpc_port_dead(C.mach_port_t(s.srv)) != 0 {
		// The service went away, and the dead-name notification is on its
		// way: end the session now, so that this message checks in again
		// rather than fail on the dead port.
		c.endLocked(s)
		lost = true
	}
	if c.sess == nil {
		if err := c.checkinLocked(); err != nil {
			first := c.invalidateLocked(err)
			c.mu.Unlock()
			if lost {
				c.reportError(c.lost())
			}
			if first {
				c.reportError(err)
			}
			return nil, err
		}
	}
	s := c.sess
	s.refs++
	if rp != 0 {
		c.pending[rp] = struct{}{}
	}
	c.mu.Unlock()
	if lost {
		c.reportError(c.lost())
	}
	return s, nil
}

// release drops the reference to s that acquire took and, unless 0,
// forgets the reply port rp, under one lock. It reports whether rp was
// still pending, and not destroyed by abort or Close.
func (c *Conn) release(s *session, rp uint32) bool {
	c.mu.Lock()
	s.refs--
	if s.refs == 0 {
		C.goxpc_port_release(C.mach_port_t(s.srv))
	}
	_, pending := c.pending[rp]
	delete(c.pending, rp)
	c.mu.Unlock()
	return pending
}

// checkinLocked looks the service up and checks in with it, starting the
// receive loop on the first check-in. c.mu is held.
func (c *Conn) checkinLocked() error {
	var service C.mach_port_t
	if c.endpoint != nil {
		p, err := c.endpoint.makeSend()
		if err != nil {
			return &Error{Err: ErrConnectionInvalid, Description: "listener closed"}
		}
		service = C.mach_port_t(p)
	} else {
		cname := C.CString(c.name)
		kr := C.goxpc_lookup(cname, boolInt(c.opts.privileged), &service)
		C.free(unsafe.Pointer(cname))
		if kr != 0 {
			return &Error{Err: ErrConnectionInvalid, Description: "service not found"}
		}
	}
	defer C.goxpc_port_release(service)
	var srv C.mach_port_t
	cli := C.mach_port_t(c.cli)
	if kr := C.goxpc_checkin(service, &srv, &cli); kr != 0 {
		return &Error{Err: ErrConnectionInvalid, Description: "check-in failed"}
	}
	c.sess = &session{srv: uint32(srv), refs: 1}
	c.cli = uint32(cli)
	if !c.receiving {
		c.receiving = true
		go c.receive(c.cli)
	}
	return nil
}

func boolInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// lost reports the error for a session that ended because the service
// went away. launchd relaunches the service on the next message.
func (c *Conn) lost() error {
	return &Error{Err: ErrConnectionInterrupted}
}

// deadNameHook, if set, runs before the receive loop handles a dead-name
// notification. It is for the tests.
var deadNameHook atomic.Pointer[func()]

// interrupt ends the session s, if it is still current, because the
// service went away.
func (c *Conn) interrupt(s *session) {
	c.mu.Lock()
	if c.sess != s || c.cancelled {
		c.mu.Unlock()
		return
	}
	c.endLocked(s)
	c.mu.Unlock()
	c.reportError(c.lost())
}

// endLocked ends the current session s. c.mu is held.
func (c *Conn) endLocked(s *session) {
	c.sess = nil
	s.refs--
	if s.refs == 0 {
		C.goxpc_port_release(C.mach_port_t(s.srv))
	}
}

// Send sends a one-way message to the service. msg is a struct, a map with
// string keys or a pointer to one.
func (c *Conn) Send(msg any) error {
	m := getMsg()
	defer m.free()
	if err := m.enc.messageValue(msg); err != nil {
		return err
	}
	return c.send(m)
}

// send sends the one-way message encoded in m in the current session,
// checking in first if needed.
func (c *Conn) send(m *machMsg) error {
	s, err := c.acquire(0)
	if err != nil {
		return err
	}
	size, err := m.finish(s.srv, 0, machMessageID)
	if err != nil {
		c.release(s, 0)
		return err
	}
	kr := C.goxpc_send(unsafe.Pointer(&m.buffer()[0]), C.mach_msg_size_t(size))
	c.release(s, 0)
	if kr != 0 {
		m.releaseSent()
		if kr == C.MACH_SEND_INVALID_DEST {
			c.interrupt(s)
			return c.lost()
		}
		return machError("mach_msg", kr)
	}
	m.sent()
	return nil
}

// Call sends req to the service and decodes its reply into reply, a
// pointer; a nil reply discards the reply. req is a struct, a map with
// string keys or a pointer to one. Reply entries that match no field are
// ignored. If ctx is done before the reply arrives, Call returns ctx.Err().
func (c *Conn) Call(ctx context.Context, req, reply any) error {
	m := getMsg()
	defer m.free()
	if err := m.enc.messageValue(req); err != nil {
		return err
	}
	if err := c.roundTrip(ctx, m); err != nil {
		return err
	}
	d, err := m.decoder()
	if err != nil {
		return err
	}
	return d.decodeMessage(reply)
}

// prepare is the requester method of the net/rpc codec: it encodes the
// request at once and sends it when the function it returns is called.
func (c *Conn) prepare(p *RPCProtocol, method string, args any) (func(context.Context) (rpcResult, error), error) {
	m := getMsg()
	if err := m.enc.call(args, p.MethodKey, method); err != nil {
		m.free()
		return nil, err
	}
	return func(ctx context.Context) (rpcResult, error) {
		if err := c.roundTrip(ctx, m); err != nil {
			m.free()
			return nil, err
		}
		d, err := m.decoder()
		if err != nil {
			m.free()
			return nil, err
		}
		return newWireResult(d, p, m.free), nil
	}, nil
}

// roundTrip sends the message encoded in m and receives the reply into m.
// Sending and receiving take a single system call, on the calling
// goroutine.
func (c *Conn) roundTrip(ctx context.Context, m *machMsg) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rp := getReplyPort()
	s, err := c.acquire(rp)
	if err != nil {
		putReplyPort(rp)
		return err
	}
	size, err := m.finish(s.srv, rp, machMessageID)
	if err != nil {
		if c.release(s, rp) {
			putReplyPort(rp)
		}
		return err
	}
	m.limit = c.opts.maxMessage()
	var stop func() bool
	if ctx.Done() != nil {
		// The only way to stop a receive in progress is to destroy the
		// port it waits on.
		stop = context.AfterFunc(ctx, func() { c.abort(rp) })
	}
	buf := m.buffer()
	kr := C.goxpc_send_receive(unsafe.Pointer(&buf[0]), C.mach_msg_size_t(size), C.mach_msg_size_t(len(buf)), C.mach_port_t(rp))
	tooLarge := false
	for kr == C.MACH_RCV_TOO_LARGE {
		m.sent()
		if m.needed() > m.limit+trailerSpace {
			// Rather than make room for it, drop the reply.
			kr, tooLarge = C.goxpc_discard(C.mach_port_t(rp)), true
			break
		}
		m.grow()
		buf = m.buffer()
		kr = C.goxpc_receive(unsafe.Pointer(&buf[0]), C.mach_msg_size_t(len(buf)), C.mach_port_t(rp))
	}
	if stop != nil {
		stop()
	}
	pending := c.release(s, rp)
	if tooLarge && kr == C.MACH_RCV_TOO_LARGE {
		// Dropped: the port holds nothing anymore.
		if pending {
			putReplyPort(rp)
		}
		return ErrMessageTooLarge
	}

	if kr != 0 && kr < C.MACH_RCV_IN_PROGRESS {
		// The send failed; nothing was received.
		m.releaseSent()
		if pending {
			if kr == C.MACH_SEND_INVALID_DEST {
				// Rejected before the reply right was made: reusable.
				putReplyPort(rp)
			} else {
				// A reply right may have been made and destroyed, and its
				// notification be on its way to rp.
				C.goxpc_port_destroy(C.mach_port_t(rp))
			}
		}
		switch {
		case kr == C.MACH_SEND_INVALID_DEST:
			c.interrupt(s)
			return c.lost()
		case !pending && ctx.Err() != nil:
			return ctx.Err()
		case !pending:
			return &Error{Err: ErrConnectionInvalid} // closed
		}
		return machError("mach_msg", kr)
	}
	m.sent()
	if kr != 0 {
		// The reply port was destroyed: by ctx, or by Close.
		if pending {
			C.goxpc_port_destroy(C.mach_port_t(rp))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return &Error{Err: ErrConnectionInvalid}
	}
	// If ctx was done just as the reply arrived, the reply still counts;
	// the port was destroyed, and is not reused, if abort got to it.
	m.received = true
	if pending {
		putReplyPort(rp)
	}
	switch m.id() {
	case machReplyID:
		if err := c.opts.checkPeer(m); err != nil {
			return err
		}
		return m.parse()
	case C.MACH_NOTIFY_SEND_ONCE:
		if m.fromKernel() {
			// The service went away without replying.
			return c.lost()
		}
	}
	return errMalformed
}

// checkPeer applies the peer requirement, if any, to the sender of the
// message received in m.
func (o *options) checkPeer(m *machMsg) error {
	fn := o.peerRequirement
	if fn == nil {
		return nil
	}
	p, ok := m.sender()
	if !ok {
		return errors.New("xpc: message without sender identity")
	}
	return fn(p)
}

// abort destroys the reply port of a pending request, which makes its
// receive fail.
func (c *Conn) abort(rp uint32) {
	c.mu.Lock()
	_, ok := c.pending[rp]
	delete(c.pending, rp)
	c.mu.Unlock()
	if ok {
		C.goxpc_port_destroy(C.mach_port_t(rp))
	}
}

// receive receives the service's messages and the notifications about its
// end, on the client port, until Close.
func (c *Conn) receive(cli uint32) {
	m := getMsg()
	defer m.free()
	self := os.Getpid()
	for {
		m.limit = c.opts.maxMessage()
		buf := m.buffer()
		kr := C.goxpc_receive(unsafe.Pointer(&buf[0]), C.mach_msg_size_t(len(buf)), C.mach_port_t(cli))
		if kr == C.MACH_RCV_TOO_LARGE {
			if m.needed() > m.limit+trailerSpace {
				C.goxpc_discard(C.mach_port_t(cli))
				c.reportError(ErrMessageTooLarge)
				continue
			}
			m.grow()
			continue
		}
		if kr != 0 {
			break
		}
		m.received = true
		// Notifications and the quit message are only taken from the
		// kernel and from this process: any process the service gave a
		// send right to could forge them.
		switch id := m.id(); {
		case id == machQuitID:
			if p, ok := m.sender(); ok && p.PID == self {
				m.releaseReceived()
				C.goxpc_port_destroy(C.mach_port_t(cli))
				c.invalidate(&Error{Err: ErrConnectionInvalid})
				return
			}
		case id == C.MACH_NOTIFY_DEAD_NAME:
			if !m.fromKernel() || m.size() < deadNameSize {
				break
			}
			if h := deadNameHook.Load(); h != nil {
				(*h)()
			}
			// The name is the session's send right; the notification
			// carries a reference to it.
			name := binary.LittleEndian.Uint32(buf[headerSize+8:])
			C.goxpc_port_release(C.mach_port_t(name))
			c.mu.Lock()
			s := c.sess
			c.mu.Unlock()
			if s != nil && s.srv == name {
				c.interrupt(s)
			}
		case id == machMessageID:
			c.deliver(m)
		}
		m.releaseReceived()
		m.dec.release()
	}
}

// deliver passes the message received in m to the message handler.
func (c *Conn) deliver(m *machMsg) {
	if err := c.opts.deliver(m); err != nil {
		c.reportError(err)
	}
}

// deliver passes the message received in m to the message handler, if
// any, and returns the error to report instead, if it cannot.
func (o *options) deliver(m *machMsg) error {
	fn := o.messageHandler
	if fn == nil {
		return nil
	}
	if err := o.checkPeer(m); err != nil {
		return err
	}
	if err := m.parse(); err != nil {
		return err
	}
	d, err := m.decoder()
	if err != nil {
		return err
	}
	msg := &Message{off: d.off, next: d.next}
	msg.d.Store(d)
	fn(msg)
	msg.d.Store(nil)
	return nil
}

func (c *Conn) reportError(err error) { c.opts.report(err) }

// report passes err to the error handler, if any.
func (o *options) report(err error) {
	if fn := o.errorHandler; fn != nil {
		fn(err)
	}
}

// Close cancels the connection. Pending requests fail with
// [ErrConnectionInvalid]. Close does not wait for the cancellation to
// complete; use [Conn.Done] for that.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.cancelled {
		c.mu.Unlock()
		return ErrClosed
	}
	c.cancelled = true
	if s := c.sess; s != nil {
		// The server sees the connection end once no send uses it.
		c.sess = nil
		s.refs--
		if s.refs == 0 {
			C.goxpc_port_release(C.mach_port_t(s.srv))
		}
	}
	for rp := range c.pending {
		delete(c.pending, rp)
		C.goxpc_port_destroy(C.mach_port_t(rp))
	}
	if c.receiving {
		c.wakeLocked() // the receive loop finishes the cancellation
		c.mu.Unlock()
		return nil
	}
	err := &Error{Err: ErrConnectionInvalid}
	first := c.invalidateLocked(err)
	c.mu.Unlock()
	if first {
		c.reportError(err)
	}
	return nil
}

// wakeLocked stops the receive loop, which owns the client port.
func (c *Conn) wakeLocked() {
	if c.receiving {
		C.goxpc_wake(C.mach_port_t(c.cli), machQuitID)
	}
}

// invalidate records that the connection can no longer be used and tells
// the error handler.
func (c *Conn) invalidate(err error) {
	c.mu.Lock()
	first := c.invalidateLocked(err)
	c.mu.Unlock()
	if first {
		c.reportError(err)
	}
}

func (c *Conn) invalidateLocked(err error) bool {
	if c.err != nil {
		return false
	}
	c.err = err
	close(c.done)
	return true
}

// Done returns a channel that is closed once the connection has become
// invalid, either through [Conn.Close] or because the service could not be
// reached. No events are delivered after that.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns the error that invalidated the connection, or nil while it
// is usable.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) usableLocked() error {
	switch {
	case c.cancelled:
		return ErrClosed
	case c.err != nil:
		return c.err
	}
	return nil
}
