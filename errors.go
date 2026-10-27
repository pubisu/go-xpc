package xpc

import "errors"

var (
	// ErrConnectionInterrupted reports that the service went away: it
	// exited, crashed or dropped the connection. The connection remains
	// usable, as the next message reaches the service again, but any state
	// the service held is gone.
	ErrConnectionInterrupted = errors.New("xpc: connection interrupted")

	// ErrConnectionInvalid reports that the connection can no longer be
	// used, either because the service could not be found or because the
	// connection was closed.
	ErrConnectionInvalid = errors.New("xpc: connection invalid")

	// ErrClosed is returned when using a connection after [Conn.Close].
	ErrClosed = errors.New("xpc: use of closed connection")

	// ErrMessageTooLarge reports a message, or a data entry in one, larger
	// than the connection accepts; see [WithMaxMessageSize].
	ErrMessageTooLarge = errors.New("xpc: message too large")
)

// Error describes the failure of a connection. Err is
// [ErrConnectionInterrupted] or [ErrConnectionInvalid], so that it can be
// tested with [errors.Is]; Description may tell more.
type Error struct {
	Err         error
	Description string
}

// Error returns the text of Err, followed by the description, if any.
func (e *Error) Error() string {
	if e.Description == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + ": " + e.Description
}

// Unwrap returns Err, so that [errors.Is] tells the kind of error.
func (e *Error) Unwrap() error { return e.Err }

// errMalformed reports a received message that does not follow the wire
// format or the Mach message layout.
var errMalformed = errors.New("xpc: malformed message")
