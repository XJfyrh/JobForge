//go:build !linux

package runclock

import (
	"errors"
	"testing"
)

func TestNativeNonLinuxClockIsExplicitlyUnsupported(t *testing.T) {
	if _, err := Now(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported platform substituted another clock: %v", err)
	}
}
