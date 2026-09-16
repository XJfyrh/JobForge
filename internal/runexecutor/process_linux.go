//go:build linux

package runexecutor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
)

const (
	terminationGrace = 100 * time.Millisecond
	cleanupLimit     = 2 * time.Second
	stderrLimit      = 8192
)

type processPipes struct {
	ordinaryIn, ordinaryOut, diagnostics, liveness, meteringIn, meteringOut [2]*os.File
}

func newPipes() (processPipes, error) {
	var pipes processPipes
	for _, pair := range []*[2]*os.File{&pipes.ordinaryIn, &pipes.ordinaryOut, &pipes.diagnostics, &pipes.liveness, &pipes.meteringIn, &pipes.meteringOut} {
		r, w, err := os.Pipe()
		if err != nil {
			pipes.closeAll()
			return processPipes{}, ErrStart
		}
		*pair = [2]*os.File{r, w}
	}
	return pipes, nil
}

func (pipes processPipes) closeChildEnds() {
	for _, f := range []*os.File{pipes.ordinaryIn[0], pipes.ordinaryOut[1], pipes.diagnostics[1], pipes.liveness[0], pipes.meteringIn[0], pipes.meteringOut[1]} {
		_ = f.Close()
	}
}

func (pipes processPipes) closeWriters() {
	_ = pipes.ordinaryIn[1].Close()
	_ = pipes.meteringIn[1].Close()
	_ = pipes.liveness[1].Close()
}

func (pipes processPipes) closeAll() {
	for _, pair := range [][2]*os.File{pipes.ordinaryIn, pipes.ordinaryOut, pipes.diagnostics, pipes.liveness, pipes.meteringIn, pipes.meteringOut} {
		for _, f := range pair {
			if f != nil {
				_ = f.Close()
			}
		}
	}
}

// Start starts only the installed guardian, with a mandatory execution deadline.
// It completes all fallible setup before exec; no child can be leaked on error.
func Start(ctx context.Context, spec Spec) (*Process, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, ErrDeadline
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env, err := childEnvironment(spec.Environment)
	if err != nil {
		return nil, err
	}
	pipes, err := newPipes()
	if err != nil {
		return nil, err
	}
	// The command and FD layout are immutable deployment code. Neither a Run
	// nor Spec can select an executable, module, path, argument or test mode.
	cmd := exec.Command("/usr/local/bin/python", "-I", "-u", "-m", "jobforge_agent.guardian")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pipes.ordinaryIn[0], pipes.ordinaryOut[1], pipes.diagnostics[1]
	cmd.ExtraFiles = []*os.File{pipes.liveness[0], pipes.meteringIn[0], pipes.meteringOut[1]}
	if err := cmd.Start(); err != nil {
		pipes.closeAll()
		return nil, ErrStart
	}
	pipes.closeChildEnds()
	return supervise(ctx, cmd, pipes), nil
}

// supervise has no command-building or dynamic-entry responsibility. Tests may
// attach already-started adversarial children to exercise the same I/O owner.
func supervise(ctx context.Context, cmd *exec.Cmd, pipes processPipes) *Process {
	p := &Process{
		pid:    cmd.Process.Pid,
		events: make(chan Event, 8), done: make(chan struct{}), stop: make(chan struct{}),
		closed: make(chan struct{}), abort: make(chan struct{}),
		ordinary: laneWriter{queue: make(chan writeRequest, 1)},
		metering: laneWriter{queue: make(chan writeRequest, 1)},
	}
	var sources sync.WaitGroup
	startSource := func(fn func(), joined chan struct{}) {
		sources.Add(1)
		go func() {
			defer sources.Done()
			defer close(joined)
			fn()
		}()
	}
	ordinaryRead, ordinaryWrite := make(chan struct{}), make(chan struct{})
	meteringRead, meteringWrite := make(chan struct{}), make(chan struct{})
	stderrDone, waiterDone, waited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	startSource(func() { p.readLane(Ordinary, pipes.ordinaryOut[0]) }, ordinaryRead)
	startSource(func() { p.readLane(Metering, pipes.meteringOut[0]) }, meteringRead)
	startSource(func() { p.writeLane(Ordinary, pipes.ordinaryIn[1], &p.ordinary) }, ordinaryWrite)
	startSource(func() { p.writeLane(Metering, pipes.meteringIn[1], &p.metering) }, meteringWrite)
	startSource(func() { p.drainStderr(pipes.diagnostics[0]) }, stderrDone)
	startSource(func() {
		err := cmd.Wait()
		exit := exitFact(cmd, err)
		p.mu.Lock()
		p.facts.Guardian = exit
		p.mu.Unlock()
		// Cleanup must observe Wait even when delivery of its event is blocked.
		close(waited)
		p.emit(Event{Kind: GuardianExited, Exit: exit})
	}, waiterDone)
	joined := make(chan struct{})
	go func() { sources.Wait(); close(joined) }()
	go p.lifecycle(ctx, cmd.Process.Pid, pipes, waited, joined, ordinaryRead, ordinaryWrite, meteringRead, meteringWrite, stderrDone)
	return p
}

