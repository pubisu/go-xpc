package xpc

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/rpc"
	"strings"
)

// RPCProtocol describes how a service lays out calls, for
// [NewClientCodec]: a request holds the arguments' entries and the method
// name under MethodKey, and the reply is the result.
type RPCProtocol struct {
	// MethodKey is the request entry that holds the method name, the
	// serviceMethod of [rpc.Client.Call].
	MethodKey string

	// Error returns the error a reply reports, if any, reading the reply
	// with [Message.Decode]. Its text becomes the [rpc.ServerError] of the
	// call. A nil Error means replies report no errors.
	Error func(reply *Message) error
}

// DialRPC connects to the service name, as [Dial] does, and returns a
// [net/rpc] client that speaks protocol p, as described by
// [NewClientCodec].
func DialRPC(name string, p RPCProtocol, opts ...Option) (*rpc.Client, error) {
	c, err := Dial(name, opts...)
	if err != nil {
		return nil, err
	}
	return NewRPCClient(c, p), nil
}

// NewRPCClient returns a [net/rpc] client that makes calls over c with
// protocol p.
func NewRPCClient(c *Conn, p RPCProtocol) *rpc.Client {
	return rpc.NewClientWithCodec(NewClientCodec(c, p))
}

// NewClientCodec returns an [rpc.ClientCodec] that makes each net/rpc call
// a request on c, laid out as p describes. The arguments of a call must be
// a struct, a map with string keys, a pointer to one, or nil for none. The
// reply is decoded into the call's reply value.
// Closing the codec closes c.
//
// An error reported by p.Error, or an interrupted connection, fails only
// that call, with an [rpc.ServerError]; [RPCError] gives connection errors
// their type back. The client shuts down, failing every pending call, once
// the connection becomes invalid or a reply cannot be decoded into its
// reply value.
func NewClientCodec(c *Conn, p RPCProtocol) rpc.ClientCodec {
	return newClientCodec(c, p)
}

// RPCError returns err, the error of a call made through a codec of this
// package, with the type net/rpc could not carry. A call whose connection
// was interrupted, or whose reply was too large, fails with an
// [rpc.ServerError] holding only the error's text: RPCError returns an
// [*Error] wrapping [ErrConnectionInterrupted], or [ErrMessageTooLarge],
// for it. Other errors are returned as they are. An error of the
// [RPCProtocol] whose text begins as those do cannot be told from them.
func RPCError(err error) error {
	se, ok := err.(rpc.ServerError)
	if !ok {
		return err
	}
	if string(se) == ErrMessageTooLarge.Error() {
		return ErrMessageTooLarge
	}
	// As (*Error).Error writes it.
	desc, ok := strings.CutPrefix(string(se), ErrConnectionInterrupted.Error())
	if !ok {
		return err
	}
	if desc != "" {
		if desc, ok = strings.CutPrefix(desc, ": "); !ok {
			return err
		}
	}
	return &Error{Err: ErrConnectionInterrupted, Description: desc}
}

// requester is the part of *Conn the codec needs.
type requester interface {
	// prepare encodes a request for method with args, so that encoding
	// errors fail the call at once, and returns a function that sends it
	// and waits for the reply. The function must be called exactly once.
	prepare(p *RPCProtocol, method string, args any) (func(ctx context.Context) (rpcResult, error), error)
	Close() error
}

// An rpcResult is a service's reply to a call. It is decoded only once
// the codec knows the value to store it in.
type rpcResult interface {
	// failure returns the error the reply reports, if any.
	failure() (string, bool)
	// decode stores the reply in v, or discards it if v is nil, and frees
	// it. It is called exactly once.
	decode(v any) error
}

// call writes a request: args, which must encode as a dictionary, with
// the entry key: method added.
func (e *encoder) call(args any, key, method string) error {
	if err := e.messageValue(args); err != nil {
		return err
	}
	root := e.base + 8 // the root dictionary's type, then length and count
	end := len(e.buf)
	if err := e.key(key); err != nil {
		return err
	}
	if err := e.string(method); err != nil {
		return err
	}
	n := binary.LittleEndian.Uint32(e.buf[root+4:])
	binary.LittleEndian.PutUint32(e.buf[root+4:], n+uint32(len(e.buf)-end))
	n = binary.LittleEndian.Uint32(e.buf[root+8:])
	binary.LittleEndian.PutUint32(e.buf[root+8:], n+1)
	return nil
}

