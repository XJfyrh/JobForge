//go:build linux

package runexecutor

import (
	"errors"
	"syscall"
	"testing"
)

func TestMeteringACKAfterActualWaitReportsStopped(t *testing.T) {
	p, ctx := startPeer(t, "exit_69")
	if err := p.WriteOrdinary(ctx, fixtureFrame(t, "execute_step")); err != nil {
		t.Fatal(err)
	}
	_, receipt := collect(p)
	requireClean(t, receipt)
	if err := p.WriteMetering(ctx, fixtureFrame(t, "metering_ack")); !errors.Is(err, ErrStopped) {
		t.Fatalf("late ACK did not report local writer closure: %v", err)
	}
	if receipt.Guardian.Code != 69 || receipt.Guardian.Signaled || receipt.Metering.Problem != NoProblem || !receipt.Metering.EOF {
		t.Fatalf("late ACK changed actual exit facts: %+v", receipt)
	}
}

func TestMeteringACKClosedReaderKeepsTypedExitAndInputFailures(t *testing.T) {
	for _, mode := range []string{"metering_closed_timeout", "metering_closed_bad_frame"} {
		t.Run(mode, func(t *testing.T) {
			p, ctx := startPeer(t, mode)
			if err := p.WriteOrdinary(ctx, fixtureFrame(t, "execute_step")); err != nil {
				t.Fatal(err)
			}
			for event := range p.Events() {
				if event.Kind == FrameReceived && event.Channel == Metering {
					break
				}
			}
			if err := p.WriteMetering(ctx, fixtureFrame(t, "metering_ack")); !errors.Is(err, ErrMeteringClosed) {
				t.Fatalf("actual EPIPE was not classified narrowly: %v", err)
			}
			if p.stopping.Load() {
				t.Fatal("advisory ACK raced natural Wait by terminating the child")
			}
			signal := syscall.SIGUSR1
			if mode == "metering_closed_bad_frame" {
				signal = syscall.SIGUSR2
			}
			if err := syscall.Kill(p.pid, signal); err != nil {
				t.Fatal(err)
			}
			_, receipt := collect(p)
			requireClean(t, receipt)
			if mode == "metering_closed_bad_frame" {
				if receipt.Metering.Problem != InvalidFrame {
					t.Fatal("closed ACK writer masked later invalid metering input")
				}
			} else if receipt.Guardian.Code != 69 || receipt.Guardian.Signaled || receipt.Metering.Problem != MeteringWriteClosed || !receipt.Metering.EOF {
				t.Fatalf("EPIPE erased actual typed exit: %+v", receipt)
			}
		})
	}
}
