//go:build linux

package runexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	runprotocol "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
)

func requireProcessTests(t *testing.T) {
	t.Helper()
	if os.Getenv("JOBFORGE_RUNEXECUTOR_PROCESS_TESTS") != "1" {
		t.Skip("requires fixed Linux runtime image with --init and JOBFORGE_RUNEXECUTOR_PROCESS_TESTS=1; skip is not process acceptance")
	}
	if _, err := os.Stat("/usr/local/bin/python"); err != nil {
		t.Fatal(err)
	}
}

func fixtureFrame(t *testing.T, kind string) runprotocol.Frame {
	t.Helper()
	data, err := os.ReadFile("../../api/executor/v2/fixtures/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Frames []json.RawMessage `json:"valid_frames"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, raw := range corpus.Frames {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatal(err)
		}
		frame, err := runprotocol.Decode(append(compact.Bytes(), '\n'))
		if err != nil {
			t.Fatal(err)
		}
		if frame.Kind == kind {
			return frame
		}
	}
	t.Fatalf("fixture kind missing: %s", kind)
	return runprotocol.Frame{}
}

func startPeer(t *testing.T, mode string) (*Process, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return startPeerWithContext(ctx, t, mode), ctx
}

func startPeerWithContext(ctx context.Context, t *testing.T, mode string) *Process {
	t.Helper()
	requireProcessTests(t)
	pipes, err := newPipes()
	if err != nil {
		t.Fatal(err)
	}
	// Only test code selects adversarial programs. Production Start cannot do so.
	cmd := exec.Command("/usr/local/bin/python", "-I", "-u", "testdata/peer.py", mode)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pipes.ordinaryIn[0], pipes.ordinaryOut[1], pipes.diagnostics[1]
	cmd.ExtraFiles = []*os.File{pipes.liveness[0], pipes.meteringIn[0], pipes.meteringOut[1]}
	if err := cmd.Start(); err != nil {
		pipes.closeAll()
		t.Fatal(err)
	}
	pipes.closeChildEnds()
	p := supervise(ctx, cmd, pipes)
	t.Cleanup(func() {
		p.Stop()
		for range p.Events() {
			// Cleanup still drains delivery if a preceding assertion failed.
			continue
		}
		receipt := p.Wait()
		if !receipt.Guardian.Observed || !receipt.GroupGone {
			t.Errorf("peer leaked: %+v", receipt)
		}
	})
	return p
}

func await(t *testing.T, limit time.Duration, predicate func() bool) {
	t.Helper()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !predicate() {
		select {
		case <-timer.C:
			t.Fatal("bounded process assertion timed out")
		case <-tick.C:
		}
	}
}

func collect(p *Process) ([]Event, Receipt) {
	var events []Event
	for event := range p.Events() {
		events = append(events, event)
	}
	return events, p.Wait()
}

func requireClean(t *testing.T, receipt Receipt) {
	t.Helper()
	if !receipt.Guardian.Observed || !receipt.GroupGone || receipt.CleanupTimedOut || receipt.EventDeliveryFailed ||
		!receipt.Ordinary.Joined || !receipt.Metering.Joined || !receipt.StderrJoined {
		t.Fatalf("incomplete cleanup: %+v", receipt)
	}
}