func exitFact(cmd *exec.Cmd, err error) Exit {
	if cmd.ProcessState == nil {
		return Exit{Observed: true, Code: -1}
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		// Actual Wait occurred, but no normal child outcome was established.
		return Exit{Observed: true, Code: -1}
	}
	if status.Signaled() {
		return Exit{Observed: true, Code: -1, Signaled: true, Signal: int(status.Signal())}
	}
	if !status.Exited() || (err != nil && status.ExitStatus() == 0) {
		return Exit{Observed: true, Code: -1}
	}
	return Exit{Observed: true, Code: status.ExitStatus()}
}

func (p *Process) readLane(channel Channel, file *os.File) {
	defer func() { _ = file.Close() }()
	reader := bufio.NewReaderSize(file, 4096)
	read := runprotocol.ReadFrame
	if channel == Metering {
		read = runprotocol.ReadMeteringFrame
	}
	for {
		frame, err := read(reader)
		if err == nil && !inbound(channel, frame.Kind) {
			err = runprotocol.ErrProtocol
		}
		if errors.Is(err, io.EOF) {
			p.mu.Lock()
			if channel == Ordinary {
				p.facts.Ordinary.EOF = true
			} else {
				p.facts.Metering.EOF = true
			}
			p.mu.Unlock()
			p.emit(Event{Kind: ChannelEOF, Channel: channel})
			return
		}
		if err != nil {
			problem := classifyRead(err)
			p.problem(channel, problem)
			p.Stop()
			p.emit(Event{Kind: ChannelFailed, Channel: channel, Problem: problem})
			return
		}
		p.emit(Event{Kind: FrameReceived, Channel: channel, Frame: &frame})
		select {
		case <-p.abort:
			return
		default:
		}
	}
}

func classifyRead(err error) Problem {
	switch {
	case errors.Is(err, runprotocol.ErrFrameLimit):
		return FrameTooLarge
	case errors.Is(err, runprotocol.ErrProtocol):
		return InvalidFrame
	default:
		return PipeFailure
	}
}

func (p *Process) writeLane(channel Channel, file *os.File, writer *laneWriter) {
	defer func() { _ = file.Close() }()
	stop := p.closed
	if channel == Ordinary {
		stop = p.stop
	}
	for {
		select {
		case <-stop:
			return
		case <-p.closed:
			return
		case req := <-writer.queue:
			if p.writeStopped(channel) {
				req.done <- ErrStopped
				continue
			}
			n, err := file.Write(req.data)
			if err != nil || n != len(req.data) {
				if channel == Metering && errors.Is(err, os.ErrClosed) && channelClosed(p.closed) {
					// Actual Wait/forced teardown may close this local descriptor
					// between the pre-write check and the syscall. Preserve the
					// same stopped fact as observing p.closed before queuing.
					req.done <- ErrStopped
					return
				}
				if channel == Metering && errors.Is(err, syscall.EPIPE) {
					// A step may flush its final report and close its ACK reader
					// before the independent settlement RPC finishes. Preserve
					// that output fact without racing its natural typed exit.
					p.problem(channel, MeteringWriteClosed)
					req.done <- ErrMeteringClosed
					p.emit(Event{Kind: ChannelFailed, Channel: channel, Problem: MeteringWriteClosed})
					return
				}
				p.problem(channel, PipeFailure)
				p.Stop()
				req.done <- ErrWrite
				p.emit(Event{Kind: ChannelFailed, Channel: channel, Problem: PipeFailure})
				return
			}
			req.done <- nil
		}
	}
}

