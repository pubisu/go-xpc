package xpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"time"
)

// An encoder writes Go values in the wire format, converted as the package
// documentation describes, to buf after its first base bytes, which hold
// the Mach message header.
type encoder struct {
	buf         []byte
	base        int
	attachments []attachment
}

func (e *encoder) reset(base int) {
	clear(e.attachments)
	e.attachments = e.attachments[:0]
	e.buf = append(e.buf[:0], make([]byte, base)...)
	e.base = base
}

func (e *encoder) u32(v uint32) {
	e.buf = binary.LittleEndian.AppendUint32(e.buf, v)
}

func (e *encoder) word(typ uint32, v uint64) {
	e.u32(typ)
	e.buf = binary.LittleEndian.AppendUint64(e.buf, v)
}

func (e *encoder) pad() {
	for (len(e.buf)-e.base)%4 != 0 {
		e.buf = append(e.buf, 0)
	}
}

// messageValue writes v, which must encode as a dictionary, as a whole
// message body; nil is an empty dictionary.
func (e *encoder) messageValue(v any) error {
	e.u32(wireMagic)
	e.u32(wireVersion)
	if v == nil {
		e.end(e.begin(wireDictionary), 0)
		return nil
	}
	at := len(e.buf)
	if err := e.value(v); err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(e.buf[at:]) != wireDictionary {
		return fmt.Errorf("xpc: %T does not encode as a dictionary, as a message must", v)
	}
	return nil
}

// begin starts a dictionary or array and returns the offset of its length.
func (e *encoder) begin(typ uint32) int {
	e.u32(typ)
	at := len(e.buf)
	e.u32(0) // length, set by end
	e.u32(0) // count, set by end
	return at
}

func (e *encoder) end(at, count int) {
	binary.LittleEndian.PutUint32(e.buf[at:], uint32(len(e.buf)-at-4))
	binary.LittleEndian.PutUint32(e.buf[at+4:], uint32(count))
}

func (e *encoder) key(k string) error {
	if strings.IndexByte(k, 0) >= 0 {
		return fmt.Errorf("xpc: dictionary key %q contains NUL", k)
	}
	e.buf = append(append(e.buf, k...), 0)
	e.pad()
	return nil
}

func (e *encoder) string(s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("xpc: string %q contains NUL", s)
	}
	if len(s) >= math.MaxUint32 {
		return errTooLong
	}
	e.u32(wireString)
	e.u32(uint32(len(s) + 1))
	e.buf = append(append(e.buf, s...), 0)
	e.pad()
	return nil
}

// errTooLong reports a string or data too long for the 32-bit lengths of
// the wire format.
var errTooLong = errors.New("xpc: string or data longer than 4 GiB")

func (e *encoder) data(b []byte) error {
	if uint64(len(b)) > math.MaxUint32 {
		return errTooLong
	}
	e.u32(wireData)
	e.u32(uint32(len(b)))
	if len(b) > maxInlineData {
		e.attachments = append(e.attachments, attachment{data: b})
		return nil
	}
	e.buf = append(e.buf, b...)
	e.pad()
	return nil
}

func (e *encoder) bool(b bool) {
	e.u32(wireBool)
	if b {
		e.u32(1)
	} else {
		e.u32(0)
	}
}

func (e *encoder) dict(m map[string]any) error {
	at := e.begin(wireDictionary)
	for k, v := range m {
		if err := e.key(k); err != nil {
			return err
		}
		if err := e.value(v); err != nil {
			return fmt.Errorf("xpc: key %q: %w", k, err)
		}
	}
	e.end(at, len(m))
	return nil
}

