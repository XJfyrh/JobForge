package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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