func TestStartRequiresDeadlineAndValidatedEnvironment(t *testing.T) {
	if _, err := Start(context.Background(), Spec{}); !errors.Is(err, ErrDeadline) {
		t.Fatalf("missing deadline: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Start(ctx, Spec{Environment: Environment{DeepSeekKey: "bad\x00key"}}); !errors.Is(err, ErrEnvironment) {
		t.Fatalf("environment: %v", err)
	}
	cancel()
	if _, err := Start(ctx, Spec{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled start: %v", err)
	}
}

func TestPhysicalExitFactsAndProtocolFailures(t *testing.T) {
	for _, tc := range []struct {
		mode               string
		ordinary, metering Problem
		code               int
		frames             int
	}{
		{"success", NoProblem, NoProblem, 0, 1},
		{"trailing", NoProblem, NoProblem, 0, 2},
		{"wrong_ordinary", InvalidFrame, NoProblem, -1, 0},
		{"wrong_metering", NoProblem, InvalidFrame, -1, 0},
		{"oversize", FrameTooLarge, NoProblem, -1, 0},
		{"metering_oversize", NoProblem, FrameTooLarge, -1, 0},
		{"fragment", InvalidFrame, NoProblem, 0, 0},
		{"metering_fragment", NoProblem, InvalidFrame, 0, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p, ctx := startPeer(t, tc.mode)
			if err := p.WriteOrdinary(ctx, fixtureFrame(t, "execute_step")); err != nil {
				t.Fatal(err)
			}
			events, receipt := collect(p)
			requireClean(t, receipt)
			if receipt.Ordinary.Problem != tc.ordinary || receipt.Metering.Problem != tc.metering {
				t.Fatalf("wrong facts: %+v", receipt)
			}
			frames := 0
			for _, event := range events {
				if event.Kind == FrameReceived {
					frames++
				}
			}
			if frames != tc.frames {
				t.Fatalf("received %d frames, want %d", frames, tc.frames)
			}
			if tc.code >= 0 && (receipt.Guardian.Signaled || receipt.Guardian.Code != tc.code) {
				t.Fatalf("wrong exit: %+v", receipt.Guardian)
			}
			if tc.ordinary == NoProblem && !receipt.Ordinary.EOF {
				t.Fatal("ordinary EOF missing")
			}
			if tc.metering == NoProblem && !receipt.Metering.EOF {
				t.Fatal("metering EOF missing")
			}
			if again := p.Wait(); again != receipt {
				t.Fatal("Wait receipt mutated")
			}
		})
	}
}

func TestOrdinaryEOFPreservesActualExitClassification(t *testing.T) {
	for _, code := range []int{0, 64, 65, 66, 67, 68, 69, 70, 71, 72, 99} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			p, ctx := startPeer(t, fmt.Sprintf("exit_%d", code))
			if err := p.WriteOrdinary(ctx, fixtureFrame(t, "execute_step")); err != nil {
				t.Fatal(err)
			}
			_, receipt := collect(p)
			requireClean(t, receipt)
			if receipt.Guardian.Code != code || receipt.Guardian.Signaled || !receipt.Ordinary.EOF || receipt.Ordinary.Problem != NoProblem {
				t.Fatalf("exit facts overwritten: %+v", receipt)
			}
		})
	}
	p, ctx := startPeer(t, "signal")
	if err := p.WriteOrdinary(ctx, fixtureFrame(t, "execute_step")); err != nil {
		t.Fatal(err)
	}
	_, receipt := collect(p)
	requireClean(t, receipt)
	if !receipt.Guardian.Signaled || receipt.Guardian.Signal != int(syscall.SIGKILL) {
		t.Fatalf("signal disguised: %+v", receipt.Guardian)
	}
}

func TestBadOrdinaryFrameStillDrainsIndependentMetering(t *testing.T) {
	p, ctx := startPeer(t, "bad_then_metering")
	if err := p.WriteOrdinary(ctx, fixtureFrame(t, "execute_step")); err != nil {
		t.Fatal(err)
	}
	events, receipt := collect(p)
	requireClean(t, receipt)
	if receipt.Ordinary.Problem != InvalidFrame || !receipt.Metering.EOF || receipt.Metering.Problem != NoProblem {
		t.Fatalf("independent lane lost: %+v", receipt)
	}
	reports := 0
	for _, event := range events {
		if event.Kind == FrameReceived && event.Channel == Metering {
			reports++
		}
	}
	if reports != 1 {
		t.Fatalf("complete captured metering reports = %d", reports)
	}
}

func TestStderrOverflowAfterOrdinaryEOF(t *testing.T) {
	p, ctx := startPeer(t, "late_stderr")
	if err := p.WriteOrdinary(ctx, fixtureFrame(t, "execute_step")); err != nil {
		t.Fatal(err)
	}
	sawEOF, sawOverflow := false, false
	for event := range p.Events() {
		if event.Kind == ChannelEOF && event.Channel == Ordinary {
			sawEOF = true
			if err := syscall.Kill(p.pid, syscall.SIGUSR1); err != nil {
				t.Fatal(err)
			}
		}
		if event.Kind == DiagnosticFailed && event.Problem == StderrTooLarge {
			sawOverflow = true
		}
	}
	receipt := p.Wait()
	requireClean(t, receipt)
	if !sawEOF || !sawOverflow || receipt.StderrBytes != 8193 {
		t.Fatalf("late stderr omitted: eof=%v overflow=%v receipt=%+v", sawEOF, sawOverflow, receipt)
	}
}

func TestFullEventsDoNotDelayKillOrInventEOF(t *testing.T) {
	for _, mode := range []string{"full_events", "full_metering"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := startPeer(t, mode)
			await(t, time.Second, func() bool { return len(p.events) == cap(p.events) })
			started := time.Now()
			p.Stop()
			if time.Since(started) > 20*time.Millisecond {
				t.Fatal("Stop blocked on delivery")
			}
			await(t, time.Second, func() bool { return groupGone(p.pid) })
			select {
			case <-p.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("full event channel blocked cleanup deadline")
			}
			receipt := p.Wait()
			if !receipt.Guardian.Observed || !receipt.GroupGone || !receipt.CleanupTimedOut || !receipt.EventDeliveryFailed || (mode == "full_events" && receipt.Ordinary.EOF) || (mode == "full_metering" && receipt.Metering.EOF) {
				t.Fatalf("backpressure facts misreported: %+v", receipt)
			}
			for range p.Events() {
				// Consume the deliberately abandoned bounded backlog after Done.
				continue
			}
		})
	}
}

