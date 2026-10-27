package xpc

import (
	"context"
	"errors"
	"net/rpc"
	"reflect"
	"sync"
	"testing"
	"time"
)

// testProtocol is the protocol of the services in the tests: the method
// name under "test.method", errors as a string under "test.error".
var testProtocol = RPCProtocol{
	MethodKey: "test.method",
	Error: func(reply *Message) error {
		var r struct {
			Err string `xpc:"test.error"`
		}
		if err := reply.Decode(&r); err != nil {
			return err
		}
		if r.Err != "" {
			return errors.New(r.Err)
		}
		return nil
	},
}

// fakeService stands in for an XPC connection to a service speaking
// testProtocol. Requests and replies go through the wire format, as they
// would through XPC.
type fakeService struct {
	mu     sync.Mutex
	closed bool
	block  chan struct{} // if set, "Slow.Wait" waits for it
}

// serve answers the request req, a message body, for method.
func (s *fakeService) serve(ctx context.Context, method string, req []byte) (any, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	switch method {
	case "Arith.Multiply":
		var args struct{ A, B int }
		if err := decodeTestMessage(req, &args); err != nil {
			return map[string]any{"test.error": err.Error()}, nil
		}
		return product{args.A * args.B}, nil
	case "Arith.Divide":
		return map[string]any{"test.error": "divide by zero"}, nil
	case "Echo.Struct":
		return req, nil // the request itself
	case "Conn.Interrupt":
		return nil, &Error{Err: ErrConnectionInterrupted}
	case "Conn.Invalidate":
		return nil, &Error{Err: ErrConnectionInvalid}
	case "Slow.Wait":
		select {
		case <-s.block:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return map[string]any{"test.error": "unknown method"}, nil
}

func (s *fakeService) prepare(p *RPCProtocol, method string, args any) (func(context.Context) (rpcResult, error), error) {
	var e encoder
	e.reset(0)
	if err := e.call(args, p.MethodKey, method); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (rpcResult, error) {
		reply, err := s.serve(ctx, method, e.buf)
		if err != nil {
			return nil, err
		}
		body, ok := reply.([]byte)
		if !ok {
			var re encoder
			re.reset(0)
			if err := re.messageValue(reply); err != nil {
				return nil, err
			}
			body = re.buf
		}
		d := &decoder{}
		if err := d.init(body, nil, nil); err != nil {
			return nil, err
		}
		return newWireResult(d, p, func() {}), nil
	}, nil
}

func (s *fakeService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func newFakeClient() (*rpc.Client, *fakeService) {
	s := &fakeService{block: make(chan struct{})}
	return rpc.NewClientWithCodec(newClientCodec(s, testProtocol)), s
}

type product struct{ Product int }

func TestRPCCall(t *testing.T) {
	client, _ := newFakeClient()
	defer client.Close()

	var p product
	if err := client.Call("Arith.Multiply", struct{ A, B int }{6, 7}, &p); err != nil {
		t.Fatal(err)
	}
	if p.Product != 42 {
		t.Errorf("product = %d, want 42", p.Product)
	}

	type point struct {
		X, Y int
		Name string `xpc:"name"`
	}
	var pt point
	if err := client.Call("Echo.Struct", point{1, 2, "p"}, &pt); err != nil {
		t.Fatal(err)
	}
	if pt != (point{1, 2, "p"}) {
		t.Errorf("echoed %+v", pt)
	}
}

// TestRPCRequestLayout checks that a request is the arguments' entries
// next to the method name, with nothing else.
func TestRPCRequestLayout(t *testing.T) {
	for _, tc := range []struct {
		args any
		want map[string]any
	}{
		{struct{ A, B int }{1, 2}, map[string]any{"test.method": "M", "A": int64(1), "B": int64(2)}},
		{map[string]any{"k": "v"}, map[string]any{"test.method": "M", "k": "v"}},
		{nil, map[string]any{"test.method": "M"}},
	} {
		var e encoder
		e.reset(0)
		if err := e.call(tc.args, "test.method", "M"); err != nil {
			t.Fatal(err)
		}
		var d decoder
		if err := d.init(e.buf, nil, nil); err != nil {
			t.Fatal(err)
		}
		got, err := d.value()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("request for %#v = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestRPCServerError(t *testing.T) {
	client, _ := newFakeClient()
	defer client.Close()

	var p product
	err := client.Call("Arith.Divide", nil, &p)
	if _, ok := err.(rpc.ServerError); !ok || err.Error() != "divide by zero" {
		t.Errorf("err = %#v, want ServerError(divide by zero)", err)
	}
	if RPCError(err) != err {
		t.Errorf("RPCError changed the protocol's error %#v", err)
	}
	err = client.Call("Conn.Interrupt", nil, &p)
	if _, ok := err.(rpc.ServerError); !ok {
		t.Errorf("interrupted call: err = %#v, want a ServerError", err)
	}
	if err := RPCError(err); !errors.Is(err, ErrConnectionInterrupted) {
		t.Errorf("RPCError of an interrupted call = %#v", err)
	}
	// The client is still usable after per-call errors.
	if err := client.Call("Arith.Multiply", struct{ A, B int }{2, 3}, &p); err != nil || p.Product != 6 {
		t.Errorf("Multiply = %d, %v", p.Product, err)
	}
}

func TestRPCEncodeError(t *testing.T) {
	client, _ := newFakeClient()
	defer client.Close()

	var p product
	if err := client.Call("Arith.Multiply", struct{ C chan int }{}, &p); err == nil {
		t.Error("call with unencodable args succeeded")
	}
	if err := client.Call("Arith.Multiply", 3, &p); err == nil {
		t.Error("call with arguments that are not a dictionary succeeded")
	}
}

func TestRPCDecodeError(t *testing.T) {
	client, _ := newFakeClient()
	defer client.Close()

	var reply struct{ Product string }
	if err := client.Call("Arith.Multiply", struct{ A, B int }{2, 3}, &reply); err == nil {
		t.Error("decoding an int into a string succeeded")
	}
}

func TestRPCInvalidConnection(t *testing.T) {
	client, s := newFakeClient()
	defer client.Close()

	slow := client.Go("Slow.Wait", nil, nil, nil)
	err := client.Call("Conn.Invalidate", nil, nil)
	if !errors.Is(err, ErrConnectionInvalid) {
		t.Errorf("err = %v, want ErrConnectionInvalid", err)
	}
	select {
	case <-slow.Done:
		if !errors.Is(slow.Error, ErrConnectionInvalid) {
			t.Errorf("pending call err = %v, want ErrConnectionInvalid", slow.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call not failed")
	}
	if err := client.Call("Arith.Multiply", struct{ A, B int }{1, 1}, new(product)); !errors.Is(err, rpc.ErrShutdown) {
		t.Errorf("call after shutdown = %v, want ErrShutdown", err)
	}
	close(s.block)
}

func TestRPCClose(t *testing.T) {
	client, s := newFakeClient()

	slow := client.Go("Slow.Wait", nil, nil, nil)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-slow.Done:
		if !errors.Is(slow.Error, rpc.ErrShutdown) {
			t.Errorf("pending call err = %v, want ErrShutdown", slow.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call not failed")
	}
	if !s.closed {
		t.Error("connection not closed")
	}
}

// TestRPCError checks that RPCError reads back the text the codec gives
// connection errors, and only that.
func TestRPCError(t *testing.T) {
	interrupted := &Error{Err: ErrConnectionInterrupted, Description: "peer died"}
	for _, tc := range []struct {
		err  error
		want error // nil: err itself
	}{
		{rpc.ServerError(interrupted.Error()), interrupted},
		{rpc.ServerError(ErrConnectionInterrupted.Error()), &Error{Err: ErrConnectionInterrupted}},
		{rpc.ServerError(ErrMessageTooLarge.Error()), ErrMessageTooLarge},
		{rpc.ServerError("xpc: connection interrupted!"), nil},
		{rpc.ServerError("divide by zero"), nil},
		{ErrConnectionInvalid, nil},
		{nil, nil},
	} {
		want := tc.want
		if want == nil {
			want = tc.err
		}
		if got := RPCError(tc.err); !reflect.DeepEqual(got, want) {
			t.Errorf("RPCError(%#v) = %#v, want %#v", tc.err, got, want)
		}
	}
}
