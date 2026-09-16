package main

import (
	"errors"
	"strings"
	"testing"
)

func TestProtocolRejectsUnsafeAndMalformedFrames(t *testing.T) {
	for _, operation := range []string{"sh", "python -c", "$(whoami)", "../echo"} {
		_, err := encodeRequest(request{1, "step-1", operation, ""})
		if !errors.Is(err, errProtocol) {
			t.Fatalf("operation %q: %v", operation, err)
		}
	}
	_, err := encodeRequest(request{1, "step-1", "echo", strings.Repeat("a", maxRequest)})
	if !errors.Is(err, errOutputLimit) {
		t.Fatalf("request limit: %v", err)
	}
	for _, data := range []string{
		`not json`,
		`{"v":2,"id":"step-1","kind":"result"}`,
		`{"v":1,"id":"other","kind":"result"}`,
		`{"v":1,"id":"step-1","kind":"result","unknown":true}`,
		`{"v":1,"id":"step-1","kind":"result"} {}`,
		`{"v":1,"id":"step-1","kind":"started","pid":0}`,
	} {
		if _, err := decodeResponse([]byte(data), "step-1"); !errors.Is(err, errProtocol) {
			t.Fatalf("malformed frame accepted: %v", err)
		}
	}
}
