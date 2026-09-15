package main

import (
	"testing"
	"time"
)

func TestClockCheckRejectsObservedWindowsSteps(t *testing.T) {
	// Real 2026-09-15 samples: timesyncd stepped back by 2.372s, then
	// Hyper-V implicit time synchronization restored the host clock.
	samples := []sample{
		{RTTMillis: .51, OffsetMillis: .2},
		{RTTMillis: .503, OffsetMillis: -2372.336, StepMillis: -2372.892},
		{RTTMillis: .586, OffsetMillis: -.938, StepMillis: 2370.716},
	}
	r := summarize(samples, 100*time.Millisecond, 100*time.Millisecond, 250*time.Millisecond)
	if r.Passed || r.MaxStepMillis < 2372 || r.MaxOffsetMillis < 2372 {
		t.Fatalf("observed NTP/Hyper-V conflict was not detected: %+v", r)
	}
}

func TestClockCheckSeparatesRTTFromClockSteps(t *testing.T) {
	r := summarize([]sample{{RTTMillis: 1}, {RTTMillis: 220, OffsetMillis: 105, StepMillis: 105}},
		100*time.Millisecond, 100*time.Millisecond, 250*time.Millisecond)
	if !r.Passed || r.MaxStepMillis != 0 {
		t.Fatalf("network uncertainty misclassified as a clock step: %+v", r)
	}
	r = summarize([]sample{{RTTMillis: 1}, {RTTMillis: 300}},
		100*time.Millisecond, 100*time.Millisecond, 250*time.Millisecond)
	if r.Passed || r.MaxStepMillis != 0 || r.MaxRTTMillis != 300 {
		t.Fatalf("slow SQL should fail the environment check separately: %+v", r)
	}
}

func TestClockCheckRequiresSamplesAndRejectsFixedOffset(t *testing.T) {
	for _, samples := range [][]sample{nil, {{RTTMillis: 1}}, {{OffsetMillis: 200}, {OffsetMillis: 200}}} {
		if summarize(samples, 100*time.Millisecond, 100*time.Millisecond, time.Second).Passed {
			t.Fatalf("incomplete or skewed samples passed: %+v", samples)
		}
	}
}
