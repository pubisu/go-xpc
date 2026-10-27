//go:build !darwin || !cgo

package xpc

import (
	"context"
	"errors"
)

// Conn is a connection to an XPC service.
type Conn struct{}

// Dial reports [errors.ErrUnsupported]: XPC is only available on Apple
// platforms, with cgo.
func Dial(name string, opts ...Option) (*Conn, error) {
	return nil, errors.ErrUnsupported
}

// Send reports [errors.ErrUnsupported].
func (c *Conn) Send(msg any) error {
	return errors.ErrUnsupported
}

// Call reports [errors.ErrUnsupported].
func (c *Conn) Call(ctx context.Context, req, reply any) error {
	return errors.ErrUnsupported
}

// prepare reports [errors.ErrUnsupported] to the net/rpc codec.
func (c *Conn) prepare(p *RPCProtocol, method string, args any) (func(context.Context) (rpcResult, error), error) {
	return nil, errors.ErrUnsupported
}

// Close reports [errors.ErrUnsupported].
func (c *Conn) Close() error {
	return errors.ErrUnsupported
}

// Done returns a channel that is already closed.
func (c *Conn) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Err reports [errors.ErrUnsupported].
func (c *Conn) Err() error {
	return errors.ErrUnsupported
}
