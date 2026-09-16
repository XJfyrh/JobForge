//go:build linux

package runexecutor

import (
	"context"
	"sync"
	"sync/atomic"

	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
)

type writeRequest struct {
	data []byte
	done chan error
}

type laneWriter struct {
	busy  atomic.Bool
	queue chan writeRequest
}

// Process owns one guardian, its fixed child, and every local pipe endpoint.
// The caller must drain Events through closure before interpreting Wait.
type Process struct {
	pid        int
	events     chan Event
	done       chan struct{}
	stop       chan struct{}
	closed     chan struct{}
	abort      chan struct{}
	stopping   atomic.Bool
	closedOnce sync.Once
	ordinary   laneWriter
	metering   laneWriter

	// Receipt updates precede event delivery, so backpressure cannot lose facts.
	mu    sync.Mutex
	facts Receipt
	final Receipt
	// The delivery lock prevents a late blocked Wait/read from sending to a
	// closed channel even when the kernel prevents a timely goroutine Join.
	delivery     sync.RWMutex
	eventsClosed bool
}

// Events returns the sole bounded event stream. Its capacity is eight.
func (p *Process) Events() <-chan Event { return p.events }

// Stop irreversibly stops ordinary writes and wakes the independent terminator.
// It never waits on a mutex, pipe, event consumer, RPC or child process.
func (p *Process) Stop() {
	if p.stopping.CompareAndSwap(false, true) {
		close(p.stop)
	}
}

// Done closes after bounded cleanup and publication of the final receipt.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait returns a stable copy. It does not cancel or replace actual process Wait.
func (p *Process) Wait() Receipt {
	<-p.done
	return p.final
}

// WriteOrdinary sends only the fixed Go-to-step ordinary frame kinds.
func (p *Process) WriteOrdinary(ctx context.Context, f runprotocol.Frame) error {
	return p.write(ctx, Ordinary, f)
}

// WriteMetering sends only narrow acknowledgements, including during TERM grace.
func (p *Process) WriteMetering(ctx context.Context, f runprotocol.Frame) error {
	return p.write(ctx, Metering, f)
}

func (p *Process) write(ctx context.Context, channel Channel, f runprotocol.Frame) error {
	if _, ok := ctx.Deadline(); !ok {
		return ErrDeadline
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w := &p.ordinary
	if channel == Metering {
		w = &p.metering
	}
	if !w.busy.CompareAndSwap(false, true) {
		return ErrConcurrentWrite
	}
	defer w.busy.Store(false)
	if p.writeStopped(channel) {
		return ErrStopped
	}
	if !outbound(channel, f.Kind) {
		return runprotocol.ErrProtocol
	}
	// Encode copies all mutable frame content before the sole writer sees it.
	data, err := runprotocol.Encode(f)
	if err != nil {
		return err
	}
	req := writeRequest{data: data, done: make(chan error, 1)}
	select {
	case w.queue <- req:
	case <-p.closed:
		return ErrStopped
	case <-ctx.Done():
		p.Stop()
		return ctx.Err()
	}
	select {
	case err = <-req.done:
		return err
	case <-p.closed:
		return ErrStopped
	case <-ctx.Done():
		// A partial frame may have reached the child. Cancellation always stops
		// the process; the lane cannot be reused as ordinary execution.
		p.Stop()
		// Retain the one-call slot until the writer completes or closure makes
		// every further call impossible. There is no detached queue of writers.
		select {
		case <-req.done:
		case <-p.closed:
		}
		return ctx.Err()
	}
}

func (p *Process) writeStopped(channel Channel) bool {
	select {
	case <-p.closed:
		return true
	default:
	}
	if channel == Ordinary {
		return p.stopping.Load()
	}
	return false
}

func (p *Process) emit(event Event) {
	p.delivery.RLock()
	defer p.delivery.RUnlock()
	if p.eventsClosed {
		return
	}
	select {
	case p.events <- event:
	case <-p.abort:
		p.mu.Lock()
		p.facts.EventDeliveryFailed = true
		p.mu.Unlock()
	}
}

func (p *Process) problem(channel Channel, problem Problem) {
	p.mu.Lock()
	defer p.mu.Unlock()
	receipt := &p.facts.Ordinary
	if channel == Metering {
		receipt = &p.facts.Metering
	}
	if receipt.Problem == NoProblem || receipt.Problem == MeteringWriteClosed {
		receipt.Problem = problem
	}
}
