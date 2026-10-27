//go:build darwin && cgo

package xpc

/*
#include "mach_darwin.h"
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"
	"unsafe"
)

// Mach message ids of the XPC protocol.
const (
	machMessageID = 0x10000000 // a message, from either side
	machReplyID   = 0x20000000 // a reply to a message that carried a reply port
	machQuitID    = 0x71756974 // "quit": stops a Conn's receive loop
)

const (
	headerSize   = 24 // mach_msg_header_t
	portDescSize = 12 // mach_msg_port_descriptor_t
	oolDescSize  = 16 // mach_msg_ool_descriptor64_t
	trailerSpace = 128
	auditTrailer = 52 // mach_msg_audit_trailer_t

	// deadNameSize is the size of a dead-name notification: the header,
	// an NDR record and the name.
	deadNameSize = headerSize + 8 + 4

	// Payloads are encoded after room for the header and this many port
	// descriptors, which are then written in front of the payload in
	// place: a payload is not moved or copied to be sent.
	reservedPorts = 4
	payloadBase   = headerSize + 4 + reservedPorts*portDescSize

	// Bodies larger than this are sent out of line, as libxpc does.
	maxInlineBody = 16384

	minBuffer       = 16 << 10
	maxPooledBuffer = 256 << 10
)

// A machMsg holds one message in both directions: the encoder lays a
// message out in enc.buf, starting at start, and a reply is received into
// the same buffer.
type machMsg struct {
	enc   encoder
	dec   decoder
	start int // offset of the Mach message in enc.buf

	// A message received into the buffer, and what parse found in it.
	// Everything it carries is released by releaseReceived.
	received bool
	body     []byte
	ports    []uint32 // the port descriptors' names, in order
	limit    int      // largest body or data accepted; 0 for no limit

	// What a message being sent attaches, released if sending fails.
	sentPorts []uint32
	sentBody  region
}

// A region is memory mapped into this task, out of line of a message.
type region struct {
	addr uintptr
	size int
}

var msgPool = sync.Pool{
	New: func() any {
		m := &machMsg{}
		m.enc.buf = make([]byte, 0, minBuffer)
		return m
	},
}

func getMsg() *machMsg {
	m := msgPool.Get().(*machMsg)
	m.enc.reset(payloadBase)
	m.start = payloadBase - headerSize
	return m
}

func (m *machMsg) free() {
	m.releaseReceived()
	m.limit = 0
	m.dec.release()
	clear(m.enc.attachments)
	m.enc.attachments = m.enc.attachments[:0]
	if cap(m.enc.buf) > maxPooledBuffer {
		return
	}
	msgPool.Put(m)
}

// finish completes the message encoded in m.enc for sending to remote with
// the given reply port (0 for none), creating its attachments, and returns
// its size.
func (m *machMsg) finish(remote, reply uint32, id uint32) (int, error) {
	payload := m.enc.buf[payloadBase:]
	atts := m.enc.attachments
	ool := len(payload) > maxInlineBody
	if len(atts) == 0 && !ool {
		m.start = payloadBase - headerSize
		size := len(m.enc.buf) - m.start
		m.setHeader(remote, reply, id, size, 0)
		return size, nil
	}

	// A complex message: descriptors go between the header and the
	// payload.
	descs := 4 + len(atts)*portDescSize
	if ool {
		descs += oolDescSize
	}
	var size int
	switch {
	case ool:
		// The body travels in pages of its own; the message is only the
		// header and descriptors.
		if uint64(len(payload)) > math.MaxUint32 {
			return 0, errTooLong
		}
		var addr C.uintptr_t
		if kr := C.goxpc_copy_pages(unsafe.Pointer(&payload[0]), C.size_t(len(payload)), &addr); kr != 0 {
			return 0, machError("vm_allocate", kr)
		}
		m.sentBody = region{uintptr(addr), len(payload)}
		m.start = 0
		size = headerSize + descs
	case descs <= payloadBase-headerSize:
		m.start = payloadBase - headerSize - descs
		size = len(m.enc.buf) - m.start
	default:
		// More descriptors than there is room for: move the payload.
		m.enc.buf = slices.Insert(m.enc.buf, payloadBase, make([]byte, descs-(payloadBase-headerSize))...)
		m.start = 0
		size = len(m.enc.buf)
	}
	b := m.enc.buf[m.start : m.start+headerSize+descs]
	n := len(atts)
	at := headerSize + 4
	if ool {
		n++
		binary.LittleEndian.PutUint64(b[at:], uint64(m.sentBody.addr))
		b[at+8] = 1  // deallocate: the pages move to the receiver
		b[at+9] = 1  // MACH_MSG_VIRTUAL_COPY
		b[at+10] = 0 // padding
		b[at+11] = 1 // MACH_MSG_OOL_DESCRIPTOR
		binary.LittleEndian.PutUint32(b[at+12:], uint32(m.sentBody.size))
		at += oolDescSize
	}
	binary.LittleEndian.PutUint32(b[headerSize:], uint32(n))
	for _, a := range atts {
		p, err := attach(a)
		if err != nil {
			m.releaseSent()
			return 0, err
		}
		m.sentPorts = append(m.sentPorts, p)
		binary.LittleEndian.PutUint32(b[at:], p)
		binary.LittleEndian.PutUint32(b[at+4:], 0) // padding
		b[at+8], b[at+9] = 0, 0
		b[at+10] = C.MACH_MSG_TYPE_MOVE_SEND
		b[at+11] = 0 // MACH_MSG_PORT_DESCRIPTOR
		at += portDescSize
	}
	m.setHeader(remote, reply, id, size, C.MACH_MSGH_BITS_COMPLEX)
	return size, nil
}

func (m *machMsg) setHeader(remote, reply, id uint32, size int, flags uint32) {
	bits := uint32(C.MACH_MSG_TYPE_COPY_SEND) | flags
	if reply != 0 {
		bits |= C.MACH_MSG_TYPE_MAKE_SEND_ONCE << 8
	}
	h := m.enc.buf[m.start : m.start+headerSize]
	binary.LittleEndian.PutUint32(h[0:], bits)
	binary.LittleEndian.PutUint32(h[4:], uint32(size))
	binary.LittleEndian.PutUint32(h[8:], remote)
	binary.LittleEndian.PutUint32(h[12:], reply)
	binary.LittleEndian.PutUint32(h[16:], 0)
	binary.LittleEndian.PutUint32(h[20:], id)
}

// attach creates the port carrying a.
func attach(a attachment) (uint32, error) {
	var p C.mach_port_t
	if l := a.endpoint; l != nil {
		return l.makeSend()
	}
	if f := a.file; f != nil {
		rc, err := f.SyscallConn()
		if err != nil {
			return 0, fmt.Errorf("xpc: %w", err)
		}
		var cerr error
		if err := rc.Control(func(fd uintptr) {
			if C.goxpc_fileport_make(C.int(fd), &p) != 0 {
				cerr = fmt.Errorf("xpc: cannot pass file descriptor of %s", f.Name())
			}
		}); err != nil {
			return 0, fmt.Errorf("xpc: %w", err)
		}
		return uint32(p), cerr
	}
	if kr := C.goxpc_memory_entry(unsafe.Pointer(&a.data[0]), C.size_t(len(a.data)), &p); kr != 0 {
		return 0, machError("mach_make_memory_entry", kr)
	}
	return uint32(p), nil
}

// sent records that the message was sent, so its attachments belong to
// the receiver now.
func (m *machMsg) sent() {
	m.sentPorts = m.sentPorts[:0]
	m.sentBody = region{}
}

// releaseSent releases the attachments of a message that was not sent.
func (m *machMsg) releaseSent() {
	for _, p := range m.sentPorts {
		C.goxpc_port_release(C.mach_port_t(p))
	}
	m.sentPorts = m.sentPorts[:0]
	if m.sentBody.size > 0 {
		C.goxpc_unmap(C.uintptr_t(m.sentBody.addr), C.size_t(m.sentBody.size))
		m.sentBody = region{}
	}
}

// buffer returns the buffer from the start of the message to the end of
// its capacity, where a message is sent from and received into.
func (m *machMsg) buffer() []byte {
	return m.enc.buf[m.start:cap(m.enc.buf)]
}

// needed returns the size of the message too large to receive, as the
// kernel reported it in the header, with room for the trailer.
func (m *machMsg) needed() int {
	return int(binary.LittleEndian.Uint32(m.buffer()[4:])) + trailerSpace
}

// grow makes room to receive the message too large to receive.
func (m *machMsg) grow() {
	m.enc.buf = make([]byte, 0, m.needed())
	m.start = 0
}

func (m *machMsg) id() uint32 {
	return binary.LittleEndian.Uint32(m.buffer()[20:])
}

// local returns the name of the port the message was received on, or,
// after MACH_RCV_TOO_LARGE, of the port the message is queued on.
func (m *machMsg) local() uint32 {
	return binary.LittleEndian.Uint32(m.buffer()[12:])
}

// size returns the size of the received message, without its trailer.
func (m *machMsg) size() int {
	return int(binary.LittleEndian.Uint32(m.buffer()[4:]))
}

// sender returns the identity the kernel recorded for the sender of the
// received message, in its audit trailer. Unlike the message, it cannot
// be forged.
func (m *machMsg) sender() (Peer, bool) {
	b := m.buffer()
	at := m.size()
	if at < headerSize || at+auditTrailer > len(b) {
		return Peer{}, false
	}
	t := b[at:]
	if binary.LittleEndian.Uint32(t[4:]) < auditTrailer { // msgh_trailer_size
		return Peer{}, false
	}
	var p Peer
	for i := range p.AuditToken {
		p.AuditToken[i] = binary.LittleEndian.Uint32(t[20+4*i:])
	}
	p.UID, p.GID, p.PID = p.AuditToken[1], p.AuditToken[2], int(p.AuditToken[5])
	return p, true
}

// sendRaw sends a message of id with body inline to port, of which this
// process holds the receive right. It is for the tests.
func sendRaw(port, id uint32, body []byte) error {
	var p unsafe.Pointer
	if len(body) > 0 {
		p = unsafe.Pointer(&body[0])
	}
	if kr := C.goxpc_send_raw(C.mach_port_t(port), C.mach_msg_id_t(id), p, C.mach_msg_size_t(len(body))); kr != 0 {
		return machError("mach_msg", kr)
	}
	return nil
}

// newPort allocates a receive right. It is for the tests.
func newPort() uint32 { return uint32(C.goxpc_port_alloc()) }

// destroyPort destroys a receive right. It is for the tests.
func destroyPort(p uint32) { C.goxpc_port_destroy(C.mach_port_t(p)) }

// fromKernel reports whether the received message was sent by the kernel,
// as notifications are.
func (m *machMsg) fromKernel() bool {
	p, ok := m.sender()
	return ok && p.PID == 0
}

// parse takes apart the message just received in the buffer. Only port
// and out-of-line memory descriptors are expected; whatever else a message
// carries is still released by releaseReceived.
func (m *machMsg) parse() error {
	b := m.buffer()
	bits := binary.LittleEndian.Uint32(b)
	size := m.size()
	if size < headerSize || size > len(b) {
		return errMalformed
	}
	if m.limit > 0 && size > m.limit {
		return ErrMessageTooLarge
	}
	b = b[:size]
	at := headerSize
	var ool []region
	if bits&C.MACH_MSGH_BITS_COMPLEX != 0 {
		if size < at+4 {
			return errMalformed
		}
		n := int(binary.LittleEndian.Uint32(b[at:]))
		at += 4
		for range n {
			if size < at+portDescSize {
				return errMalformed
			}
			switch b[at+11] {
			case C.MACH_MSG_PORT_DESCRIPTOR:
				m.ports = append(m.ports, binary.LittleEndian.Uint32(b[at:]))
				at += portDescSize
			case C.MACH_MSG_OOL_DESCRIPTOR:
				if size < at+oolDescSize {
					return errMalformed
				}
				ool = append(ool, region{
					addr: uintptr(binary.LittleEndian.Uint64(b[at:])),
					size: int(binary.LittleEndian.Uint32(b[at+12:])),
				})
				at += oolDescSize
			default:
				// Port arrays, volatile memory and guarded ports are not
				// part of the protocol.
				return errMalformed
			}
		}
	}
	m.body = b[at:]
	if len(m.body) == 0 && len(ool) > 0 {
		// The body travelled out of line.
		r := ool[0]
		if m.limit > 0 && r.size > m.limit {
			return ErrMessageTooLarge
		}
		m.body = unsafe.Slice((*byte)(cPointer(r.addr)), r.size)
	}
	return nil
}

// releaseReceived releases what the received message carries, port rights
// and out-of-line memory of every kind.
func (m *machMsg) releaseReceived() {
	if m.received {
		C.goxpc_destroy(unsafe.Pointer(&m.buffer()[0]))
		m.received = false
	}
	m.ports = m.ports[:0]
	m.body = nil
}

// decoder prepares m.dec to read the received message's body.
func (m *machMsg) decoder() (*decoder, error) {
	if err := m.dec.init(m.body, m.ports, machResolver{}); err != nil {
		return nil, err
	}
	m.dec.limit = m.limit
	return &m.dec, nil
}

// machResolver resolves the ports attached to a received Mach message:
// memory entries holding data, and fileports.
type machResolver struct{}

func (machResolver) data(port uint32, n int) ([]byte, error) {
	var addr C.uintptr_t
	if kr := C.goxpc_map(C.mach_port_t(port), C.size_t(n), &addr); kr != 0 {
		return nil, machError("mach_vm_map", kr)
	}
	b := append([]byte{}, unsafe.Slice((*byte)(cPointer(uintptr(addr))), n)...)
	C.goxpc_unmap(addr, C.size_t(n))
	return b, nil
}

func (machResolver) file(port uint32) (*os.File, error) {
	fd := C.goxpc_fileport_fd(C.mach_port_t(port))
	if fd < 0 {
		return nil, errors.New("xpc: cannot receive file descriptor")
	}
	return os.NewFile(uintptr(fd), "xpc-fd"), nil
}

// Reply ports are reused: once its reply or the notification replacing it
// arrived, nothing else can be sent to one.
var replyPorts struct {
	sync.Mutex
	free []uint32
}

func getReplyPort() uint32 {
	replyPorts.Lock()
	if n := len(replyPorts.free); n > 0 {
		p := replyPorts.free[n-1]
		replyPorts.free = replyPorts.free[:n-1]
		replyPorts.Unlock()
		return p
	}
	replyPorts.Unlock()
	return uint32(C.goxpc_port_alloc())
}

func putReplyPort(p uint32) {
	replyPorts.Lock()
	if len(replyPorts.free) < 64 {
		replyPorts.free = append(replyPorts.free, p)
		p = 0
	}
	replyPorts.Unlock()
	if p != 0 {
		C.goxpc_port_destroy(C.mach_port_t(p))
	}
}

// cPointer converts an address of C memory that Mach reported.
func cPointer(addr uintptr) unsafe.Pointer {
	return unsafe.Add(nil, addr)
}

func machError(op string, kr C.kern_return_t) error {
	return fmt.Errorf("xpc: %s: kern_return %#x", op, uint32(kr))
}