func (p *Process) drainStderr(file *os.File) {
	defer func() { _ = file.Close() }()
	var buf [4096]byte
	overflow := false
	for {
		n, err := file.Read(buf[:])
		p.mu.Lock()
		p.facts.StderrBytes = min(stderrLimit+1, p.facts.StderrBytes+int64(n))
		tooLarge := p.facts.StderrBytes > stderrLimit
		p.mu.Unlock()
		if tooLarge && !overflow {
			overflow = true
			p.Stop()
			p.emit(Event{Kind: DiagnosticFailed, Problem: StderrTooLarge})
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.problem(Ordinary, PipeFailure)
				p.Stop()
				p.emit(Event{Kind: DiagnosticFailed, Problem: PipeFailure})
			}
			return
		}
	}
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func groupGone(pgid int) bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }

func (p *Process) closeWrites(pipes processPipes) {
	p.closedOnce.Do(func() { close(p.closed) })
	pipes.closeWriters()
}

func (p *Process) lifecycle(ctx context.Context, pgid int, pipes processPipes, waited, joined, ordinaryRead, ordinaryWrite, meteringRead, meteringWrite, stderrDone <-chan struct{}) {
	defer pipes.closeAll()
	var grace <-chan time.Time
	var graceTimer *time.Timer
	var limit <-chan time.Time
	var limitTimer *time.Timer
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
		if limitTimer != nil {
			limitTimer.Stop()
		}
	}()
	select {
	case <-waited:
		p.closeWrites(pipes)
		limitTimer = time.NewTimer(cleanupLimit)
		limit = limitTimer.C
		if !groupGone(pgid) {
			// Reaping the guardian is insufficient. A surviving child makes this
			// execution unclean even if our subsequent forced cleanup succeeds.
			p.problem(Ordinary, PipeFailure)
			p.Stop()
		}
	case <-ctx.Done():
		p.Stop()
	case <-p.stop:
	}
	if channelClosed(p.stop) {
		_ = pipes.ordinaryIn[1].Close()
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		graceTimer = time.NewTimer(terminationGrace)
		grace = graceTimer.C
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timedOut := false
	for !channelClosed(joined) || !groupGone(pgid) {
		select {
		case <-grace:
			grace = nil
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			p.closeWrites(pipes)
			if limitTimer == nil {
				limitTimer = time.NewTimer(cleanupLimit)
				limit = limitTimer.C
			}
		case <-limit:
			timedOut = true
		case <-ticker.C:
		}
		if timedOut {
			break
		}
	}
	// Stop event producers before closing their channel. Deadline exhaustion is
	// recorded before forced pipe closure; local closure never invents peer EOF.
	close(p.abort)
	p.closeWrites(pipes)
	if timedOut {
		pipes.closeAll()
	}
	p.delivery.Lock()
	p.eventsClosed = true
	close(p.events)
	p.delivery.Unlock()
	p.mu.Lock()
	p.facts.Ordinary.Joined = channelClosed(ordinaryRead) && channelClosed(ordinaryWrite)
	p.facts.Metering.Joined = channelClosed(meteringRead) && channelClosed(meteringWrite)
	p.facts.StderrJoined = channelClosed(stderrDone)
	p.facts.GroupGone = groupGone(pgid)
	p.facts.CleanupTimedOut = timedOut
	p.final = p.facts
	p.mu.Unlock()
	close(p.done)
}
