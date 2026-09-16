//go:build linux

package runclock

import "golang.org/x/sys/unix"

// Now returns boot-relative milliseconds, including suspension, matching
// Python time.clock_gettime_ns(time.CLOCK_BOOTTIME) in the same time namespace.
// Go time.Time's private monotonic component is not this cross-process domain.
func Now() (int64, error) {
	var sample unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &sample); err != nil {
		return 0, ErrClock
	}
	seconds, nanoseconds := int64(sample.Sec), int64(sample.Nsec)
	if seconds < 0 || seconds > MaxMillis/1000 || nanoseconds < 0 || nanoseconds >= 1_000_000_000 {
		return 0, ErrClock
	}
	value := seconds*1000 + nanoseconds/1_000_000
	if value > MaxMillis {
		return 0, ErrClock
	}
	return value, nil
}
