// Package xpc is a client for Apple's XPC interprocess communication
// framework.
//
// A [Conn] talks to a service registered with launchd. Messages are Go
// structs, or maps with string keys, converted to and from XPC objects:
//
//	Go                     XPC
//	---------------------  ----------------
//	nil, nil pointer       null
//	bool                   bool
//	int, int8 … int64      int64
//	uint, uint8 … uint64   uint64
//	float32, float64       double
//	string                 string
//	[]byte, [N]byte        data
//	DataMarshaler          data
//	time.Time              date
//	UUID                   uuid
//	*os.File               file descriptor
//	map[string]T, struct   dictionary
//	[]T, [N]T              array
//
// Struct fields are encoded under their names; the field tag
// `xpc:"name,omitempty"` renames a field or omits it when zero, and
// `xpc:"-"` skips it. Dictionaries decode into structs, matching keys to
// field names exactly, then case-insensitively, and into maps; numbers
// decode into any numeric type that can hold them. An interface value
// receives nil, bool, int64, uint64, float64, string, []byte, time.Time,
// UUID, *os.File, map[string]any or []any.
//
// [Conn.Call] sends a request and decodes its reply, [Conn.Send] sends a
// one-way message, and [WithMessageHandler] receives the messages the
// service sends on its own.
//
// [Listen] creates an anonymous listener, whose [Endpoint] a message
// carries to a service that then connects back to it, to send events such
// as the progress of a request.
//
// # net/rpc
//
// [NewClientCodec], [NewRPCClient] and [DialRPC] adapt a connection to the
// standard [net/rpc] client, for services described by an [RPCProtocol].
// [RPCError] gives the connection errors of a call, which net/rpc passes as
// text, their type back.
//
// XPC is only available on Apple platforms, with cgo. On other systems, or
// without cgo, the package builds, but [Dial] and [Listen] report
// [errors.ErrUnsupported].
package xpc
