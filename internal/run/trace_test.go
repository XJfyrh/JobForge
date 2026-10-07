package run

import (
	"context"
	"testing"
)

func TestTraceContextKeepsSDKRandomFlagWithoutGrantingAuthority(t *testing.T) {
	for _, value := range []string{"", "00-11111111111111111111111111111111-2222222222222222-01", "00-11111111111111111111111111111111-2222222222222222-03", "00-11111111111111111111111111111111-2222222222222222-ff"} {
		if !ValidTraceContext(value) || admissionTraceContext(WithTraceContext(context.Background(), value)) != value {
			t.Fatal("valid flags were discarded")
		}
	}
	for _, value := range []string{"arbitrary", "00-00000000000000000000000000000000-2222222222222222-03", "00-11111111111111111111111111111111-0000000000000000-03", "00-11111111111111111111111111111111-2222222222222222-03\n"} {
		if ValidTraceContext(value) || admissionTraceContext(WithTraceContext(context.Background(), value)) != "" {
			t.Fatal("invalid metadata accepted")
		}
	}
}
