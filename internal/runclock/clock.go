// Package runclock maps database execution authority into a shared Linux
// CLOCK_BOOTTIME millisecond domain. Host wall clocks never grant more time.
package runclock

import (
	"errors"
	"time"
)

// MaxMillis is the wire protocol's largest exactly representable integer.
const MaxMillis int64 = 1<<53 - 1

var (
	// ErrClock rejects missing, overflowing or inconsistent authority samples.
	ErrClock = errors.New("invalid execution clock sample")
	// ErrExpired rejects a deadline already reached, including equality.
	ErrExpired = errors.New("execution deadline expired")
	// ErrUnsupported prevents native non-Linux workers from mixing clock domains.
	ErrUnsupported = errors.New("execution clock requires linux")
)

// FromAuthority anchors remaining DB authority at the RPC start, not receipt.
// The server samples observedAt after its blocking locks. Subtracting the entire
// round trip is deliberately conservative; network or lock delay cannot renew a
// permission. maxTTL is the registered bound for this particular permission.
func FromAuthority(startMS, receivedMS int64, observedAt, expiresAt time.Time, maxTTL time.Duration) (int64, error) {
	if startMS < 0 || receivedMS < startMS || receivedMS > MaxMillis ||
		observedAt.IsZero() || expiresAt.IsZero() || maxTTL <= 0 || maxTTL > 24*time.Hour {
		return 0, ErrClock
	}
	remaining := expiresAt.Sub(observedAt)
	if remaining <= 0 {
		return 0, ErrExpired
	}
	if remaining > maxTTL {
		return 0, ErrClock
	}
	// Floor fractional milliseconds: rounding upward could extend a lease.
	deltaMS := remaining.Milliseconds()
	if deltaMS > MaxMillis-startMS {
		return 0, ErrClock
	}
	deadlineMS := startMS + deltaMS
	if receivedMS >= deadlineMS {
		return 0, ErrExpired
	}
	return deadlineMS, nil
}
