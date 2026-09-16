package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

const emptyDeployment = `{"schema_version":1,"tenants":["tenant-a"],"profiles":[],"enabled_profiles":[],"workers":[{"worker_id":"worker-a","tenants":["tenant-a"],"profile_ids":[],"capacity":1}],"tenant_capacity":1,"profile_capacity":2,"budgets":[],"bindings":[]}`

func deploymentFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDeploymentRejectsAmbiguousAuthorityConfiguration(t *testing.T) {
	for name, body := range map[string]string{
		"top-level-alias": strings.Replace(emptyDeployment, `"schema_version"`, `"SCHEMA_VERSION"`, 1),
		"worker-alias":    strings.Replace(emptyDeployment, `"worker_id"`, `"Worker_ID"`, 1),
		"null-capacity":   strings.Replace(emptyDeployment, `"tenant_capacity":1`, `"tenant_capacity":null`, 1),
		"null-profiles":   strings.Replace(emptyDeployment, `"profiles":[]`, `"profiles":null`, 1),
		"duplicate":       strings.Replace(emptyDeployment, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"unknown-executor": strings.Replace(emptyDeployment, `"profile_capacity":2`,
			`"profile_capacity":2,"command":"SECRET_CONFIG_EXECUTABLE"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readDeployment(deploymentFile(t, body))
			if err == nil || strings.Contains(err.Error(), "SECRET_CONFIG") {
				t.Fatalf("ambiguous configuration accepted or leaked: %v", err)
			}
		})
	}
	config, err := readDeployment(deploymentFile(t, emptyDeployment))
	if err != nil || len(config.Profiles) != 0 || len(config.Workers) != 1 {
		t.Fatalf("explicit no-model deployment rejected: %+v %v", config, err)
	}
}

func TestCredentialsCannotCrossPublicWorkerOrBusinessBoundaries(t *testing.T) {
	workers := []agentrun.WorkerConfig{{ID: "worker-a"}}
	for _, name := range []string{"public-worker-reuse", "public-business-reuse", "worker-business-reuse", "missing-worker", "unknown-worker", "extra-business-tenant", "invalid-business-control", "null-role", "role-alias"} {
		t.Run(name, func(t *testing.T) {
			public := `{"SECRET_PUBLIC_TOKEN":{"tenant_id":"tenant-a","role":"operator"}}`
			worker := `{"worker-a":"SECRET_WORKER_TOKEN"}`
			business := `{"tenant-a":"SECRET_BUSINESS_TOKEN"}`
			switch name {
			case "public-worker-reuse":
				worker = `{"worker-a":"SECRET_PUBLIC_TOKEN"}`
			case "public-business-reuse":
				business = `{"tenant-a":"SECRET_PUBLIC_TOKEN"}`
			case "worker-business-reuse":
				business = `{"tenant-a":"SECRET_WORKER_TOKEN"}`
			case "missing-worker":
				worker = `{}`
			case "unknown-worker":
				worker = `{"worker-b":"SECRET_WORKER_TOKEN"}`
			case "extra-business-tenant":
				business = `{"tenant-a":"SECRET_BUSINESS_TOKEN","tenant-b":"SECRET_EXTRA_TOKEN"}`
			case "invalid-business-control":
				business = `{"tenant-a":"SECRET_BUSINESS_\u0001TOKEN"}`
			case "null-role":
				public = `{"SECRET_PUBLIC_TOKEN":{"tenant_id":"tenant-a","role":null}}`
			case "role-alias":
				public = `{"SECRET_PUBLIC_TOKEN":{"tenant_id":"tenant-a","ROLE":"operator"}}`
			}
			t.Setenv("JOBFORGE_AGENT_API_KEYS", public)
			t.Setenv("JOBFORGE_AGENT_WORKER_KEYS", worker)
			t.Setenv("JOBFORGE_AGENT_BUSINESS_KEYS", business)
			if _, err := loadCredentials(workers, []string{"tenant-a"}); err == nil || strings.Contains(err.Error(), "SECRET_") {
				t.Fatalf("invalid credential boundary accepted or exposed secret: %v", err)
			}
		})
	}
	t.Setenv("JOBFORGE_AGENT_API_KEYS", `{"public-fixture":{"tenant_id":"tenant-a","role":"reader"}}`)
	t.Setenv("JOBFORGE_AGENT_WORKER_KEYS", `{"worker-a":"worker-fixture"}`)
	t.Setenv("JOBFORGE_AGENT_BUSINESS_KEYS", `{"tenant-a":"business-fixture-token"}`)
	if _, err := loadCredentials(workers, []string{"tenant-a"}); err != nil {
		t.Fatalf("separate configured identities rejected: %v", err)
	}
}

func TestStartupErrorsDoNotDiscloseConnectionOrConfigurationSecrets(t *testing.T) {
	t.Setenv("JOBFORGE_AGENT_CONFIG", deploymentFile(t, emptyDeployment))
	t.Setenv("JOBFORGE_AGENT_DSN", "postgres://operator:SECRET_DATABASE_TOKEN@%invalid/database")
	if err := command(t.Context(), []string{"serve"}); err == nil || strings.Contains(err.Error(), "SECRET_") || strings.Contains(err.Error(), "operator") {
		t.Fatalf("invalid DSN error exposed connection details: %v", err)
	}
	t.Setenv("JOBFORGE_AGENT_CONFIG", filepath.Join(t.TempDir(), "SECRET_CONFIG_PATH"))
	if err := command(t.Context(), []string{"bootstrap"}); err == nil || strings.Contains(err.Error(), "SECRET_") {
		t.Fatalf("configuration file error exposed path: %v", err)
	}
}

type blockedScanner struct {
	started chan struct{}
	stopped chan struct{}
}

func (s *blockedScanner) Sweep(ctx context.Context, _ int) (int, error) {
	close(s.started)
	<-ctx.Done()
	close(s.stopped)
	return 0, ctx.Err()
}

type blockedHealth struct {
	healthv1.UnimplementedHealthServer
	started chan struct{}
	stopped chan struct{}
}

func (h *blockedHealth) Check(ctx context.Context, _ *healthv1.HealthCheckRequest) (*healthv1.HealthCheckResponse, error) {
	close(h.started)
	<-ctx.Done()
	close(h.stopped)
	return nil, ctx.Err()
}

func localListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func TestShutdownCancelsActiveHTTPScannerAndBoundedRPC(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	httpListener, rpcListener := localListener(t), localListener(t)
	httpStarted, httpStopped := make(chan struct{}), make(chan struct{})
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(httpStarted)
		<-r.Context().Done()
		close(httpStopped)
	})}
	gateway := grpc.NewServer()
	health := &blockedHealth{started: make(chan struct{}), stopped: make(chan struct{})}
	healthv1.RegisterHealthServer(gateway, health)
	scanner := &blockedScanner{started: make(chan struct{}), stopped: make(chan struct{})}
	finished := make(chan error, 1)
	go func() { finished <- runServers(ctx, server, gateway, httpListener, rpcListener, scanner) }()
	clientCtx, stopClients := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopClients()
	httpFinished := make(chan struct{})
	go func() {
		defer close(httpFinished)
		request, err := http.NewRequestWithContext(clientCtx, http.MethodGet, "http://"+httpListener.Addr().String(), nil)
		if err != nil {
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	connection, err := grpc.NewClient(rpcListener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	rpcFinished := make(chan struct{})
	go func() {
		defer close(rpcFinished)
		_, _ = healthv1.NewHealthClient(connection).Check(clientCtx, &healthv1.HealthCheckRequest{})
	}()
	for _, started := range []chan struct{}{httpStarted, health.started, scanner.started} {
		select {
		case <-started:
		case <-clientCtx.Done():
			t.Fatal("active shutdown test did not reach its request/scan barrier")
		}
	}
	cancel()
	// HTTP work and the scanner should observe cancellation immediately, while
	// the live RPC has the command's existing bounded graceful-stop interval.
	for _, stopped := range []chan struct{}{httpStopped, scanner.stopped} {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("shutdown left HTTP or scanner work alive until forced close")
		}
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("shutdown did not terminate the active RPC and server goroutines")
	}
	for _, stopped := range []chan struct{}{health.stopped, httpFinished, rpcFinished} {
		select {
		case <-stopped:
		case <-clientCtx.Done():
			t.Fatal("shutdown returned with client or handler work still active")
		}
	}
}

type failingListener struct{ net.Listener }

func (f failingListener) Accept() (net.Conn, error) {
	return nil, errors.New("synthetic listener failure SECRET_CONNECTION_TOKEN")
}

func TestListenerFailureStopsPeersAndReturnsSafeDiagnostic(t *testing.T) {
	httpListener, rpcListener := localListener(t), localListener(t)
	server := &http.Server{ReadHeaderTimeout: time.Second}
	scanner := &blockedScanner{started: make(chan struct{}), stopped: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := runServers(ctx, server, grpc.NewServer(), failingListener{httpListener}, rpcListener, scanner)
	if err == nil || err.Error() != "run listener failed" || strings.Contains(err.Error(), "SECRET_") {
		t.Fatalf("listener diagnostic escaped its safe boundary: %v", err)
	}
}
