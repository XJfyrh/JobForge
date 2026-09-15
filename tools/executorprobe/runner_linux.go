//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	terminationGrace  = 100 * time.Millisecond
	activeStepTimeout = time.Second
)

type execution struct {
	GuardianPID int    `json:"guardian_pid"`
	StepPID     int    `json:"step_pid"`
	Value       string `json:"value"`
}

type received struct {
	frame response
	err   error
}

// diagnostics counts bytes but never stores or logs raw Python diagnostics.
type diagnostics struct {
	mu       sync.Mutex
	count    int
	overflow chan struct{}
}

func (d *diagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count <= 8192 {
		d.count += len(p)
		if d.count > 8192 {
			close(d.overflow)
		}
	}
	return len(p), nil
}

func receiveFrames(out io.Reader, id string, messages chan<- received) {
	defer close(messages)
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 1024), maxResponse+2)
	count := 0
	for scanner.Scan() {
		count++
		if count > 2 {
			messages <- received{err: errProtocol}
			return
		}
		frame, err := decodeResponse(scanner.Bytes(), id)
		messages <- received{frame: frame, err: err}
		if err != nil {
			return
		}
	}
	if scanner.Err() != nil {
		messages <- received{err: errOutputLimit}
	}
}

func execute(ctx context.Context, script string, req request, onStarted func(execution)) (result execution, returnErr error) {
	encoded, err := encodeRequest(req)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return result, errors.New("step deadline required")
	}
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		return result, fmt.Errorf("create control pipe: %w", err)
	}
	defer func() { _ = inputRead.Close(); _ = inputWrite.Close() }()
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		return result, fmt.Errorf("create response pipe: %w", err)
	}
	defer func() { _ = outputRead.Close(); _ = outputWrite.Close() }()
	diag := &diagnostics{overflow: make(chan struct{})}
	// A deployment-controlled fixed script is the only entry point. No shell is used.
	cmd := exec.Command("python3", "-I", "-u", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "PYTHONHASHSEED=0"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inputRead, outputWrite, diag
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start executor: %w", err)
	}
	result.GuardianPID = cmd.Process.Pid
	_ = inputRead.Close()
	_ = outputWrite.Close()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	// One owner always signals the whole group and waits the direct child. Killing
	// just the guardian is insufficient: a blocked business step could outlive it.
	defer func() {
		if returnErr != nil {
			_ = syscall.Kill(-result.GuardianPID, syscall.SIGTERM)
		}
		timer := time.NewTimer(terminationGrace)
		defer timer.Stop()
		select {
		case waitErr := <-waited:
			_ = syscall.Kill(-result.GuardianPID, syscall.SIGKILL)
			if returnErr == nil && waitErr != nil {
				returnErr = errProtocol
			}
		case <-timer.C:
			_ = syscall.Kill(-result.GuardianPID, syscall.SIGKILL)
			if waitErr := <-waited; returnErr == nil && waitErr != nil {
				returnErr = errProtocol
			}
		}
	}()
	if _, err := inputWrite.Write(encoded); err != nil {
		return result, fmt.Errorf("send bounded request: %w", err)
	}
	messages := make(chan received, 4)
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); receiveFrames(outputRead, req.ID, messages) }()
	defer func() { _ = outputRead.Close(); <-readerDone }()
	started, completed := false, false
	var stepDeadline <-chan time.Time
	var stepTimer *time.Timer
	defer func() {
		if stepTimer != nil {
			stepTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-stepDeadline:
			return result, context.DeadlineExceeded
		case <-diag.overflow:
			return result, errOutputLimit
		case message, ok := <-messages:
			if !ok {
				if completed {
					return result, nil
				}
				return result, errProtocol
			}
			if message.err != nil {
				return result, message.err
			}
			switch message.frame.Kind {
			case "started":
				if started || completed {
					return result, errProtocol
				}
				started, result.StepPID = true, message.frame.PID
				stepTimer = time.NewTimer(activeStepTimeout)
				stepDeadline = stepTimer.C
				if onStarted != nil {
					onStarted(result)
				}
			case "result":
				if !started || completed {
					return result, errProtocol
				}
				completed, result.Value = true, message.frame.Value
			}
		}
	}
}
