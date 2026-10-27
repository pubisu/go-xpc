//go:build darwin && cgo

package xpc

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// listen returns a listener whose messages, decoded as maps, arrive on the
// returned channel.
func listen(t *testing.T, opts ...Option) (*Listener, <-chan map[string]any) {
	t.Helper()
	ch := make(chan map[string]any, 16)
	opts = append(opts, WithMessageHandler(func(m *Message) {
		var v map[string]any
		if err := m.Decode(&v); err != nil {
			t.Errorf("Decode: %v", err)
		}
		ch <- v
	}))
	l, err := Listen(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		l.Close()
		<-l.Done()
	})
	return l, ch
}

func receive(t *testing.T, ch <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
		return nil
	}
}

// peers returns the number of connections the listener holds.
func (l *Listener) peerCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.peers)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestListenerMessages(t *testing.T) {
	l, ch := listen(t)
	c := dialEndpoint(l.Endpoint())

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	big := make([]byte, maxInlineData+1)
	if err := c.Send(map[string]any{"n": 1, "s": "x", "f": w, "big": big}); err != nil {
		t.Fatal(err)
	}
	v := receive(t, ch)
	if v["n"] != int64(1) || v["s"] != "x" || len(v["big"].([]byte)) != len(big) {
		t.Errorf("message = %v", v)
	}
	f := v["f"].(*os.File)
	if _, err := f.WriteString("hi"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	w.Close()
	if b, _ := io.ReadAll(r); string(b) != "hi" {
		t.Errorf("read %q through the received file", b)
	}

	// A second connection is a peer of its own.
	c2 := dialEndpoint(l.Endpoint())
	if err := c2.Send(map[string]any{"from": 2}); err != nil {
		t.Fatal(err)
	}
	receive(t, ch)
	if n := l.peerCount(); n != 2 {
		t.Errorf("%d peers, want 2", n)
	}

	// Peers that close are released.
	c.Close()
	c2.Close()
	waitFor(t, "the peers to be released", func() bool { return l.peerCount() == 0 })
}

// TestListenerNoReply checks that a request, which waits for a reply, gets
// none: its caller sees the connection interrupted.
func TestListenerNoReply(t *testing.T) {
	l, ch := listen(t)
	c := dialEndpoint(l.Endpoint())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, map[string]any{"q": 1}, nil); !errors.Is(err, ErrConnectionInterrupted) {
		t.Errorf("Call = %v, want ErrConnectionInterrupted", err)
	}
	receive(t, ch)
}

func TestListenerClose(t *testing.T) {
	l, ch := listen(t)
	c := dialEndpoint(l.Endpoint())
	defer c.Close()
	if err := c.Send(map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	receive(t, ch)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	<-l.Done()
	if err := l.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close = %v, want ErrClosed", err)
	}
	// The connection notices that the listener went away; it cannot
	// connect again.
	waitFor(t, "sends to fail", func() bool { return c.Send(map[string]any{"a": 2}) != nil })
	if _, err := l.makeSend(); !errors.Is(err, errListenerClosed) {
		t.Errorf("endpoint of a closed listener: %v", err)
	}
}

