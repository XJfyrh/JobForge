//go:build linux

package runclock

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLinuxClockMatchesIsolatedPython(t *testing.T) {
	before, err := Now()
	if err != nil {
		t.Fatal(err)
	}
	// The formal Linux runtime includes Python. A missing interpreter is a
	// failure here, not a skip that could masquerade as cross-process evidence.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "python3", "-I", "-c", "import time; print(time.clock_gettime_ns(time.CLOCK_BOOTTIME) // 1000000)").Output()
	if err != nil {
		t.Fatal("isolated Python clock probe failed")
	}
	after, err := Now()
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || child < before || child > after {
		t.Fatalf("clock domains differ: before=%d child=%d after=%d", before, child, after)
	}
}
