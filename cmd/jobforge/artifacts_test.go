package main

import (
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/config"
)

func TestArtifactStartupErrorDoesNotExposeCredentials(t *testing.T) {
	err := runArtifacts(t.Context(), &config.Config{DatabaseURL: "postgres://user:private-marker@localhost:bad-port/db"})
	if err == nil || strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("startup must return a safe error: %v", err)
	}
}