// value writes v. Common types are written without reflection, and a
// DataMarshaler writes itself; the rest goes to reflect.
func (e *encoder) value(v any) error {
	switch v := v.(type) {
	case nil:
		e.u32(wireNull)
	case string:
		return e.string(v)
	case int64:
		e.word(wireInt64, uint64(v))
	case int:
		e.word(wireInt64, uint64(v))
	case bool:
		e.bool(v)
	case float64:
		e.word(wireDouble, math.Float64bits(v))
	case uint64:
		e.word(wireUint64, v)
	case []byte:
		return e.data(v)
	case map[string]any:
		if v == nil {
			e.u32(wireNull)
			return nil
		}
		return e.dict(v)
	case []any:
		if v == nil {
			e.u32(wireNull)
			return nil
		}
		at := e.begin(wireArray)
		for i, x := range v {
			if err := e.value(x); err != nil {
				return fmt.Errorf("xpc: index %d: %w", i, err)
			}
		}
		e.end(at, len(v))
	case time.Time:
		e.word(wireDate, uint64(v.UnixNano()))
	case UUID:
		e.u32(wireUUID)
		e.buf = append(e.buf, v[:]...)
	case *os.File:
		if v == nil {
			e.u32(wireNull)
			return nil
		}
		e.u32(wireFD)
		e.attachments = append(e.attachments, attachment{file: v})
	case Endpoint:
		if v.l == nil {
			return errors.New("xpc: zero Endpoint")
		}
		e.u32(wireEndpoint)
		e.attachments = append(e.attachments, attachment{endpoint: v.l})
	default:
		rv := reflect.ValueOf(v)
		if m, ok := v.(DataMarshaler); ok && (rv.Kind() != reflect.Pointer || !rv.IsNil()) {
			return e.marshaled(m)
		}
		return e.reflect(rv)
	}
	return nil
}

// reflect writes rv, converted as the package documentation describes.
func (e *encoder) reflect(rv reflect.Value) error {
	// The special types, found by kind first: getting the type costs more,
	// and is done once.
	switch rv.Kind() {
	case reflect.Struct, reflect.Array, reflect.Pointer:
		if t := rv.Type(); t == timeType || t == endType || t == uuidType || t == fileType {
			return e.value(rv.Interface())
		}
	}
	switch rv.Kind() {
	case reflect.Bool:
		e.bool(rv.Bool())
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.word(wireInt64, uint64(rv.Int()))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e.word(wireUint64, rv.Uint())
		return nil
	case reflect.Float32, reflect.Float64:
		e.word(wireDouble, math.Float64bits(rv.Float()))
		return nil
	case reflect.String:
		return e.string(rv.String())
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			break
		}
		if rv.IsNil() {
			e.u32(wireNull)
			return nil
		}
		at := e.begin(wireDictionary)
		// Reused for every entry, as MapIter.Key and Value allocate.
		k := reflect.New(rv.Type().Key()).Elem()
		v := reflect.New(rv.Type().Elem()).Elem()
		flags := dataFlagsOf(v.Type())
		for it := rv.MapRange(); it.Next(); {
			k.SetIterKey(it)
			v.SetIterValue(it)
			if err := e.key(k.String()); err != nil {
				return err
			}
			if err := e.reflectData(v, flags); err != nil {
				return fmt.Errorf("xpc: key %q: %w", k.String(), err)
			}
		}
		e.end(at, rv.Len())
		return nil
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			if rv.Kind() == reflect.Array && !rv.CanAddr() {
				a := reflect.New(rv.Type()).Elem()
				a.Set(rv)
				rv = a
			}
			return e.data(rv.Bytes())
		}
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			e.u32(wireNull)
			return nil
		}
		at := e.begin(wireArray)
		flags := dataFlagsOf(rv.Type().Elem())
		for i := range rv.Len() {
			if err := e.reflectData(rv.Index(i), flags); err != nil {
				return fmt.Errorf("xpc: index %d: %w", i, err)
			}
		}
		e.end(at, rv.Len())
		return nil
	case reflect.Struct:
		at := e.begin(wireDictionary)
		n := 0
		for _, f := range structFields(rv.Type()) {
			fv, err := rv.FieldByIndexErr(f.index)
			if err != nil {
				continue // field promoted through a nil embedded pointer
			}
			if f.omitEmpty && fv.IsZero() {
				continue
			}
			if err := e.key(f.name); err != nil {
				return err
			}
			if err := e.reflectData(fv, f.data); err != nil {
				return fmt.Errorf("xpc: key %q: %w", f.name, err)
			}
			n++
		}
		e.end(at, n)
		return nil
	case reflect.Interface:
		if rv.IsNil() {
			e.u32(wireNull)
			return nil
		}
		// Not addressable, so Interface does not copy the value.
		return e.value(rv.Elem().Interface())
	case reflect.Pointer:
		if rv.IsNil() {
			e.u32(wireNull)
			return nil
		}
		return e.reflectData(rv.Elem(), dataFlagsOf(rv.Type().Elem()))
	}
	return fmt.Errorf("xpc: unsupported type %s", rv.Type())
}
