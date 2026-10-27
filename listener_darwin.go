//go:build darwin && cgo

package xpc

/*
#include "mach_darwin.h"
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"unsafe"
)

// machCheckinID is the id of the message a peer connects with, "w00t".
const machCheckinID = 0x77303074

// errListenerClosed reports an endpoint of a closed listener.
var errListenerClosed = errors.New("xpc: listener closed")

// A Listener is an anonymous XPC service in this process, as libxpc's
// xpc_connection_create(NULL, NULL) makes: it has no launchd name, and
// other processes reach it through its [Endpoint], which a message carries
// to them. A service can then send events back on a connection of their
// own, such as the progress of a long request.
//
// The messages peers send go to the handler set with [WithMessageHandler],
// one at a time; errors, such as messages rejected by
// [WithPeerRequirement], go to [WithErrorHandler]. A Listener receives
// one-way messages: a peer's request that waits for a reply gets none, and
// fails as if the connection was interrupted.
type Listener struct {
	opts options
	port uint32 // the receive right peers check in on
	set  uint32 // the port set of port and the peers' ports

	mu     sync.Mutex
	peers  map[uint32]uint32 // a peer's messages port -> our send right to the peer
	closed bool
	done   chan struct{}
}

// Listen creates a listener. The options that apply are
// [WithMessageHandler], [WithErrorHandler], [WithPeerRequirement] and
// [WithMaxMessageSize].
func Listen(opts ...Option) (*Listener, error) {
	l := &Listener{peers: make(map[uint32]uint32), done: make(chan struct{})}
	for _, opt := range opts {
		opt(&l.opts)
	}
	var port, set C.mach_port_t
	if kr := C.goxpc_listen(&port, &set); kr != 0 {
		return nil, machError("mach_port_allocate", kr)
	}
	l.port, l.set = uint32(port), uint32(set)
	go l.receive()
	return l, nil
}

// Endpoint returns the listener's endpoint, to send in a message to the
// process that should connect to the listener. Once the listener is
// closed, its endpoint no longer encodes.
func (l *Listener) Endpoint() Endpoint {
	return Endpoint{l: l}
}

// makeSend makes a send right to the listener's port, for a message to
// move to a peer.
func (l *Listener) makeSend() (uint32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, errListenerClosed
	}
	if kr := C.goxpc_make_send(C.mach_port_t(l.port)); kr != 0 {
		return 0, machError("mach_port_insert_right", kr)
	}
	return l.port, nil
}

// Close stops the listener: the peers' connections become invalid, and
// its endpoints lead nowhere. Close does not wait for the handler to
// return; use [Listener.Done] for that.
func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	l.closed = true
	// The receive loop shuts down. The wake-up does not wait for room in
	// the port's queue: the loop may be waiting for l.mu.
	C.goxpc_wake(C.mach_port_t(l.port), machQuitID)
	return nil
}

// Done returns a channel that is closed once the listener has stopped,
// after [Listener.Close], and its handler is no longer called.
func (l *Listener) Done() <-chan struct{} { return l.done }

// receive receives check-ins, the peers' messages and the notifications
// that peers went away, on the port set, until Close.
func (l *Listener) receive() {
	m := getMsg()
	defer m.free()
	defer l.shutdown()
	self := os.Getpid()
	for {
		m.limit = l.opts.maxMessage()
		buf := m.buffer()
		kr := C.goxpc_receive(unsafe.Pointer(&buf[0]), C.mach_msg_size_t(len(buf)), C.mach_port_t(l.set))
		if kr == C.MACH_RCV_TOO_LARGE {
			if m.needed() > m.limit+trailerSpace {
				// Dropped from the member port it is queued on, which the
				// header names: the set's next message may be another's.
				C.goxpc_discard(C.mach_port_t(m.local()))
				l.opts.report(ErrMessageTooLarge)
				continue
			}
			m.grow()
			continue
		}
		if kr != 0 {
			return
		}
		m.received = true
		// As with a Conn, notifications are only taken from the kernel and
		// the quit message from this process.
		switch local, id := m.local(), m.id(); {
		case local == l.port && id == machQuitID:
			if p, ok := m.sender(); ok && p.PID == self {
				m.releaseReceived()
				return
			}
		case local == l.port && id == machCheckinID:
			l.accept(m)
		case local == l.port:
			// Only check-ins come to the listener's own port.
		case id == C.MACH_NOTIFY_NO_SENDERS:
			if m.fromKernel() {
				l.drop(local)
			}
		case id == machMessageID:
			l.mu.Lock()
			_, ok := l.peers[local]
			l.mu.Unlock()
			if ok {
				if err := l.opts.deliver(m); err != nil {
					l.opts.report(err)
				}
			}
		}
		m.releaseReceived()
		m.dec.release()
	}
}

// accept takes in the connection the check-in received in m opens: the
// receive right of the peer's messages, and a send right to the peer. A
// check-in without them is dropped, and its rights released.
func (l *Listener) accept(m *machMsg) {
	if err := l.opts.checkPeer(m); err != nil {
		l.opts.report(err)
		return
	}
	b := m.buffer()
	size := m.size()
	if binary.LittleEndian.Uint32(b)&C.MACH_MSGH_BITS_COMPLEX == 0 || size < headerSize+4 {
		return
	}
	n := int(binary.LittleEndian.Uint32(b[headerSize:]))
	var rcv, snd []byte // the descriptors of the two rights
	for i, at := 0, headerSize+4; i < n && at+portDescSize <= size; i, at = i+1, at+portDescSize {
		d := b[at : at+portDescSize]
		if d[11] != C.MACH_MSG_PORT_DESCRIPTOR {
			break // the rest cannot be laid out as port descriptors
		}
		switch {
		case d[10] == C.MACH_MSG_TYPE_PORT_RECEIVE && rcv == nil:
			rcv = d
		case d[10] == C.MACH_MSG_TYPE_PORT_SEND && snd == nil:
			snd = d
		}
	}
	if rcv == nil || snd == nil {
		return
	}
	in, out := binary.LittleEndian.Uint32(rcv), binary.LittleEndian.Uint32(snd)
	// The listener holds them now: releasing the message leaves them be.
	binary.LittleEndian.PutUint32(rcv, 0)
	binary.LittleEndian.PutUint32(snd, 0)
	if kr := C.goxpc_accept(C.mach_port_t(in), C.mach_port_t(l.set)); kr != 0 {
		C.goxpc_port_destroy(C.mach_port_t(in))
		C.goxpc_port_release(C.mach_port_t(out))
		l.opts.report(machError("mach_port_insert_member", kr))
		return
	}
	l.mu.Lock()
	l.peers[in] = out
	l.mu.Unlock()
}

// drop releases the connection of the peer whose messages arrive on port,
// once it holds no send right to it: the peer has gone.
func (l *Listener) drop(port uint32) {
	l.mu.Lock()
	out, ok := l.peers[port]
	delete(l.peers, port)
	l.mu.Unlock()
	if ok {
		C.goxpc_port_destroy(C.mach_port_t(port)) // also leaves the set
		C.goxpc_port_release(C.mach_port_t(out))
	}
}

// shutdown releases every port of the stopped listener.
func (l *Listener) shutdown() {
	l.mu.Lock()
	l.closed = true
	peers := l.peers
	l.peers = nil
	l.mu.Unlock()
	for in, out := range peers {
		C.goxpc_port_destroy(C.mach_port_t(in))
		C.goxpc_port_release(C.mach_port_t(out))
	}
	C.goxpc_port_destroy(C.mach_port_t(l.port))
	C.goxpc_set_destroy(C.mach_port_t(l.set))
	close(l.done)
}

// dialEndpoint returns a connection to the listener ep names, in this
// process. It is for the tests.
func dialEndpoint(ep Endpoint, opts ...Option) *Conn {
	c := &Conn{name: "endpoint", endpoint: ep.l, pending: make(map[uint32]struct{}), done: make(chan struct{})}
	for _, opt := range opts {
		opt(&c.opts)
	}
	return c
}
