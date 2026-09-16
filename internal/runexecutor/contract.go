// Package runexecutor owns the fixed Linux executor's processes and bounded I/O.
// It reports physical facts, never execution authority or a successful Run.
package runexecutor

import (
	"errors"

	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
)

var (
	// ErrUnsupported rejects platforms without the fixed Linux process contract.
	ErrUnsupported = errors.New("executor requires linux")
	// ErrDeadline requires a bounded start or write operation.
	ErrDeadline = errors.New("executor deadline required")
	// ErrEnvironment rejects invalid deployment credentials or origins.
	ErrEnvironment = errors.New("invalid executor environment")
	// ErrStart contains no operating-system diagnostics or child output.
	ErrStart = errors.New("executor start failed")
	// ErrStopped indicates that ordinary execution cannot resume.
	ErrStopped = errors.New("executor stopped")
	// ErrConcurrentWrite rejects a second outstanding write on the same lane.
	ErrConcurrentWrite = errors.New("executor lane write already pending")
	// ErrWrite is a content-free pipe failure.
	ErrWrite = errors.New("executor pipe write failed")
	// ErrMeteringClosed records EPIPE on the advisory metering ACK writer only.
	ErrMeteringClosed = errors.New("executor metering acknowledgement reader closed")
)

// Environment contains only deployment-selected, least-privilege credentials.
// Never format this value: it contains secrets, unlike the process receipt.
type Environment struct {
	BusinessOrigin  string
	BusinessReadKey string
	OllamaOrigin    string
	DeepSeekKey     string
}

// Spec contains internal deployment configuration, never a command selector.
type Spec struct{ Environment Environment }

// Channel identifies one of the two independent protocol lanes.
type Channel uint8

// Protocol lanes have separate readers, writers and bounded queues.
const (
	Ordinary Channel = iota + 1
	Metering
)

// EventKind identifies a decoded frame or a physical lifecycle fact.
type EventKind uint8

// Events carry no raw pipe bytes or exception text.
const (
	FrameReceived EventKind = iota + 1
	ChannelEOF
	ChannelFailed
	DiagnosticFailed
	GuardianExited
)

// Problem is a local diagnostic, not a domain or provider error.
type Problem uint8

// Problems are fixed classifications safe for diagnostic output.
const (
	NoProblem Problem = iota
	InvalidFrame
	FrameTooLarge
	PipeFailure
	StderrTooLarge
	EventDeliveryFailed
	// MeteringWriteClosed is provisional until actual Wait and complete cleanup.
	// It never represents a reader, ordinary writer, or decoding failure.
	MeteringWriteClosed
)

// Exit records actual direct-child Wait, without inferring step success.
type Exit struct {
	Observed bool
	Code     int
	Signaled bool
	Signal   int
}

// Event owns its decoded frame; it never shares mutable data with the runner.
type Event struct {
	Kind    EventKind
	Channel Channel
	Frame   *runprotocol.Frame
	Problem Problem
	Exit    Exit
}

// ChannelReceipt records EOF and actual termination of both lane goroutines.
// A failed decoder may join without having seen a clean EOF.
type ChannelReceipt struct {
	EOF     bool
	Joined  bool
	Problem Problem
}

// Receipt is an immutable snapshot after Done. Missing facts are never success.
type Receipt struct {
	Guardian            Exit
	Ordinary            ChannelReceipt
	Metering            ChannelReceipt
	StderrBytes         int64
	StderrJoined        bool
	GroupGone           bool
	CleanupTimedOut     bool
	EventDeliveryFailed bool
}
