//go:build !linux

package runexecutor

import (
	"context"
	"errors"
	"testing"
)

func TestNativePlatformIsExplicitlyUnsupported(t *testing.T) {
	process, err := Start(context.Background(), Spec{})
	if process != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("process=%v error=%v", process, err)
	}
}
