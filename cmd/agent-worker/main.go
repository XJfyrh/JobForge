// Command agent-worker runs one fixed Linux executor under AgentService leases.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/grpcapi"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	"github.com/xjfyrh/jobforge/internal/runworker"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

type deployment struct {
	SchemaVersion int                  `json:"schema_version"`
	Profiles      []run.Profile        `json:"profiles"`
	Tenants       map[string]endpoints `json:"tenants"`
}

type endpoints struct {
	BusinessOrigin string `json:"business_origin"`
	OllamaOrigin   string `json:"ollama_origin"`
}

type secrets struct {
	ControlToken string                   `json:"control_token"`
	Tenants      map[string]tenantSecrets `json:"tenants"`
}

type tenantSecrets struct {
	BusinessReadKey string `json:"business_read_key"`
	DeepSeekKey     string `json:"deepseek_api_key"`
}

type bearer struct {
	token  string
	secure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return b.secure }

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// Child output, credentials and remote errors are never diagnostics.
		slog.Error("agent worker stopped", "reason", stopReason(err))
		os.Exit(1)
	}
}

// stopReason classifies trusted error identities without logging their text.
func stopReason(err error) string {
	switch {
	case errors.Is(err, runworker.ErrCleanup):
		return "CLEANUP_UNCONFIRMED"
	case errors.Is(err, runworker.ErrAuthority):
		return "AUTHORITY_LOST"
	case errors.Is(err, runworker.ErrBatchStopped):
		return "BATCH_STOPPED"
	case errors.Is(err, run.ErrStepConflict):
		return "STEP_CONFLICT"
	case errors.Is(err, run.ErrProfileUnavailable):
		return "PROFILE_UNAVAILABLE"
	case errors.Is(err, run.ErrInvalidArgument):
		return "INVALID_ARGUMENT"
	case errors.Is(err, run.ErrDependencyUnavailable):
		return "DEPENDENCY_UNAVAILABLE"
	default:
		return "INTERNAL_ERROR"
	}
}

func serve(ctx context.Context) error {
	if len(os.Args) != 1 {
		return run.ErrInvalidArgument
	}
	manifest, err := runworker.LoadManifest()
	if err != nil {
		return err
	}
	var config deployment
	var keys secrets
	if readJSON(os.Getenv("JOBFORGE_AGENT_WORKER_CONFIG"), &config) != nil ||
		readJSON(os.Getenv("JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE"), &keys) != nil || config.SchemaVersion != 1 ||
		len(config.Tenants) < 1 || len(config.Tenants) != len(keys.Tenants) || !validToken(keys.ControlToken) {
		return run.ErrInvalidArgument
	}
	environments := make(map[string]runexecutor.Environment, len(config.Tenants))
	for tenant, origin := range config.Tenants {
		key, ok := keys.Tenants[tenant]
		if !ok || !run.ValidIdentifier(tenant) {
			return run.ErrInvalidArgument
		}
		environments[tenant] = runexecutor.Environment{BusinessOrigin: origin.BusinessOrigin, OllamaOrigin: origin.OllamaOrigin,
			BusinessReadKey: key.BusinessReadKey, DeepSeekKey: key.DeepSeekKey}
	}
	target := os.Getenv("JOBFORGE_AGENT_GATEWAY")
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || port == "" || strings.ContainsAny(target, "/\\\x00\r\n\t ") {
		return run.ErrInvalidArgument
	}
	secure := true
	transport := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	switch os.Getenv("JOBFORGE_AGENT_GRPC_TLS") {
	case "", "true":
	case "false":
		// Explicit local/private-network deployment only; TLS is the default.
		secure, transport = false, insecure.NewCredentials()
	default:
		return run.ErrInvalidArgument
	}
	connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport),
		grpc.WithPerRPCCredentials(bearer{token: keys.ControlToken, secure: secure}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcapi.MaxMessageBytes), grpc.MaxCallSendMsgSize(grpcapi.MaxMessageBytes)))
	if err != nil {
		return run.ErrDependencyUnavailable
	}
	defer func() { _ = connection.Close() }()
	worker, err := runworker.New(agentv1.NewAgentServiceClient(connection), manifest,
		runworker.Config{Profiles: config.Profiles, Environments: environments})
	if err != nil {
		return err
	}
	return worker.Run(ctx)
}

func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return run.ErrInvalidArgument
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, 256*1024+1))
	if err != nil || run.ValidateStepJSON(raw, 256*1024) != nil {
		return run.ErrInvalidArgument
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return run.ErrInvalidArgument
	}
	return nil
}

func validToken(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}
