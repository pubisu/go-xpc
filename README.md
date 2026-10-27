# go-xpc

A Go client for Apple's [XPC](https://developer.apple.com/documentation/xpc)
interprocess communication framework. It does not use libxpc: it speaks
the XPC protocol over Mach messages itself, with only the Mach and launchd
calls going through cgo.

```sh
go get github.com/pubisu/go-xpc
```

Requires Go 1.27, macOS and cgo. On other platforms, or without cgo, the
package compiles, but `Dial` and `Listen` return `errors.ErrUnsupported`.

## Usage

Messages are Go structs (or maps), encoded straight into the XPC message
and decoded straight out of it, with no intermediate representation:

```go
conn, err := xpc.Dial("com.example.helper") // xpc.Privileged() for system daemons
if err != nil {
	log.Fatal(err)
}
defer conn.Close()

// One-way message.
err = conn.Send(struct {
	Event string `xpc:"event"`
}{"ping"})

// Request and reply, cancellable with a context. Reply entries that no
// field wants are skipped without being decoded.
var status struct {
	Running bool   `xpc:"running"`
	Version string `xpc:"version"`
}
err = conn.Call(ctx, struct {
	Command string `xpc:"command"`
}{"status"}, &status)
```

Messages the service sends on its own initiative go to the handler set
with `xpc.WithMessageHandler`, which decodes each with `msg.Decode(&v)`;
connection errors (such as `xpc.ErrConnectionInterrupted`) go to
`xpc.WithErrorHandler`. `conn.Done()` is closed once the connection
becomes invalid.

A type that implements `xpc.DataMarshaler` and `xpc.DataUnmarshaler`
travels as a data entry in an encoding of its own (JSON, for instance)
and decodes from the message's bytes without a copy.

### Listeners and endpoints

A service can send events back to a client on a connection of its own,
such as the progress of a long request. `xpc.Listen` creates an anonymous
listener, like libxpc's `xpc_connection_create(NULL, NULL)`; its endpoint
travels in a message, and the service connects to it:

```go
l, err := xpc.Listen(xpc.WithMessageHandler(func(msg *xpc.Message) {
	var event struct {
		Done int64 `xpc:"done"`
	}
	if msg.Decode(&event) == nil {
		fmt.Println("progress:", event.Done)
	}
}))
if err != nil {
	log.Fatal(err)
}
defer l.Close()

err = conn.Call(ctx, struct {
	Command  string       `xpc:"command"`
	Progress xpc.Endpoint `xpc:"progress"`
}{"download", l.Endpoint()}, nil)
```

A listener receives one-way messages; requests that wait for a reply get
none. `xpc.WithPeerRequirement` and `xpc.WithMaxMessageSize` apply to its
peers as to a connection's service.

## net/rpc

The package provides an `rpc.ClientCodec`, so the standard `net/rpc` client
works over XPC. XPC services define their own messages; an `RPCProtocol`
says how a service lays out calls: which request entry names the method,
and how a reply reports an error. The arguments' own entries make up the
rest of the request, and the reply dictionary is decoded into the reply
value, both converted straight between Go values and XPC messages.

```go
protocol := xpc.RPCProtocol{
	MethodKey: "op", // the request entry naming the method
	Error: func(reply *xpc.Message) error {
		var r struct {
			Error string `xpc:"error"`
		}
		if err := reply.Decode(&r); err != nil {
			return err
		}
		if r.Error != "" {
			return errors.New(r.Error)
		}
		return nil
	},
}
client, err := xpc.DialRPC("com.example.arith", protocol)
if err != nil {
	log.Fatal(err)
}
defer client.Close()

var reply struct {
	Product int `xpc:"product"`
}
// Sends {"op": "multiply", "a": 7, "b": 8}.
err = client.Call("multiply", struct {
	A int `xpc:"a"`
	B int `xpc:"b"`
}{7, 8}, &reply)
```

An error the protocol reports, or an interrupted connection, fails only
that call, with an `rpc.ServerError`; `xpc.RPCError` gives connection
errors their type back. Once the connection becomes invalid, or a reply
cannot be decoded into its reply value, the client shuts down, as `net/rpc`
treats every error reading a reply as fatal.

## How it works

Messages are encoded straight from Go values into the binary format libxpc
puts on the wire (`CPX@`, version 5) and sent with `mach_msg`; see
`wire.go` for the format. A connection checks in with the service the way
libxpc does, sending it a receive right for the server's side and a send
right for the client's; each request carries a reply port and is sent and
answered in a single `mach_msg` call on the calling goroutine. Data larger
than 16 KB travels in a memory entry, bodies larger than 16 KB out of line,
and file descriptors as fileports, as libxpc does.

The protocol is not documented by Apple; it was taken from libxpc's own
traffic on macOS 27, and an update of macOS could change it.

Compared with the previous libxpc-based implementation, a request round
trip is about 44% faster and typed `net/rpc` calls 28–42% faster.

## Security

Everything the service sends is treated as untrusted:

- The sender of every message is taken from its kernel audit trailer, which
  cannot be forged. Connection notifications are only accepted from the
  kernel, so a process holding a send right to the connection cannot end it
  or make it release port rights.
- `xpc.WithPeerRequirement` checks the process behind each reply and each
  message (PID, UID, GID, audit token), and `xpc.Privileged` only finds
  daemons of the system domain, which a user process cannot register.
- Decoding is bounded: nesting depth, memory reserved ahead of the data,
  and the size of messages and data entries (`xpc.WithMaxMessageSize`,
  64 MiB by default). Every port right and out-of-line region a message
  carries is released, whatever its kind.
- Malformed input is fuzzed (`go test -fuzz FuzzDecode`).

## Package layout

| File | Contents |
| --- | --- |
| `doc.go` | Package documentation |
| `conn_darwin.go`, `conn_other.go` | `Conn`: check-in, `Send`, `Call`, the receive loop; stubs elsewhere |
| `mach_darwin.go`, `mach_darwin.{c,h}` | Mach message layout, attachments, reply ports; thin Mach and launchd calls |
| `wire.go` | The XPC wire format: type tags and limits |
| `encode.go`, `decode.go`, `decode_into.go` | Encoding Go values; decoding to `any` and straight into typed values |
| `fields.go`, `data.go` | Struct field metadata; `DataMarshaler` and `DataUnmarshaler` |
| `message.go` | `Message` for pushed messages |
| `listener.go`, `listener_darwin.go`, `listener_other.go` | `Listener` and `Endpoint`: anonymous listeners that services connect back to |
| `options.go`, `errors.go`, `uuid.go` | Dial options, errors, `UUID` |
| `rpc.go` | The `net/rpc` codec and `RPCProtocol` |

## Testing

On macOS:

```sh
go test -race ./...
```

The wire format codec is portable and fuzzed:

```sh
go test -fuzz FuzzDecode
```

## License

MIT; see [LICENSE](LICENSE).
