package runclock

import (
	"errors"
	"testing"
	"time"
)

func TestAuthorityMappingSubtractsTheWholeRoundTrip(t *testing.T) {
	// A wildly different host wall clock never enters the calculation.
	observed := time.Date(2048, time.January, 2, 3, 4, 5, 0, time.UTC)
	deadline, err := FromAuthority(1000, 6000, observed, observed.Add(30*time.Second), 30*time.Second)
	if err != nil || deadline != 31000 {
		t.Fatalf("deadline=%d err=%v", deadline, err)
	}
	// Sampling the server later after its row locks cannot reset the already
	// spent transport/lock time when the response is received.
	if _, err := FromAuthority(1000, 31000, observed, observed.Add(30*time.Second), 30*time.Second); !errors.Is(err, ErrExpired) {
		t.Fatalf("equal expiry was accepted: %v", err)
	}
	// Millisecond IPC values must never round 1.9 ms of authority up to 2 ms.
	deadline, err = FromAuthority(1000, 1000, observed, observed.Add(1900*time.Microsecond), time.Second)
	if err != nil || deadline != 1001 {
		t.Fatalf("fractional deadline=%d err=%v", deadline, err)
	}
	if _, err := FromAuthority(1000, 1000, observed, observed.Add(time.Microsecond), time.Second); !errors.Is(err, ErrExpired) {
		t.Fatalf("sub-millisecond grant extended: %v", err)
	}
}

func TestAuthoritySamplesFailClosed(t *testing.T) {
	observed := time.Date(2026, time.September, 16, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		start, end  int64
		at, expires time.Time
		ttl         time.Duration
		want        error
	}{
		{"missing observation", 1, 1, time.Time{}, observed, time.Second, ErrClock},
		{"missing expiry", 1, 1, observed, time.Time{}, time.Second, ErrClock},
		{"negative clock", -1, 0, observed, observed.Add(time.Second), time.Second, ErrClock},
		{"reversed clock", 5, 4, observed, observed.Add(time.Second), time.Second, ErrClock},
		{"unsafe clock", MaxMillis + 1, MaxMillis + 1, observed, observed.Add(time.Second), time.Second, ErrClock},
		{"overflow", MaxMillis - 2, MaxMillis - 1, observed, observed.Add(time.Second), time.Second, ErrClock},
		{"zero authority", 1, 1, observed, observed, time.Second, ErrExpired},
		{"expired authority", 1, 1, observed, observed.Add(-time.Second), time.Second, ErrExpired},
		{"round trip consumed grant", 1, 1002, observed, observed.Add(time.Second), time.Second, ErrExpired},
		{"over registered ttl", 1, 1, observed, observed.Add(31 * time.Second), 30 * time.Second, ErrClock},
		{"zero ttl", 1, 1, observed, observed.Add(time.Second), 0, ErrClock},
		{"unbounded ttl", 1, 1, observed, observed.Add(time.Second), 25 * time.Hour, ErrClock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FromAuthority(tc.start, tc.end, tc.at, tc.expires, tc.ttl); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}
