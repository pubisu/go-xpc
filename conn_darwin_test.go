//go:build darwin && cgo

package xpc

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// missingService is a launchd name no service is registered under.
const missingService = "dev.pubisu.go-xpc.does-not-exist"

func TestDialMissingService(t *testing.T) {
	c, err := Dial(missingService)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, map[string]any{}, nil); !errors.Is(err, ErrConnectionInvalid) {
		t.Errorf("Call = %v, want ErrConnectionInvalid", err)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connection not invalidated")
	}
}

func TestMissingServiceReportsOnce(t *testing.T) {
	var events []error
	c, _ := Dial(missingService, WithErrorHandler(func(err error) { events = append(events, err) }))
	c.Call(context.Background(), map[string]any{}, nil)
	c.Close()
	if len(events) != 1 || !errors.Is(events[0], ErrConnectionInvalid) {
		t.Errorf("error handler got %v, want one ErrConnectionInvalid", events)
	}
}

// TestEncodeErrors checks that messages that cannot be encoded fail before
// any connection is made.
func TestEncodeErrors(t *testing.T) {
	c, _ := Dial(missingService)
	defer c.Close()

	for name, msg := range map[string]map[string]any{
		"nul in key":       {"a\x00b": 1},
		"nul in string":    {"a": "x\x00y"},
		"unsupported type": {"a": make(chan int)},
		"non-string keys":  {"a": map[int]int{1: 1}},
		"nested":           {"a": []any{map[string]any{"b": func() {}}}},
	} {
		if err := c.Send(msg); err == nil || errors.Is(err, ErrConnectionInvalid) {
			t.Errorf("%s: Send = %v, want an encoding error", name, err)
		}
	}
	if c.Err() != nil {
		t.Errorf("Err() = %v: a connection was attempted", c.Err())
	}
}

func TestCallContextCanceled(t *testing.T) {
	c, _ := Dial(missingService)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Call(ctx, map[string]any{}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Call = %v, want context.Canceled", err)
	}
}

func TestDialRejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "com.example\x00.other", strings.Repeat("a", 128)} {
		if _, err := Dial(name); err == nil {
			t.Errorf("Dial(%q) succeeded", name)
		}
	}
	if _, err := Dial(strings.Repeat("a", 127)); err != nil {
		t.Errorf("Dial of a 127-byte name: %v", err)
	}
}

// TestForgedNotificationsIgnored checks that a dead-name notification sent
// by a process rather than by the kernel neither interrupts the connection
// nor releases the name it carries.
func TestForgedNotificationsIgnored(t *testing.T) {
	const deadName = 0x48 // MACH_NOTIFY_DEAD_NAME
	errc := make(chan error, 4)
	handled := make(chan struct{}, 1)
	c, err := Dial(missingService,
		WithErrorHandler(func(err error) { errc <- err }),
		WithMessageHandler(func(*Message) { handled <- struct{}{} }))
	if err != nil {
		t.Fatal(err)
	}
	// A connection as a check-in leaves it, without a service.
	s := &session{srv: newPort(), refs: 1}
	c.mu.Lock()
	c.cli, c.sess, c.receiving = newPort(), s, true
	c.mu.Unlock()
	go c.receive(c.cli)

	body := make([]byte, 12) // an NDR record, then the name
	binary.LittleEndian.PutUint32(body[8:], s.srv)
	if err := sendRaw(c.cli, deadName, body); err != nil {
		t.Fatal(err)
	}
	if err := sendRaw(c.cli, deadName, nil); err != nil { // and a short one
		t.Fatal(err)
	}
	// Messages are received in order: once this one is handled, the
	// forgeries were dealt with.
	var e encoder
	e.reset(0)
	e.messageValue(nil)
	if err := sendRaw(c.cli, machMessageID, e.buf); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("message not handled")
	}
	select {
	case err := <-errc:
		t.Errorf("forged notification reported %v", err)
	default:
	}
	c.mu.Lock()
	current := c.sess
	c.mu.Unlock()
	if current != s {
		t.Error("forged notification ended the session")
	}
	c.Close()
	<-c.Done()
}
