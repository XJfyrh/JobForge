//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func scriptPath(t *testing.T) string {
	t.Helper()
	if os.Getenv("JOBFORGE_EXECUTOR_PROCESS_TESTS") != "1" {
		t.Skip("requires Linux container with --init and JOBFORGE_EXECUTOR_PROCESS_TESTS=1; protocol-only tests are not process acceptance")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "executor.py")
}

func requireReaped(t *testing.T, pid int) {
	t.Helper()
	if pid == 0 {
		t.Fatal("missing process identity")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d remains alive or unreaped", pid)
}

func TestRealProcessContracts(t *testing.T) {
	for _, tc := range []struct {
		op   string
		want error
	}{
		{"echo", nil}, {"bad_json", errProtocol}, {"wrong_id", errProtocol},
		{"oversize", errOutputLimit}, {"stderr", errOutputLimit},
		{"trailing_bytes", errProtocol},
	} {
		t.Run(tc.op, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := execute(ctx, scriptPath(t), request{1, "s0-step", tc.op, "synthetic"}, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("result=%+v error=%v want=%v", result, err, tc.want)
			}
			if err == nil && result.Value != "synthetic" {
				t.Fatal("lost result")
			}
			requireReaped(t, result.GuardianPID)
			requireReaped(t, -result.GuardianPID)
			if result.StepPID > 0 {
				requireReaped(t, result.StepPID)
			}
			t.Logf("guardian=%d step=%d returned=%v; process group reaped", result.GuardianPID, result.StepPID, err)
		})
	}
}

func TestStderrOverflowAfterProtocolEOF(t *testing.T) {
	fixture := filepath.Join(filepath.Dir(scriptPath(t)), "testdata", "late_stderr.py")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	protocolEnded := false
	result, err := executeObserved(ctx, fixture, request{1, "late-stderr", "echo", "synthetic"}, nil, func(r execution) {
		protocolEnded = true
		// The fixture cannot write stderr before this branch has consumed stdout EOF.
		if err := syscall.Kill(r.GuardianPID, syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
	})
	if !protocolEnded || result.Value != "synthetic" {
		t.Fatalf("did not observe complete valid protocol before stderr: result=%+v error=%v", result, err)
	}
	if !errors.Is(err, errOutputLimit) {
		t.Fatalf("late stderr overflow was accepted: error=%v want=%v", err, errOutputLimit)
	}
	requireReaped(t, result.GuardianPID)
	requireReaped(t, -result.GuardianPID)
	t.Logf("guardian=%d: valid started/result and EOF preceded stderr overflow; rejected and reaped", result.GuardianPID)
}

func TestCancelTimeoutAndGuardianDeath(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "guardian_kill"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			var stepStarted time.Time
			result, err := execute(ctx, scriptPath(t), request{1, "s0-step", "ignore_term", ""}, func(r execution) {
				stepStarted = time.Now()
				if mode == "cancel" {
					cancel()
				}
				if mode == "guardian_kill" {
					if err := syscall.Kill(r.GuardianPID, syscall.SIGKILL); err != nil {
						t.Fatal(err)
					}
				}
			})
			want := context.Canceled
			if mode == "timeout" {
				want = context.DeadlineExceeded
			}
			if mode == "guardian_kill" {
				want = errProtocol
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
			requireReaped(t, result.GuardianPID)
			requireReaped(t, result.StepPID)
			requireReaped(t, -result.GuardianPID)
			t.Logf("guardian=%d step=%d startup=%s after_started=%s; group killed and reaped", result.GuardianPID, result.StepPID, stepStarted.Sub(started), time.Since(stepStarted))
		})
	}
}

func TestParentKillControlEOF(t *testing.T) {
	_ = scriptPath(t)
	if os.Getenv("JOBFORGE_EXECUTOR_PROBE_HELPER") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := execute(ctx, scriptPath(t), request{1, "s0-orphan", "ignore_term", ""}, func(r execution) {
			if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
				os.Exit(2)
			}
		})
		if err != nil {
			os.Exit(3)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestParentKillControlEOF$")
	cmd.Env = append(os.Environ(), "JOBFORGE_EXECUTOR_PROBE_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	parentWaited := false
	defer func() {
		if !parentWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var result execution
	if err := json.Unmarshal(line, &result); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	parentWaited = true
	if waitErr == nil {
		t.Fatal("parent kill unexpectedly succeeded")
	}
	requireReaped(t, result.GuardianPID)
	requireReaped(t, result.StepPID)
	requireReaped(t, -result.GuardianPID)
	t.Logf("Go parent=%d SIGKILL+Wait, guardian=%d step=%d reaped after control EOF in %s", cmd.Process.Pid, result.GuardianPID, result.StepPID, time.Since(killed))
}
