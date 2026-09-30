package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/xjfyrh/jobforge/internal/runworker"
)

func TestStopReasonDoesNotExposeWrappedOrUnknownErrorText(t *testing.T) {
	if got := stopReason(fmt.Errorf("private upstream response: %w", runworker.ErrCleanup)); got != "CLEANUP_UNCONFIRMED" {
		t.Fatalf("cleanup identity lost: %s", got)
	}
	if got := stopReason(errors.New("private upstream response")); got != "INTERNAL_ERROR" {
		t.Fatalf("untrusted error escaped: %s", got)
	}
}

func TestCredentialDecoderDoesNotAcceptAmbiguousOrExtraFields(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate": `{"control_token":"first","control_token":"second","tenants":{}}`,
		"extra":     `{"control_token":"fixture","tenants":{},"shell":"ignored"}`,
		"trailing":  `{"control_token":"fixture","tenants":{}}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			var value secrets
			if readJSON(path, &value) == nil {
				t.Fatal("ambiguous credential file accepted")
			}
		})
	}
}

func TestCredentialTransportPolicyAndInjectionRejection(t *testing.T) {
	for _, value := range []string{"", "one\r\ntwo", "two words", "nonascii-密", string([]byte{0})} {
		if validToken(value) {
			t.Fatal("invalid credential accepted")
		}
	}
	credential := bearer{token: "synthetic-worker-token", secure: true}
	metadata, err := credential.GetRequestMetadata(context.Background())
	if err != nil || metadata["authorization"] != "Bearer synthetic-worker-token" || !credential.RequireTransportSecurity() {
		t.Fatal("default TLS credential policy changed")
	}
}

func TestCredentialDecoderAcceptsEphemeralPipe(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("requires a POSIX anonymous descriptor path")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if _, err := writer.Write([]byte(`{"control_token":"synthetic-control","tenants":{"tenant-north":{"business_read_key":"synthetic-business","deepseek_api_key":"synthetic-provider"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var value secrets
	if err := readJSON(fmt.Sprintf("/dev/fd/%d", reader.Fd()), &value); err != nil {
		t.Fatal("pipe credential input rejected")
	}
	if value.ControlToken != "synthetic-control" || value.Tenants["tenant-north"].DeepSeekKey != "synthetic-provider" {
		t.Fatal("pipe input did not bind credentials")
	}
}
