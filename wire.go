package xpc

import (
	"fmt"
	"os"
)

// The XPC wire format, as libxpc writes it in the body of Mach messages:
//
//	message    = "CPX@" u32(5) dictionary
//	object     = u32 type, payload
//	dictionary = u32 length, u32 count, (key, object)...
//	array      = u32 length, u32 count, object...
//	key        = bytes, NUL, padding
//	string     = u32 length including the NUL, bytes, NUL, padding
//	data       = u32 length, bytes, padding; longer data travels in a
//	             memory entry attached to the message instead
//	bool       = u32
//	int64, uint64, double, date = 8 bytes
//	uuid       = 16 bytes
//	fd, endpoint, mach_send = no payload, an attached port
//	shmem      = 8 bytes, an attached port
//
// Integers are little-endian, lengths count the bytes after the length
// field, and padding aligns to 4 bytes. Ports are attached to the Mach
// message in the order the objects needing them appear.
const (
	wireMagic   = 0x40585043 // "CPX@"
	wireVersion = 5

	wireNull       = 0x1000
	wireBool       = 0x2000
	wireInt64      = 0x3000
	wireUint64     = 0x4000
	wireDouble     = 0x5000
	wireDate       = 0x7000
	wireData       = 0x8000
	wireString     = 0x9000
	wireUUID       = 0xa000
	wireFD         = 0xb000
	wireShmem      = 0xc000
	wireMachSend   = 0xd000
	wireArray      = 0xe000
	wireDictionary = 0xf000
	wireEndpoint   = 0x12000

	// Data longer than this travels in a memory entry, as libxpc does.
	maxInlineData = 16384
)

// An attachment is a port an encoded message needs: a memory entry
// holding data, a fileport for a file, or a send right to a listener.
type attachment struct {
	data     []byte
	file     *os.File
	endpoint *Listener
}

func wireTypeName(typ uint32) string {
	switch typ {
	case wireShmem:
		return "shmem"
	case wireMachSend:
		return "mach_send"
	case wireEndpoint:
		return "endpoint"
	}
	return fmt.Sprintf("%#x", typ)
}

// wireType names the canonical Go type of objects of type typ, as %T
// would.
func wireType(typ uint32) string {
	switch typ {
	case wireBool:
		return "bool"
	case wireInt64:
		return "int64"
	case wireUint64:
		return "uint64"
	case wireDouble:
		return "float64"
	case wireString:
		return "string"
	case wireData:
		return "[]uint8"
	case wireDate:
		return "time.Time"
	case wireUUID:
		return "xpc.UUID"
	case wireFD:
		return "*os.File"
	case wireArray:
		return "[]interface {}"
	case wireDictionary:
		return "map[string]interface {}"
	}
	return wireTypeName(typ)
}
