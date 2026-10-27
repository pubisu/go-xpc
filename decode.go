package xpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"sync"
	"time"
)

// A portResolver turns the ports attached to a received message into the
// values they stand for.
type portResolver interface {
	// data returns a copy of the n bytes in the memory entry port.
	data(port uint32, n int) ([]byte, error)
	// file returns a file for the fileport port.
	file(port uint32) (*os.File, error)
}

var decoderPool = sync.Pool{New: func() any { return new(decoder) }}

func getDecoder() *decoder { return decoderPool.Get().(*decoder) }

func (d *decoder) put() {
	d.release()
	decoderPool.Put(d)
}

// A decoder reads values in the wire format. Messages come from another
// process, so every read is checked.
type decoder struct {
	b     []byte
	off   int
	ports []uint32
	next  int // index of the next port to use
	res   portResolver
	files []*os.File // received so far, closed if decoding fails
	limit int        // largest data entry accepted; 0 for no limit
	depth int        // of the dictionaries and arrays being read

	// keys interns dictionary keys, which repeat from one dictionary and
	// message to the next. It is indexed by a hash of the key.
	keys [256]string
}

// maxInternedKey is the length of the longest key decoders intern.
const maxInternedKey = 64

// init prepares d to read the message body b, checking its header.
func (d *decoder) init(b []byte, ports []uint32, res portResolver) error {
	d.b, d.off, d.ports, d.next, d.res = b, 0, ports, 0, res
	d.limit, d.depth = 0, 0
	if len(b) < 8 || binary.LittleEndian.Uint32(b) != wireMagic {
		return errMalformed
	}
	if v := binary.LittleEndian.Uint32(b[4:]); v != wireVersion {
		return fmt.Errorf("xpc: unsupported wire version %d", v)
	}
	d.off = 8
	return nil
}

// release drops the decoder's references to the message.
func (d *decoder) release() {
	d.b, d.ports, d.res = nil, nil, nil
	clear(d.files)
	d.files = d.files[:0]
}

// closeFiles closes the files received so far, which nothing will own
// after a failed decoding.
func (d *decoder) closeFiles() {
	for _, f := range d.files {
		f.Close()
	}
	clear(d.files)
	d.files = d.files[:0]
}

func (d *decoder) u32() (uint32, error) {
	if len(d.b)-d.off < 4 {
		return 0, errMalformed
	}
	v := binary.LittleEndian.Uint32(d.b[d.off:])
	d.off += 4
	return v, nil
}

func (d *decoder) u64() (uint64, error) {
	if len(d.b)-d.off < 8 {
		return 0, errMalformed
	}
	v := binary.LittleEndian.Uint64(d.b[d.off:])
	d.off += 8
	return v, nil
}

// bytes returns the next n bytes, skipping the padding after them.
func (d *decoder) bytes(n int) ([]byte, error) {
	if n < 0 || len(d.b)-d.off < n {
		return nil, errMalformed
	}
	b := d.b[d.off : d.off+n : d.off+n]
	d.off += (n + 3) &^ 3
	if d.off > len(d.b) {
		d.off = len(d.b)
	}
	return b, nil
}

func (d *decoder) port() (uint32, error) {
	if d.next >= len(d.ports) {
		return 0, errMalformed
	}
	p := d.ports[d.next]
	d.next++
	return p, nil
}

// Limits on what a message can make the decoder do.
const (
	// maxDepth is the deepest nesting of dictionaries and arrays decoded:
	// decoding recurses, and the stack must not run out.
	maxDepth = 512
	// maxPrealloc is the most entries made room for ahead of decoding them:
	// a count is only a claim until the entries are read.
	maxPrealloc = 1024
)

// errTooDeep reports a message nested deeper than maxDepth.
var errTooDeep = errors.New("xpc: message nested too deeply")

// container reads the length and count of a dictionary or array, entering
// it: the caller decrements d.depth once it has read the entries.
func (d *decoder) container() (int, error) {
	if d.depth++; d.depth > maxDepth {
		return 0, errTooDeep
	}
	if _, err := d.u32(); err != nil {
		return 0, err
	}
	n, err := d.u32()
	if err != nil {
		return 0, err
	}
	// Every entry takes at least 4 bytes, which bounds what a corrupt
	// count can make the decoder allocate.
	if int(n) > (len(d.b)-d.off)/4 {
		return 0, errMalformed
	}
	return int(n), nil
}

// key reads a dictionary key.
func (d *decoder) key() (string, error) {
	rest := d.b[d.off:]
	n := 0
	for n < len(rest) && rest[n] != 0 {
		n++
	}
	if n == len(rest) {
		return "", errMalformed
	}
	b, _ := d.bytes(n + 1)
	b = b[:n]
	if n > maxInternedKey {
		return string(b), nil
	}
	h := uint32(2166136261) // FNV-1a
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}
	slot := &d.keys[h%uint32(len(d.keys))]
	if *slot != string(b) {
		*slot = string(b)
	}
	return *slot, nil
}