// A wireResult is a reply kept as the received message until the codec
// knows where it goes, so that it is decoded once, straight into that
// value.
type wireResult struct {
	d         *decoder
	off, port int    // where the reply's dictionary starts
	release   func() // frees the message; nil once freed
	errText   string
	failed    bool
}

// newWireResult takes over the reply d reads, which release frees, and
// checks it for an error with p.
func newWireResult(d *decoder, p *RPCProtocol, release func()) rpcResult {
	r := &wireResult{d: d, off: d.off, port: d.next, release: release}
	if p.Error != nil {
		msg := &Message{off: d.off, next: d.next}
		msg.d.Store(d)
		err := p.Error(msg)
		msg.d.Store(nil)
		if err != nil {
			r.errText, r.failed = err.Error(), true
			if r.errText == "" {
				r.errText = "xpc: empty error"
			}
			d.closeFiles()
			r.free()
		}
	}
	return r
}

func (r *wireResult) failure() (string, bool) {
	return r.errText, r.failed
}

func (r *wireResult) free() {
	if r.release != nil {
		r.release()
		r.release = nil
	}
}

func (r *wireResult) decode(v any) error {
	if r.release == nil {
		return nil
	}
	defer r.free()
	r.d.off, r.d.next = r.off, r.port
	return r.d.decodeMessage(v)
}

// A clientCodec carries net/rpc calls as XPC requests. Each call makes its
// round trip in a goroutine of its own, and the replies reach
// ReadResponseHeader through replies as they arrive.
type clientCodec struct {
	conn    requester
	proto   RPCProtocol
	ctx     context.Context
	cancel  context.CancelFunc
	replies chan rpcReply
	body    rpcResult // of the response whose header was read last
}

// An rpcReply is the outcome of a call, passed from its goroutine to
// ReadResponseHeader.
type rpcReply struct {
	seq    uint64
	method string
	result rpcResult
	err    string // error reported for this call
	fatal  error  // error that ends the client
}

func newClientCodec(conn requester, p RPCProtocol) *clientCodec {
	ctx, cancel := context.WithCancel(context.Background())
	return &clientCodec{
		conn:    conn,
		proto:   p,
		ctx:     ctx,
		cancel:  cancel,
		replies: make(chan rpcReply),
	}
}

// WriteRequest encodes the call and sends it from a goroutine of its own,
// which waits for the reply.
func (c *clientCodec) WriteRequest(r *rpc.Request, args any) error {
	request, err := c.conn.prepare(&c.proto, r.ServiceMethod, args)
	if err != nil {
		return err
	}
	seq, method := r.Seq, r.ServiceMethod
	go func() {
		res, err := request(c.ctx)
		rr := rpcReply{seq: seq, method: method}
		switch {
		case c.ctx.Err() != nil:
			if res != nil {
				res.decode(nil)
			}
			return // closed
		case errors.Is(err, ErrConnectionInvalid), errors.Is(err, ErrClosed):
			rr.fatal = err
		case err != nil:
			rr.err = err.Error()
		default:
			if msg, failed := res.failure(); failed {
				rr.err = msg
			} else {
				rr.result = res
			}
		}
		select {
		case c.replies <- rr:
		case <-c.ctx.Done():
			if rr.result != nil {
				rr.result.decode(nil)
			}
		}
	}()
	return nil
}

// ReadResponseHeader waits for the next reply, of any call.
func (c *clientCodec) ReadResponseHeader(r *rpc.Response) error {
	select {
	case rr := <-c.replies:
		if rr.fatal != nil {
			c.cancel() // release the other in-flight calls
			return rr.fatal
		}
		r.Seq, r.ServiceMethod, r.Error = rr.seq, rr.method, rr.err
		c.body = rr.result
		return nil
	case <-c.ctx.Done():
		return io.EOF
	}
}

// ReadResponseBody decodes the reply whose header was read last into v,
// straight from the received message.
func (c *clientCodec) ReadResponseBody(v any) error {
	body := c.body
	c.body = nil
	if body == nil {
		return nil // an error response: net/rpc passes no value
	}
	// The reply is decoded straight into v.
	return body.decode(v)
}

// Close stops the codec, which fails the calls in flight, and closes the
// connection.
func (c *clientCodec) Close() error {
	c.cancel()
	return c.conn.Close()
}
