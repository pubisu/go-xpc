package xpc

import (
	"encoding/binary"
	"errors"
	"os"
	"reflect"
	"runtime"
	"testing"
)

// FuzzDecode checks that malformed messages are rejected, not crashed on.
func FuzzDecode(f *testing.F) {
	for _, msg := range []map[string]any{benchMessage(), {"a": []any{map[string]any{"b": nil}}}, {}} {
		var e encoder
		e.reset(0)
		if err := e.messageValue(msg); err != nil {
			f.Fatal(err)
		}
		f.Add(e.buf)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var d decoder
		if d.init(b, nil, nil) == nil {
			d.value()
		}
		if d.init(b, nil, nil) == nil {
			var v struct {
				A []map[string]*int
				S string
			}
			d.into(reflect.ValueOf(&v).Elem())
		}
		if d.init(b, nil, nil) == nil {
			r := newWireResult(&d, &testProtocol, func() {})
			r.failure()
			var v struct{ Product int }
			r.decode(&v)
		}
	})
}

// rawMessage starts a message whose dictionary holds the single entry key,
// whose value the caller appends.
func rawMessage(key string) []byte {
	var b []byte
	for _, v := range []uint32{wireMagic, wireVersion, wireDictionary, 0, 1} {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	b = append(b, key...)
	for range 4 - len(key)%4 {
		b = append(b, 0)
	}
	return b
}

// deepMessage returns a message whose entry "a" nests depth arrays.
func deepMessage(depth int) []byte {
	b := rawMessage("a")
	for range depth {
		for _, v := range []uint32{wireArray, 0, 1} {
			b = binary.LittleEndian.AppendUint32(b, v)
		}
	}
	return binary.LittleEndian.AppendUint32(b, wireNull)
}

// TestDecodeDepthLimit checks that a deeply nested message fails to decode
// rather than exhausting the stack, which would crash the process.
func TestDecodeDepthLimit(t *testing.T) {
	for _, tc := range []struct {
		depth int
		ok    bool
	}{
		{maxDepth - 1, true}, // with the dictionary, maxDepth levels
		{maxDepth, false},
		{1_000_000, false},
	} {
		b := deepMessage(tc.depth)
		check := func(how string, err error) {
			t.Helper()
			if tc.ok && err != nil {
				t.Errorf("depth %d, %s: %v", tc.depth, how, err)
			}
			if !tc.ok && !errors.Is(err, errTooDeep) {
				t.Errorf("depth %d, %s: %v, want errTooDeep", tc.depth, how, err)
			}
		}
		var d decoder
		d.init(b, nil, nil)
		_, err := d.value()
		check("as any", err)
		var typed struct{ A any }
		check("into a field", decodeTestMessage(b, &typed))
		var skipped struct{ B int }
		check("skipped", decodeTestMessage(b, &skipped))
	}
}

// TestDecodePreallocationBounded checks that a count the message claims is
// not trusted for memory before the entries are read.
func TestDecodePreallocationBounded(t *testing.T) {
	const n = 100_000
	b := rawMessage("a")
	for _, v := range []uint32{wireArray, 0, n} {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	for range n {
		b = binary.LittleEndian.AppendUint32(b, 0xffffffff) // no such type
	}
	var out struct {
		A []struct{ B [1024]byte }
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := decodeTestMessage(b, &out)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("decoding junk succeeded")
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
		t.Errorf("decoding allocated %d bytes for a message of %d", alloc, len(b))
	}
}

// TestAttachedDataLimit checks that data larger than the limit is refused
// before it is mapped.
func TestAttachedDataLimit(t *testing.T) {
	b := rawMessage("d")
	b = binary.LittleEndian.AppendUint32(b, wireData)
	b = binary.LittleEndian.AppendUint32(b, maxInlineData+1) // in a memory entry
	var d decoder
	if err := d.init(b, nil, nil); err != nil {
		t.Fatal(err)
	}
	d.limit = maxInlineData
	var out struct{ D []byte }
	if err := d.decodeMessage(&out); !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("decoding = %v, want ErrMessageTooLarge", err)
	}
}

// fileResolver resolves every fileport to f.
type fileResolver struct{ f *os.File }

func (r fileResolver) data(uint32, int) ([]byte, error) { return nil, errMalformed }

func (r fileResolver) file(uint32) (*os.File, error) { return r.f, nil }

// TestDecodedFilesBelongToCaller checks that files decoded successfully are
// not closed afterwards, as cleanup after a failed decoding would.
func TestDecodedFilesBelongToCaller(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	b := binary.LittleEndian.AppendUint32(rawMessage("f"), wireFD)
	var d decoder
	if err := d.init(b, []uint32{0}, fileResolver{w}); err != nil {
		t.Fatal(err)
	}
	var out struct{ F *os.File }
	if err := d.decodeMessage(&out); err != nil {
		t.Fatal(err)
	}
	d.closeFiles()
	if _, err := out.F.Write([]byte("x")); err != nil {
		t.Errorf("decoded file closed: %v", err)
	}
}
