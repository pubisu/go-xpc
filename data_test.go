package xpc

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

// csv encodes a list of strings as data, comma-separated.
type csv []string

func (c csv) MarshalXPCData() ([]byte, error) { return []byte(strings.Join(c, ",")), nil }

func (c *csv) UnmarshalXPCData(b []byte) error {
	*c = strings.Split(string(b), ",")
	return nil
}

// ptrData has its marshaler on the pointer.
type ptrData struct{ N int }

func (p *ptrData) MarshalXPCData() ([]byte, error) { return []byte(fmt.Sprint(p.N)), nil }

func (p *ptrData) UnmarshalXPCData(b []byte) error {
	_, err := fmt.Sscan(string(b), &p.N)
	return err
}

func TestDataMarshaler(t *testing.T) {
	type msg struct {
		List   csv            `xpc:"list"`
		Ptr    ptrData        `xpc:"ptr"`
		NilPtr *ptrData       `xpc:"nilPtr"`
		Many   []csv          `xpc:"many"`
		ByKey  map[string]csv `xpc:"byKey"`
	}
	in := msg{
		List:  csv{"a", "b"},
		Ptr:   ptrData{7},
		Many:  []csv{{"x"}, {"y", "z"}},
		ByKey: map[string]csv{"k": {"v"}},
	}
	b := encodeTestMessage(t, &in)
	var m map[string]any
	if err := decodeTestMessage(b, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"list":   []byte("a,b"),
		"ptr":    []byte("7"),
		"nilPtr": nil,
		"many":   []any{[]byte("x"), []byte("y,z")},
		"byKey":  map[string]any{"k": []byte("v")},
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("Marshal = %#v\nwant %#v", m, want)
	}
	var out msg
	if err := decodeTestMessage(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("Unmarshal = %#v\nwant %#v", out, in)
	}
}

// borrow records the slice it was decoded from.
type borrow struct{ b []byte }

func (r *borrow) UnmarshalXPCData(b []byte) error {
	r.b = b
	return nil
}

func TestDataUnmarshalerBorrows(t *testing.T) {
	var e encoder
	e.reset(0)
	if err := e.messageValue(map[string]any{"d": []byte("payload")}); err != nil {
		t.Fatal(err)
	}
	var d decoder
	if err := d.init(e.buf, nil, nil); err != nil {
		t.Fatal(err)
	}
	var out struct {
		D borrow `xpc:"d"`
	}
	if err := d.decodeMessage(&out); err != nil {
		t.Fatal(err)
	}
	start := uintptr(unsafe.Pointer(&e.buf[0]))
	if p := uintptr(unsafe.Pointer(&out.D.b[0])); p < start || p >= start+uintptr(len(e.buf)) || string(out.D.b) != "payload" {
		t.Errorf("UnmarshalXPCData got %q, not the message's own bytes", out.D.b)
	}
}
