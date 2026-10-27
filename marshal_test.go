package xpc

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type Inner struct {
	Note string
}

type sample struct {
	Name       string `xpc:"name"`
	Count      int
	Ratio      float32
	Tags       []string
	Labels     map[string]int
	Raw        []byte
	ID         UUID
	When       time.Time
	Opt        *int   `xpc:"opt,omitempty"`
	Skip       string `xpc:"-"`
	Small      uint8
	Children   []Inner
	Any        any
	Inner      // promoted
	unexported int
}

func encodeTestMessage(t testing.TB, value any) []byte {
	t.Helper()
	var e encoder
	e.reset(0)
	if err := e.messageValue(value); err != nil {
		t.Fatal(err)
	}
	return e.buf
}

func decodeTestMessage(data []byte, value any) error {
	var d decoder
	if err := d.init(data, nil, nil); err != nil {
		return err
	}
	return d.decodeMessage(value)
}

func TestMarshalUnmarshal(t *testing.T) {
	seven := 7
	in := sample{
		Name:     "n",
		Count:    -3,
		Ratio:    0.5,
		Tags:     []string{"a", "b"},
		Labels:   map[string]int{"x": 1},
		Raw:      []byte{1, 2},
		ID:       UUID{9},
		When:     time.Unix(100, 0),
		Opt:      &seven,
		Skip:     "dropped",
		Small:    200,
		Children: []Inner{{Note: "c"}},
		Any:      []any{"z"},
		Note:     "promoted",
	}
	b := encodeTestMessage(t, in)
	if !bytes.HasPrefix(b, []byte("CPX@\x05\x00\x00\x00\x00\xf0\x00\x00")) {
		t.Errorf("Marshal = % x..., want a version 5 message holding a dictionary", b[:12])
	}

	// What a service receives, seen through a map.
	var got map[string]any
	if err := decodeTestMessage(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name":     "n",
		"Count":    int64(-3),
		"Ratio":    0.5,
		"Tags":     []any{"a", "b"},
		"Labels":   map[string]any{"x": int64(1)},
		"Raw":      []byte{1, 2},
		"ID":       UUID{9},
		"When":     time.Unix(100, 0),
		"opt":      int64(7),
		"Small":    uint64(200),
		"Children": []any{map[string]any{"Note": "c"}},
		"Any":      []any{"z"},
		"Note":     "promoted",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("message\n got: %#v\nwant: %#v", got, want)
	}

	var out sample
	if err := decodeTestMessage(b, &out); err != nil {
		t.Fatal(err)
	}
	in.Skip = ""
	if !reflect.DeepEqual(out, in) {
		t.Errorf("Unmarshal\n got: %#v\nwant: %#v", out, in)
	}

	b = encodeTestMessage(t, sample{})
	var zero map[string]any
	if err := decodeTestMessage(b, &zero); err != nil {
		t.Fatal(err)
	}
	if _, ok := zero["opt"]; ok {
		t.Errorf("omitempty field present: %v", zero)
	}
}

// convert sends src through the wire format as the entry "v" of a message
// and decodes that entry into the value dst points to.
func convert(t testing.TB, src, dst any) error {
	b := encodeTestMessage(t, map[string]any{"v": src})
	dt := reflect.TypeOf(dst).Elem()
	holder := reflect.New(reflect.StructOf([]reflect.StructField{{Name: "V", Type: dt, Tag: `xpc:"v"`}}))
	holder.Elem().Field(0).Set(reflect.ValueOf(dst).Elem())
	if err := decodeTestMessage(b, holder.Interface()); err != nil {
		return err
	}
	reflect.ValueOf(dst).Elem().Set(holder.Elem().Field(0))
	return nil
}