// str reads the payload of a string.
func (d *decoder) str() (string, error) {
	n, err := d.u32()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", errMalformed
	}
	b, err := d.bytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b[:n-1]), nil
}

// dataValue reads the payload of a data object.
func (d *decoder) dataValue() ([]byte, error) {
	n, err := d.u32()
	if err != nil {
		return nil, err
	}
	if n > maxInlineData {
		return d.attachedData(n)
	}
	b, err := d.bytes(int(n))
	if err != nil {
		return nil, err
	}
	return append([]byte{}, b...), nil
}

// attachedData returns a copy of data of n bytes that travelled in a
// memory entry attached to the message, if n is within the limit.
func (d *decoder) attachedData(n uint32) ([]byte, error) {
	if d.limit > 0 && uint64(n) > uint64(d.limit) {
		return nil, ErrMessageTooLarge
	}
	p, err := d.port()
	if err != nil {
		return nil, err
	}
	return d.res.data(p, int(n))
}

func (d *decoder) fileValue() (*os.File, error) {
	p, err := d.port()
	if err != nil {
		return nil, err
	}
	f, err := d.res.file(p)
	if err != nil {
		return nil, err
	}
	d.files = append(d.files, f)
	return f, nil
}

func (d *decoder) uuid() (UUID, error) {
	var u UUID
	b, err := d.bytes(len(u))
	if err == nil {
		copy(u[:], b)
	}
	return u, err
}

// decodeMessage stores the message body d was initialized with in the
// value v points to, decoding it straight from the wire format. A nil v
// discards the message.
func (d *decoder) decodeMessage(v any) error {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("xpc: cannot decode into %T, need a non-nil pointer", v)
	}
	d.depth = 0
	if err := d.into(rv.Elem()); err != nil {
		d.closeFiles()
		return err
	}
	// The files decoded belong to the caller now.
	clear(d.files)
	d.files = d.files[:0]
	return nil
}

// value reads the next object as its canonical Go value.
func (d *decoder) value() (any, error) {
	typ, err := d.u32()
	if err != nil {
		return nil, err
	}
	switch typ {
	case wireString:
		return d.str()
	case wireInt64:
		v, err := d.u64()
		return int64(v), err
	case wireDictionary:
		n, err := d.container()
		if err != nil {
			return nil, err
		}
		out := make(map[string]any, min(n, maxPrealloc))
		for range n {
			k, err := d.key()
			if err != nil {
				return nil, err
			}
			v, err := d.value()
			if err != nil {
				return nil, fmt.Errorf("xpc: key %q: %w", k, err)
			}
			out[k] = v
		}
		d.depth--
		return out, nil
	case wireBool:
		v, err := d.u32()
		return v != 0, err
	case wireArray:
		n, err := d.container()
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, min(n, maxPrealloc))
		for i := range n {
			v, err := d.value()
			if err != nil {
				return nil, fmt.Errorf("xpc: index %d: %w", i, err)
			}
			out = append(out, v)
		}
		d.depth--
		return out, nil
	case wireUint64:
		return d.u64()
	case wireDouble:
		v, err := d.u64()
		return math.Float64frombits(v), err
	case wireData:
		return d.dataValue()
	case wireNull:
		return nil, nil
	case wireDate:
		v, err := d.u64()
		return time.Unix(0, int64(v)), err
	case wireUUID:
		return d.uuid()
	case wireFD:
		return d.fileValue()
	}
	if err := d.skipPayload(typ); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("xpc: unsupported object type %s", wireTypeName(typ))
}

// skip moves past the next object and everything it contains, without
// converting it.
func (d *decoder) skip() error {
	typ, err := d.u32()
	if err != nil {
		return err
	}
	return d.skipPayload(typ)
}

// skipPayload moves past the payload of an object of type typ, whose type
// has been read, consuming the ports it attaches.
func (d *decoder) skipPayload(typ uint32) error {
	switch typ {
	case wireNull:
		return nil
	case wireBool:
		_, err := d.u32()
		return err
	case wireInt64, wireUint64, wireDouble, wireDate:
		_, err := d.u64()
		return err
	case wireUUID:
		_, err := d.bytes(16)
		return err
	case wireString:
		n, err := d.u32()
		if err == nil {
			_, err = d.bytes(int(n))
		}
		return err
	case wireData:
		n, err := d.u32()
		if err != nil {
			return err
		}
		if n > maxInlineData {
			_, err = d.port()
		} else {
			_, err = d.bytes(int(n))
		}
		return err
	case wireFD, wireMachSend, wireEndpoint:
		_, err := d.port()
		return err
	case wireShmem:
		if _, err := d.u64(); err != nil {
			return err
		}
		_, err := d.port()
		return err
	case wireDictionary, wireArray:
		// The length covers the whole payload, but ports must be counted.
		n, err := d.container()
		if err != nil {
			return err
		}
		for range n {
			if typ == wireDictionary {
				if _, err := d.key(); err != nil {
					return err
				}
			}
			if err := d.skip(); err != nil {
				return err
			}
		}
		d.depth--
		return nil
	}
	return fmt.Errorf("xpc: unknown object type %#x", typ)
}