// TestEndpointTravels sends the endpoint of one listener to another: the
// message carries a send right to the first listener's port.
func TestEndpointTravels(t *testing.T) {
	a, _ := listen(t)
	ports := make(chan []uint32, 1)
	b, err := Listen(WithMessageHandler(func(m *Message) {
		ports <- append([]uint32(nil), m.d.Load().ports...)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c := dialEndpoint(b.Endpoint())
	defer c.Close()
	if err := c.Send(struct {
		E Endpoint `xpc:"endpoint"`
	}{a.Endpoint()}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-ports:
		// Within one task, a send right has the name of its receive right.
		if len(p) != 1 || p[0] != a.port {
			t.Errorf("endpoint ports %v, want [%d]", p, a.port)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}

	a.Close()
	<-a.Done()
	if err := c.Send(map[string]any{"e": a.Endpoint()}); !errors.Is(err, errListenerClosed) {
		t.Errorf("Send with a closed listener's endpoint = %v", err)
	}
	if err := c.Send(map[string]any{"e": Endpoint{}}); err == nil {
		t.Error("a zero Endpoint encoded")
	}
}

func TestListenerPeerRequirement(t *testing.T) {
	errs := make(chan error, 4)
	refused := errors.New("refused")
	l, ch := listen(t,
		WithPeerRequirement(func(p Peer) error {
			if p.PID != os.Getpid() {
				t.Errorf("peer PID %d, want %d", p.PID, os.Getpid())
			}
			return refused
		}),
		WithErrorHandler(func(err error) { errs <- err }))
	c := dialEndpoint(l.Endpoint())
	defer c.Close()
	c.Send(map[string]any{"a": 1})
	select {
	case err := <-errs:
		if !errors.Is(err, refused) {
			t.Errorf("error handler got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("check-in not refused")
	}
	select {
	case v := <-ch:
		t.Errorf("refused peer's message delivered: %v", v)
	case <-time.After(100 * time.Millisecond):
	}
	if n := l.peerCount(); n != 0 {
		t.Errorf("%d peers after a refused check-in", n)
	}
}

func TestListenerMaxMessageSize(t *testing.T) {
	errs := make(chan error, 4)
	l, ch := listen(t, WithMaxMessageSize(1024), WithErrorHandler(func(err error) { errs <- err }))
	c := dialEndpoint(l.Endpoint())
	defer c.Close()
	if err := c.Send(map[string]any{"a": make([]byte, 4096)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, ErrMessageTooLarge) {
			t.Errorf("error handler got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("large message not refused")
	}
	if err := c.Send(map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if v := receive(t, ch); v["a"] != int64(1) {
		t.Errorf("message after the large one = %v", v)
	}
}

// TestDeadSession checks that a session whose service died with a request
// pending ends at once: the next message checks in again, rather than fail
// on the dead port.
func TestDeadSession(t *testing.T) {
	// A listener that never runs: check-ins queue on its port, which the
	// test destroys, as a service that dies.
	p := newPort()
	l := &Listener{port: p, peers: map[uint32]uint32{}, done: make(chan struct{})}
	// The receive loop handles the dead-name notification only once the
	// test is over, as on a busy machine.
	release := make(chan struct{})
	hook := func() { <-release }
	deadNameHook.Store(&hook)
	defer deadNameHook.Store(nil)
	defer close(release)
	c := dialEndpoint(l.Endpoint())
	defer c.Close()
	if err := c.Send(map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), map[string]any{"b": 2}, nil) }()
	time.Sleep(50 * time.Millisecond)
	l.mu.Lock()
	l.closed = true // the service cannot be reached again
	l.mu.Unlock()
	destroyPort(p) // and with the check-in, the session's port and the request
	select {
	case err := <-done:
		if !errors.Is(err, ErrConnectionInterrupted) {
			t.Fatalf("Call = %v, want ErrConnectionInterrupted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Call did not return")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, map[string]any{"c": 3}, nil); !errors.Is(err, ErrConnectionInvalid) {
		t.Errorf("next Call = %v, want ErrConnectionInvalid: it must check in again", err)
	}
}

// TestDeadIdleSession checks the same for a service that died while no
// request was pending.
func TestDeadIdleSession(t *testing.T) {
	p := newPort()
	l := &Listener{port: p, peers: map[uint32]uint32{}, done: make(chan struct{})}
	release := make(chan struct{})
	hook := func() { <-release }
	deadNameHook.Store(&hook)
	defer deadNameHook.Store(nil)
	defer close(release)
	errs := make(chan error, 4)
	c := dialEndpoint(l.Endpoint(), WithErrorHandler(func(err error) { errs <- err }))
	defer c.Close()
	if err := c.Send(map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	destroyPort(p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, map[string]any{"b": 2}, nil); !errors.Is(err, ErrConnectionInvalid) {
		t.Errorf("Call = %v, want ErrConnectionInvalid: it must check in again", err)
	}
	if err := <-errs; !errors.Is(err, ErrConnectionInterrupted) {
		t.Errorf("error handler got %v first, want ErrConnectionInterrupted", err)
	}
}

// TestListenerTooLargeKeepsOthers checks that a message too large to
// receive is dropped from its own peer's port, and that the messages other
// peers queued behind it arrive.
func TestListenerTooLargeKeepsOthers(t *testing.T) {
	gate := make(chan struct{})
	errs := make(chan error, 16)
	ch := make(chan map[string]any, 16)
	l, err := Listen(WithMaxMessageSize(1024), WithErrorHandler(func(err error) { errs <- err }),
		WithMessageHandler(func(m *Message) {
			var v map[string]any
			if err := m.Decode(&v); err != nil {
				t.Errorf("Decode: %v", err)
			}
			if _, ok := v["wait"]; ok {
				<-gate
			}
			ch <- v
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		l.Close()
		<-l.Done()
	}()
	a := dialEndpoint(l.Endpoint())
	b := dialEndpoint(l.Endpoint())
	defer a.Close()
	defer b.Close()
	for _, c := range []*Conn{a, b} {
		if err := c.Send(map[string]any{"hello": 1}); err != nil {
			t.Fatal(err)
		}
		receive(t, ch)
	}
	// While the handler waits, a's large message queues, then b's.
	if err := a.Send(map[string]any{"wait": 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := a.Send(map[string]any{"big": make([]byte, 4096)}); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(map[string]any{"b": 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	receive(t, ch) // the waiting message
	if v := receive(t, ch); v["b"] != int64(1) {
		t.Errorf("message after the large one = %v, want b's", v)
	}
	if err := <-errs; !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("error handler got %v", err)
	}
}

// TestListenerCloseFullQueue checks that Close returns, and the listener
// stops, while the listener's port has a full queue and the handler is
// busy: Close must not wait for room in the queue while it holds the lock
// the receive loop takes next.
func TestListenerCloseFullQueue(t *testing.T) {
	gate := make(chan struct{})
	first := true
	l, err := Listen(WithMessageHandler(func(*Message) {
		if first {
			first = false
			<-gate
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := dialEndpoint(l.Endpoint())
	defer c.Close()
	if err := c.Send(map[string]any{"n": 1}); err != nil { // the handler waits on it
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := c.Send(map[string]any{"n": 2}); err != nil { // queued on the peer's port
		t.Fatal(err)
	}
	for i := range 5 { // fills the queue of the listener's port
		if err := sendRaw(l.port, 1, []byte{byte(i), 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-l.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not stop")
	}
}
