//go:build !darwin || !cgo

package xpc

import "errors"

// A Listener is an anonymous XPC service in this process.
type Listener struct{}

// Listen reports [errors.ErrUnsupported]: XPC is only available on Apple
// platforms, with cgo.
func Listen(opts ...Option) (*Listener, error) {
	return nil, errors.ErrUnsupported
}

// Endpoint returns the listener's endpoint.
func (l *Listener) Endpoint() Endpoint {
	return Endpoint{l: l}
}

// Close reports [errors.ErrUnsupported].
func (l *Listener) Close() error {
	return errors.ErrUnsupported
}

// Done returns a channel that is already closed.
func (l *Listener) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
