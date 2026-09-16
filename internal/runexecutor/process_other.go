//go:build !linux

package runexecutor

import (
	"context"

	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
)

// Process is unavailable natively outside Linux. Start never constructs it.
type Process struct{}

// Start rejects non-Linux platforms instead of substituting another clock or
// process-ownership model. Windows acceptance uses the fixed Linux container.
func Start(context.Context, Spec) (*Process, error) { return nil, ErrUnsupported }

// Events cannot produce events on an unsupported platform.
func (*Process) Events() <-chan Event { return nil }

// WriteOrdinary rejects an unsupported platform.
func (*Process) WriteOrdinary(context.Context, runprotocol.Frame) error { return ErrUnsupported }

// WriteMetering rejects an unsupported platform.
func (*Process) WriteMetering(context.Context, runprotocol.Frame) error { return ErrUnsupported }

// Stop has no process to stop on an unsupported platform.
func (*Process) Stop() {}

// Done is already closed on an unsupported platform.
func (*Process) Done() <-chan struct{} { done := make(chan struct{}); close(done); return done }

// Wait reports no observed exit, EOF, Join or group disappearance.
func (*Process) Wait() Receipt { return Receipt{} }
