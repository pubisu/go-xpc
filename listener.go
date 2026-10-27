package xpc

// An Endpoint names a [Listener] in a message: it encodes as an XPC
// endpoint, from which the process that receives it can connect to the
// listener, as libxpc's xpc_connection_create_from_endpoint does. Get one
// with [Listener.Endpoint].
type Endpoint struct {
	l *Listener
}
