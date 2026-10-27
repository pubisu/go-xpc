package xpc

import (
	"fmt"
	"net/rpc"
	"testing"
)

type benchItem struct {
	ID    int64    `xpc:"id"`
	Name  string   `xpc:"name"`
	Score float64  `xpc:"score"`
	Tags  []string `xpc:"tags"`
}

type benchDoc struct {
	Title string
	Count int
	Items []benchItem
	Meta  map[string]string
}

func newBenchDoc(n int) benchDoc {
	d := benchDoc{Title: "benchmark document", Count: n, Meta: map[string]string{"owner": "bench", "kind": "doc"}}
	for i := range n {
		d.Items = append(d.Items, benchItem{ID: int64(i), Name: fmt.Sprintf("item-%d", i), Score: float64(i) / 3, Tags: []string{"a", "b"}})
	}
	return d
}

// benchMessage is a typical small request.
func benchMessage() map[string]any {
	return map[string]any{
		"method": "Service.Method",
		"id":     int64(12345),
		"flag":   true,
		"ratio":  0.75,
		"name":   "a reasonably sized string value",
		"blob":   make([]byte, 256),
		"tags":   []any{"a", "b", "c", "d"},
		"nested": map[string]any{"x": int64(1), "y": int64(2), "label": "point"},
	}
}

// benchLargeMessage is an array of 1000 small dictionaries.
func benchLargeMessage() map[string]any {
	items := make([]any, 1000)
	for i := range items {
		items[i] = map[string]any{"id": int64(i), "name": fmt.Sprintf("item-%d", i), "ok": i%2 == 0}
	}
	return map[string]any{"items": items}
}

func BenchmarkEncodeDocument(b *testing.B) {
	for _, n := range []int{10, 1000} {
		doc := newBenchDoc(n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var e encoder
			b.ReportAllocs()
			for b.Loop() {
				e.reset(0)
				if err := e.messageValue(doc); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecodeDocument(b *testing.B) {
	for _, n := range []int{10, 1000} {
		var e encoder
		e.reset(0)
		if err := e.messageValue(newBenchDoc(n)); err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var dec decoder
			b.ReportAllocs()
			for b.Loop() {
				var doc benchDoc
				if err := dec.init(e.buf, nil, nil); err != nil {
					b.Fatal(err)
				}
				if err := dec.decodeMessage(&doc); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRPCCodec measures the codec's own overhead over an in-memory
// service.
func BenchmarkRPCCodec(b *testing.B) {
	client := rpc.NewClientWithCodec(newClientCodec(&fakeService{}, testProtocol))
	defer client.Close()
	args := struct{ A, B int }{6, 7}
	b.ReportAllocs()
	for b.Loop() {
		var p product
		if err := client.Call("Arith.Multiply", args, &p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	for name, msg := range map[string]map[string]any{"small": benchMessage(), "large": benchLargeMessage()} {
		b.Run(name, func(b *testing.B) {
			var e encoder
			b.ReportAllocs()
			for b.Loop() {
				e.reset(24) // room for a Mach message header
				if err := e.messageValue(msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	for name, msg := range map[string]map[string]any{"small": benchMessage(), "large": benchLargeMessage()} {
		var e encoder
		e.reset(0)
		if err := e.messageValue(msg); err != nil {
			b.Fatal(err)
		}
		b.Run(name, func(b *testing.B) {
			var d decoder
			b.ReportAllocs()
			for b.Loop() {
				if err := d.init(e.buf, nil, nil); err != nil {
					b.Fatal(err)
				}
				if _, err := d.value(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
