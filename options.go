package xpc

// Option configures a connection created by [Dial].
type Option func(*options)

// options are the settings of a connection, set by [Option]s.
type options struct {
	privileged      bool
	messageHandler  func(*Message)
	errorHandler    func(error)
	peerRequirement func(Peer) error
	maxMessageSize  int
}

// defaultMaxMessageSize is the largest message accepted by default.
const defaultMaxMessageSize = 64 << 20

// maxMessage returns the largest message, or data entry in one, accepted.
func (o *options) maxMessage() int {
	if o.maxMessageSize > 0 {
		return o.maxMessageSize
	}
	return defaultMaxMessageSize
}

// Privileged requires the service to be a privileged daemon registered in
// the system launchd domain rather than a per-user agent.
func Privileged() Option {
	return func(o *options) {
		o.privileged = true
	}
}

// WithMessageHandler sets a function that receives the messages the
// service sends on its own initiative, rather than in reply to
// [Conn.Call]; fn decodes each with [Message.Decode]. Messages are handled
// one at a time, and fn must not block for long.
func WithMessageHandler(fn func(msg *Message)) Option {
	return func(o *options) {
		o.messageHandler = fn
	}
}

// WithErrorHandler sets a function that receives connection-level errors,
// such as [ErrConnectionInterrupted], and errors decoding the messages the
// service sends on its own.
func WithErrorHandler(fn func(err error)) Option {
	return func(o *options) {
		o.errorHandler = fn
	}
}

// WithPeerRequirement sets a function that checks the process that sends
// each reply and each message the connection receives. A reply it rejects
// fails its call with the error fn returns; a message it rejects is
// dropped, and the error goes to the error handler.
func WithPeerRequirement(fn func(peer Peer) error) Option {
	return func(o *options) { o.peerRequirement = fn }
}

// WithMaxMessageSize sets the size of the largest message, and of the
// largest data entry in a message, that the connection accepts; larger
// ones fail with [ErrMessageTooLarge]. The default is 64 MiB.
func WithMaxMessageSize(n int) Option {
	return func(o *options) { o.maxMessageSize = n }
}
