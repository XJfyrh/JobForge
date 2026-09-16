//go:build !linux

package runclock

// Now requires the same Linux clock domain as the supervised Python process.
func Now() (int64, error) { return 0, ErrUnsupported }