func TestUnmarshalCaseInsensitive(t *testing.T) {
	b := encodeTestMessage(t, map[string]any{"username": "u", "name": "folded", "Name": "exact", "NAME": "folded"})
	var out struct{ UserName, Name string }
	if err := decodeTestMessage(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.UserName != "u" || out.Name != "exact" {
		t.Errorf("got %+v, want UserName u and the exactly named Name", out)
	}
}

func TestUnmarshalNumbers(t *testing.T) {
	var i8 int8
	if err := convert(t, uint64(100), &i8); err != nil || i8 != 100 {
		t.Errorf("int8 from uint64: %v, %v", i8, err)
	}
	var u uint
	if err := convert(t, int64(5), &u); err != nil || u != 5 {
		t.Errorf("uint from int64: %v, %v", u, err)
	}
	var f float64
	if err := convert(t, int64(2), &f); err != nil || f != 2 {
		t.Errorf("float64 from int64: %v, %v", f, err)
	}
	for _, tc := range []struct {
		src any
		dst any
	}{
		{int64(300), new(int8)},
		{int64(-1), new(uint)},
		{uint64(1 << 63), new(int64)},
		{"x", new(int)},
		{int64(1), new(string)},
		{[]any{int64(1)}, new([2]int)},
		{map[string]any{}, new([]int)},
		{[]byte{1}, new([3]byte)},
		{int64(1), new(fmt.Stringer)},
		{"now", new(time.Time)},
	} {
		if err := convert(t, tc.src, tc.dst); err == nil {
			t.Errorf("decoding %#v into %T succeeded, want an error", tc.src, tc.dst)
		}
	}
}

func TestUnmarshalNilAndPointers(t *testing.T) {
	p := new(int)
	if err := convert(t, nil, &p); err != nil || p != nil {
		t.Errorf("nil into *int: %v, %v", p, err)
	}
	if err := convert(t, int64(4), &p); err != nil || p == nil || *p != 4 {
		t.Errorf("int64 into *int: %v, %v", p, err)
	}
	var pp **[]byte
	if err := convert(t, []byte{9}, &pp); err != nil || pp == nil || !bytes.Equal(**pp, []byte{9}) {
		t.Errorf("data into **[]byte: %v", err)
	}
	b := encodeTestMessage(t, map[string]any{})
	if err := decodeTestMessage(b, 3); err == nil {
		t.Error("Unmarshal into non-pointer succeeded")
	}
	if err := decodeTestMessage(b, nil); err != nil {
		t.Errorf("discard message: %v", err)
	}
	if err := decodeTestMessage([]byte("junk"), new(map[string]any)); err == nil {
		t.Error("Unmarshal of junk succeeded")
	}
}

func TestUnmarshalCollections(t *testing.T) {
	type myString string
	var m map[myString]int64
	if err := convert(t, map[string]any{"a": 1, "b": 2}, &m); err != nil || !reflect.DeepEqual(m, map[myString]int64{"a": 1, "b": 2}) {
		t.Errorf("map with named keys: %v, %v", m, err)
	}
	var fs []float64
	if err := convert(t, []any{1.5, 2}, &fs); err != nil || !reflect.DeepEqual(fs, []float64{1.5, 2}) {
		t.Errorf("[]float64: %v, %v", fs, err)
	}
	var bs []byte
	if err := convert(t, []any{1, 2}, &bs); err != nil || !bytes.Equal(bs, []byte{1, 2}) {
		t.Errorf("[]byte from an array: %v, %v", bs, err)
	}
	var s myString
	if err := convert(t, "s", &s); err != nil || s != "s" {
		t.Errorf("named string: %v, %v", s, err)
	}
	var a any
	if err := convert(t, map[string]any{"x": 1, "y": []int{2}}, &a); err != nil ||
		!reflect.DeepEqual(a, map[string]any{"x": int64(1), "y": []any{int64(2)}}) {
		t.Errorf("into any: %#v, %v", a, err)
	}
	// A null resets what was there.
	n := &struct{ N *int }{N: new(int)}
	if err := convert(t, nil, &n.N); err != nil || n.N != nil {
		t.Errorf("null into a set pointer: %v, %v", n.N, err)
	}
}

func TestMessageValueErrors(t *testing.T) {
	for name, v := range map[string]any{
		"not a dictionary": 3,
		"nul in key":       map[string]any{"a\x00": 1},
		"nul in string":    struct{ S string }{"a\x00"},
		"unsupported":      struct{ C chan int }{},
		"int keys":         map[string]any{"m": map[int]int{1: 1}},
	} {
		var e encoder
		e.reset(0)
		if err := e.messageValue(v); err == nil {
			t.Errorf("%s: encoding succeeded, want error", name)
		} else if !strings.HasPrefix(err.Error(), "xpc: ") {
			t.Errorf("%s: error %q lacks the xpc prefix", name, err)
		}
	}
}

type Embedded struct{ Deep string }

func TestUnmarshalEmbeddedAndInterfaces(t *testing.T) {
	b := encodeTestMessage(t, map[string]any{"Deep": "promoted", "Stringer": UUID{2}, "Fixed": []any{"a", "b"}, "Small": []byte{4, 5, 6}})
	var out struct {
		*Embedded
		Stringer fmt.Stringer
		Fixed    [2]string
		Small    [3]byte
	}
	if err := decodeTestMessage(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Embedded == nil || out.Deep != "promoted" || out.Stringer != (UUID{2}) ||
		out.Fixed != [2]string{"a", "b"} || out.Small != [3]byte{4, 5, 6} {
		t.Errorf("got %+v", out)
	}
}