func TestBlockedMeteringWriteDoesNotDelayStop(t *testing.T) {
	p, _ := startPeer(t, "block_metering")
	if event := <-p.Events(); event.Kind != FrameReceived {
		t.Fatalf("missing ready frame: %+v", event)
	}
	ack := fixtureFrame(t, "metering_ack")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	written := make(chan error, 1)
	go func() {
		for i := 0; i < 128; i++ {
			if err := p.WriteMetering(ctx, ack); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	await(t, time.Second, func() bool { return p.metering.busy.Load() })
	started := time.Now()
	p.Stop()
	_, receipt := collect(p)
	requireClean(t, receipt)
	if time.Since(started) > time.Second {
		t.Fatal("metering backpressure delayed group kill")
	}
	if err := <-written; err == nil {
		t.Fatal("blocked metering unexpectedly completed all writes")
	}
}

func TestContextCancellationDuringStopPublicationCleansProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	p := startPeerWithContext(ctx, t, "block_input")
	select {
	case event := <-p.Events():
		if event.Kind != FrameReceived {
			t.Fatalf("missing ready frame: %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("peer did not reach its ready barrier")
	}
	// Pause the first Stop between its successful CAS and channel notification.
	// Cancellation must initiate cleanup without waiting for that notification.
	if !p.stopping.CompareAndSwap(false, true) {
		t.Fatal("peer stopped before the publication race was established")
	}
	drained := make(chan struct{})
	go func() {
		for range p.Events() {
			continue
		}
		close(drained)
	}()
	t.Cleanup(func() {
		close(p.stop) // Finish the deliberately paused publication exactly once.
		select {
		case <-p.Done():
		default:
			// Only failed assertions use this escape hatch. The old bug leaves
			// both termination timers unset and the metering writer asleep.
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
			p.closedOnce.Do(func() { close(p.closed) })
		}
		select {
		case <-drained:
		case <-time.After(3 * time.Second):
			t.Error("forced peer cleanup did not drain events")
		}
	})
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	cancel()
	select {
	case <-p.Done():
	case <-timer.C:
		t.Fatal("context cancellation waited for an unpublished Stop notification")
	}
	select {
	case <-drained:
	case <-timer.C:
		t.Fatal("process completed without joining its event consumer")
	}
	receipt := p.Wait()
	requireClean(t, receipt)
	if !receipt.Ordinary.EOF || !receipt.Metering.EOF || !receipt.Guardian.Signaled || receipt.Guardian.Signal != int(syscall.SIGKILL) {
		t.Fatalf("cancelled peer lacks actual kill and pipe EOF facts: %+v", receipt)
	}
}

func TestRepeatedProcessesJoinReadersAndReleaseFDs(t *testing.T) {
	// Warm up exec/runtime poll state before counting our own steady-state FDs.
	run := func() {
		p, ctx := startPeer(t, "success")
		if err := p.WriteOrdinary(ctx, fixtureFrame(t, "execute_step")); err != nil {
			t.Fatal(err)
		}
		_, receipt := collect(p)
		requireClean(t, receipt)
	}
	run()
	beforeFD, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	beforeGo := runtime.NumGoroutine()
	for i := 0; i < 12; i++ {
		run()
	}
	await(t, time.Second, func() bool { return runtime.NumGoroutine() <= beforeGo })
	afterFD, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFD) != len(beforeFD) {
		t.Fatalf("FD count grew from %d to %d", len(beforeFD), len(afterFD))
	}
}

func TestBlockedWriteCancelAndConcurrentCallAreBounded(t *testing.T) {
	p, _ := startPeer(t, "block_input")
	if event := <-p.Events(); event.Kind != FrameReceived {
		t.Fatalf("missing ready frame: %+v", event)
	}
	frame := fixtureFrame(t, "execute_step")
	frame.Checkpoint = json.RawMessage(`{"padding":"` + strings.Repeat("x", 200*1024) + `"}`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	written := make(chan error, 1)
	go func() { written <- p.WriteOrdinary(ctx, frame) }()
	await(t, time.Second, func() bool { return p.ordinary.busy.Load() })
	if err := p.WriteOrdinary(ctx, frame); !errors.Is(err, ErrConcurrentWrite) {
		t.Fatalf("second writer: %v", err)
	}
	cancel()
	select {
	case err := <-written:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked writer did not cancel")
	}
	_, receipt := collect(p)
	requireClean(t, receipt)
	if err := p.WriteOrdinary(context.Background(), frame); !errors.Is(err, ErrDeadline) {
		t.Fatalf("unbounded write: %v", err)
	}
}

func TestSurvivingGroupIsNotACleanExit(t *testing.T) {
	p, _ := startPeer(t, "residual")
	_, receipt := collect(p)
	requireClean(t, receipt)
	if receipt.Guardian.Code != 0 || receipt.Ordinary.Problem != PipeFailure {
		t.Fatalf("residual group disguised as clean exit: %+v", receipt)
	}
}

func children(pid int) []int {
	files, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	var result []int
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, word := range strings.Fields(string(data)) {
			if child, err := strconv.Atoi(word); err == nil {
				result = append(result, child)
			}
		}
	}
	return result
}

func startFixed(t *testing.T) (*Process, context.CancelFunc, int) {
	t.Helper()
	requireProcessTests(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	p, err := Start(ctx, Spec{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Stop()
		for range p.Events() {
			// Cleanup retains the normal drain-before-Wait ownership rule.
			continue
		}
		receipt := p.Wait()
		cancel()
		if !receipt.GroupGone || !receipt.Guardian.Observed {
			t.Errorf("fixed runtime leaked: %+v", receipt)
		}
	})
	var child int
	await(t, 3*time.Second, func() bool {
		ids := children(p.pid)
		if len(ids) == 1 {
			child = ids[0]
			return true
		}
		return false
	})
	return p, cancel, child
}

func TestFixedGuardianFDIsolationAndGroup(t *testing.T) {
	t.Setenv("JOBFORGE_DATABASE_URL", "synthetic-control-secret")
	t.Setenv("PYTHONPATH", "/untrusted")
	p, _, child := startFixed(t)
	group, err := syscall.Getpgid(child)
	if err != nil || group != p.pid {
		t.Fatalf("child PGID=%d guardian=%d error=%v", group, p.pid, err)
	}
	liveness, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/3", p.pid))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", child))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		target, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", child, entry.Name()))
		if target == liveness {
			t.Fatal("step inherited guardian parent-liveness pipe")
		}
	}
	await(t, time.Second, func() bool {
		for _, fd := range []int{0, 1, 4, 5} {
			childPipe, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", child, fd))
			if err != nil {
				return false
			}
			guardianEntries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", p.pid))
			if err != nil {
				return false
			}
			for _, entry := range guardianEntries {
				target, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", p.pid, entry.Name()))
				if target == childPipe {
					return false
				}
			}
		}
		return true
	})
	for _, pid := range []int{p.pid, child} {
		environment, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(environment, []byte("synthetic-control-secret")) || bytes.Contains(environment, []byte("PYTHONPATH=")) {
			t.Fatal("ambient control secret or Python path inherited")
		}
		command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			t.Fatal(err)
		}
		module := "jobforge_agent.guardian"
		if pid == child {
			module = "jobforge_agent.step"
		}
		if !bytes.Contains(command, []byte("\x00-I\x00-u\x00-m\x00"+module+"\x00")) {
			t.Fatal("runtime did not use the fixed isolated command")
		}
	}
	p.Stop()
	_, receipt := collect(p)
	requireClean(t, receipt)
	if !receipt.Ordinary.EOF || !receipt.Metering.EOF {
		t.Fatalf("fixed EOF missing: %+v", receipt)
	}
}

