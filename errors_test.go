package xpc

import (
	"errors"
	"testing"
)

func TestError(t *testing.T) {
	err := error(&Error{Err: ErrConnectionInvalid, Description: "Connection invalid"})
	if !errors.Is(err, ErrConnectionInvalid) {
		t.Errorf("errors.Is(%v, ErrConnectionInvalid) = false", err)
	}
	if got, want := err.Error(), "xpc: connection invalid: Connection invalid"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got, want := (&Error{Err: ErrConnectionInterrupted}).Error(), ErrConnectionInterrupted.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