func TestFixedGuardianDeathAndCancellation(t *testing.T) {
	for _, mode := range []string{"guardian_death", "context_cancel"} {
		t.Run(mode, func(t *testing.T) {
			p, cancel, child := startFixed(t)
			started := time.Now()
			if mode == "guardian_death" {
				if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			_, receipt := collect(p)
			requireClean(t, receipt)
			if time.Since(started) > time.Second {
				t.Fatal("termination exceeded grace and init scheduling allowance")
			}
			if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("child remains alive or zombie: %v", err)
			}
			if mode == "guardian_death" && !receipt.Guardian.Signaled {
				t.Fatal("guardian signal was disguised")
			}
		})
	}
}

func TestFixedParentDeathUsesLivenessEOF(t *testing.T) {
	requireProcessTests(t)
	if os.Getenv("JOBFORGE_RUNEXECUTOR_PARENT_HELPER") == "1" {
		p, _, child := startFixed(t)
		if err := json.NewEncoder(os.Stdout).Encode([2]int{p.pid, child}); err != nil {
			os.Exit(2)
		}
		for range p.Events() {
			// The parent helper is intentionally killed while consuming events.
			continue
		}
		os.Exit(3)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestFixedParentDeathUsesLivenessEOF$")
	cmd.Env = append(os.Environ(), "JOBFORGE_RUNEXECUTOR_PARENT_HELPER=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var ids [2]int
	if err := json.NewDecoder(out).Decode(&ids); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("SIGKILL parent unexpectedly exited normally")
	}
	await(t, 2*time.Second, func() bool { return groupGone(ids[0]) })
	for _, pid := range ids {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("orphan %d alive or zombie: %v", pid, err)
		}
	}
}
