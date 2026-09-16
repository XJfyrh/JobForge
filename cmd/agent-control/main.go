// Command agent-control runs the v3 Run API, authenticated Worker RPC and one
// bounded recovery scanner. It never starts the historical jobs scheduler.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/migrate"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/businessclient"
	"github.com/xjfyrh/jobforge/internal/run/grpcapi"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

type deployment struct {
	SchemaVersion   int                `json:"schema_version"`
	Tenants         []string           `json:"tenants"`
	Profiles        []agentrun.Profile `json:"profiles"`
	EnabledProfiles []string           `json:"enabled_profiles"`
	Workers         []workerConfig     `json:"workers"`
	TenantCapacity  int                `json:"tenant_capacity"`
	ProfileCapacity int                `json:"profile_capacity"`
	Budgets         []budgetConfig     `json:"budgets"`
	Bindings        []budgetBinding    `json:"bindings"`
}

type workerConfig struct {
	ID       string   `json:"worker_id"`
	Tenants  []string `json:"tenants"`
	Profiles []string `json:"profile_ids"`
	Capacity int      `json:"capacity"`
}

type budgetConfig struct {
	ID         string         `json:"account_id"`
	Scope      string         `json:"scope"`
	Key        string         `json:"scope_key"`
	ValidFrom  time.Time      `json:"valid_from"`
	ValidUntil time.Time      `json:"valid_until"`
	Limits     agentrun.Usage `json:"limits"`
}

type budgetBinding struct {
	TenantID        string `json:"tenant_id"`
	BatchAccountID  string `json:"batch_account_id"`
	TenantAccountID string `json:"tenant_account_id"`
}

type publicIdentity struct {
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
}

type credentials struct {
	business map[string]string
	workers  map[string]string
	public   map[string]publicIdentity
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := command(ctx, os.Args[1:]); err != nil {
		// Every returned error is a fixed safe diagnostic, never a DSN, token,
		// database detail, configuration body or protected business content.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "prepare-support" {
		return prepareSupport(args[1:])
	}
	if len(args) != 1 || (args[0] != "bootstrap" && args[0] != "serve" && args[0] != "inspect-support") {
		return errors.New("expected agent-control bootstrap, serve, prepare-support or inspect-support")
	}
	config, err := readDeployment(os.Getenv("JOBFORGE_AGENT_CONFIG"))
	if err != nil {
		return err
	}
	dsn := os.Getenv("JOBFORGE_AGENT_DSN")
	pgConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil || dsn == "" {
		return errors.New("invalid JOBFORGE_AGENT_DSN")
	}
	pgConfig.MaxConns = 16
	pgConfig.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, pgConfig)
	if err != nil {
		return errors.New("control PostgreSQL unavailable")
	}
	defer pool.Close()
	options := runpostgres.Options{Profiles: config.Profiles, TenantCapacity: config.TenantCapacity, ProfileCapacity: config.ProfileCapacity}
	for _, w := range config.Workers {
		options.Workers = append(options.Workers, agentrun.WorkerConfig{ID: w.ID, Tenants: w.Tenants, ProfileIDs: w.Profiles, Capacity: w.Capacity})
	}
	store, err := runpostgres.New(pool, options)
	if err != nil {
		return errors.New("invalid registered Run deployment")
	}
	if args[0] == "inspect-support" {
		return inspectSupport(ctx, store, config)
	}
	if args[0] == "bootstrap" {
		setupCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		var businessDatabase bool
		if err := pool.QueryRow(setupCtx, "select to_regclass('business_meta.database_identity') is not null").Scan(&businessDatabase); err != nil || businessDatabase {
			return errors.New("control bootstrap refused a business or unavailable database")
		}
		if err := migrate.New(pool, slog.Default()).Up(setupCtx); err != nil {
			return errors.New("control migration failed")
		}
		if err := store.EnsureProfiles(setupCtx); err != nil {
			return errors.New("immutable profile registration failed")
		}
		for _, b := range config.Budgets {
			if err := store.CreateBudget(setupCtx, agentrun.BudgetSpec{ID: b.ID, Scope: b.Scope, Key: b.Key, ValidFrom: b.ValidFrom, ValidUntil: b.ValidUntil, Limits: b.Limits}); err != nil {
				return errors.New("budget setup rejected; existing accounts are never reset")
			}
		}
		for _, b := range config.Bindings {
			if err := store.BindBudgetTenant(setupCtx, b.TenantID, b.BatchAccountID, b.TenantAccountID); err != nil {
				return errors.New("budget tenant binding rejected")
			}
		}
		fmt.Println(`{"bootstrap":"complete"}`)
		return nil
	}
	return serve(ctx, pool, store, options, config.Tenants)
}

func readDeployment(path string) (deployment, error) {
	var config deployment
	file, err := os.Open(path)
	if err != nil {
		return config, errors.New("run deployment file unavailable")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return config, errors.New("run deployment must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || agentrun.ValidateStepJSON(data, 1<<20) != nil {
		return config, errors.New("run deployment is not bounded unambiguous JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || !exactDeploymentFields(data, config) || config.SchemaVersion != 1 || len(config.Tenants) == 0 || len(config.Tenants) > 128 ||
		len(config.Profiles) > 128 || len(config.Workers) == 0 || len(config.Workers) > 128 || len(config.Budgets) > 512 || len(config.Bindings) > 512 {
		return config, errors.New("invalid Run deployment schema")
	}
	for _, tenant := range config.Tenants {
		if !agentrun.ValidIdentifier(tenant) {
			return config, errors.New("invalid configured tenant")
		}
	}
	for _, id := range config.EnabledProfiles {
		if !slices.ContainsFunc(config.Profiles, func(p agentrun.Profile) bool { return p.ID == id }) {
			return config, errors.New("enabled profile is not registered")
		}
	}
	for i := range config.Profiles {
		p := &config.Profiles[i]
		registeredStrategy := p.Strategy == agentrun.BoundedReadonlyStrategy || agentrun.IsSupportStrategy(p.Strategy) && p.AuditEnabled()
		if !registeredStrategy ||
			!agentrun.ValidIdentifier(p.ExecutorVersion) || agentrun.ValidateStepJSON(p.Definition, 16384) != nil || agentrun.ValidateSupportProfile(*p) != nil {
			return config, errors.New("unregistered profile strategy or invalid immutable definition")
		}
		if _, err := agentrun.ReservationBudget(*p, agentrun.SubcallChat); err != nil {
			return config, errors.New("profile has no valid conservative call bound")
		}
		p.Executable = slices.Contains(config.EnabledProfiles, p.ID)
	}
	for _, w := range config.Workers {
		for _, tenant := range w.Tenants {
			if !slices.Contains(config.Tenants, tenant) {
				return config, errors.New("worker tenant is not registered")
			}
		}
	}
	if slices.ContainsFunc(config.Profiles, func(p agentrun.Profile) bool { return p.Strategy == agentrun.SupportFixedStrategy }) {
		for _, budget := range config.Budgets {
			if budget.Scope == "batch" && (budget.Limits.CostMicroyuan <= 0 || budget.Limits.CostMicroyuan > 5000000) {
				return config, errors.New("support batch cost limit must be between 1 and 5000000 microyuan")
			}
		}
	}
	return config, nil
}

// encoding/json accepts case-insensitive aliases and scalar nulls. Compare the
// typed wire shape without inspecting the immutable profile's opaque definition.
// Missing optional fields retain documented server defaults; explicit nulls do not.
func exactDeploymentFields(data []byte, config deployment) bool {
	canonical, err := json.Marshal(config)
	if err != nil {
		return false
	}
	var actual, expected any
	if json.Unmarshal(data, &actual) != nil || json.Unmarshal(canonical, &expected) != nil {
		return false
	}
	return deploymentShape(actual, expected)
}

func deploymentShape(actual, expected any) bool {
	if actual == nil {
		return false
	}
	switch typed := actual.(type) {
	case map[string]any:
		fields, ok := expected.(map[string]any)
		if !ok {
			return false
		}
		for name, value := range typed {
			field, exists := fields[name]
			if !exists {
				return false
			}
			if name != "definition" && !deploymentShape(value, field) {
				return false
			}
		}
	case []any:
		values, ok := expected.([]any)
		if !ok || len(typed) != len(values) {
			return false
		}
		for i, value := range typed {
			if !deploymentShape(value, values[i]) {
				return false
			}
		}
	}
	return true
}

func readSecretJSON(name string, target any) error {
	data := []byte(os.Getenv(name))
	if len(data) == 0 || len(data) > 65536 || jsonstrict.Decode(data, target) != nil {
		return errors.New("invalid Run credential configuration")
	}
	return nil
}

// loadCredentials prevents a public credential from authenticating an internal
// Worker or business operator. Every configured capability has a distinct key;
// invalid configuration is rejected before readiness probes or listeners.
func loadCredentials(workers []agentrun.WorkerConfig, tenants []string) (credentials, error) {
	var result credentials
	if readSecretJSON("JOBFORGE_AGENT_BUSINESS_KEYS", &result.business) != nil ||
		readSecretJSON("JOBFORGE_AGENT_WORKER_KEYS", &result.workers) != nil ||
		readSecretJSON("JOBFORGE_AGENT_API_KEYS", &result.public) != nil || len(result.public) == 0 || len(result.public) > 512 ||
		len(result.workers) != len(workers) || len(result.business) != len(tenants) {
		return result, errors.New("invalid Run credential configuration")
	}
	seen := make(map[string]bool)
	accept := func(key string, minimum int) bool {
		if len(key) < minimum || len(key) > 256 || seen[key] {
			return false
		}
		for _, char := range key {
			if char < 33 || char > 126 {
				return false
			}
		}
		seen[key] = true
		return true
	}
	for key, identity := range result.public {
		if !slices.Contains(tenants, identity.TenantID) || (identity.Role != "reader" && identity.Role != "operator") || !accept(key, 1) {
			return result, errors.New("invalid or overlapping Run credentials")
		}
	}
	for _, worker := range workers {
		if !accept(result.workers[worker.ID], 1) {
			return result, errors.New("invalid or overlapping Run credentials")
		}
	}
	for _, tenant := range tenants {
		if !accept(result.business[tenant], 16) {
			return result, errors.New("invalid or overlapping Run credentials")
		}
	}
	return result, nil
}

func serve(ctx context.Context, pool *pgxpool.Pool, store *runpostgres.Store, options runpostgres.Options, tenants []string) error {
	configured, err := loadCredentials(options.Workers, tenants)
	if err != nil {
		return err
	}
	keys := make(map[string]httpapi.Identity, len(configured.public))
	for key, value := range configured.public {
		keys[key] = httpapi.Identity{TenantID: value.TenantID, Role: value.Role}
	}
	capture, err := businessclient.New(os.Getenv("JOBFORGE_AGENT_BUSINESS_URL"), configured.business)
	if err != nil {
		return errors.New("invalid configured business capture boundary")
	}
	service, err := agentrun.NewService(store, capture, tenants)
	if err != nil {
		return errors.New("run admission service unavailable")
	}
	router, err := httpapi.NewRouter(service, keys)
	if err != nil {
		return errors.New("invalid public Run identities")
	}
	gateway, err := grpcapi.NewServer(store, grpcapi.Config{Workers: options.Workers, Credentials: configured.workers})
	if err != nil {
		return errors.New("invalid Worker identities")
	}
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = checkControlReady(readyCtx, pool)
	if err == nil {
		err = store.EnsureProfiles(readyCtx)
	}
	cancel()
	if err != nil {
		return errors.New("control schema or immutable profiles not ready; bootstrap first")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		probe, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if checkControlReady(probe, pool) != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		router.ServeHTTP(w, r.WithContext(request))
	}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	httpListener, err := net.Listen("tcp", address("JOBFORGE_AGENT_HTTP_ADDR", "127.0.0.1:8093"))
	if err != nil {
		return errors.New("run HTTP listener unavailable")
	}
	defer func() { _ = httpListener.Close() }()
	rpcListener, err := net.Listen("tcp", address("JOBFORGE_AGENT_GRPC_ADDR", "127.0.0.1:9093"))
	if err != nil {
		return errors.New("run Worker listener unavailable")
	}
	defer func() { _ = rpcListener.Close() }()
	return runServers(ctx, server, gateway, httpListener, rpcListener, store)
}

func checkControlReady(ctx context.Context, pool *pgxpool.Pool) error {
	var ready bool
	err := pool.QueryRow(ctx, `select to_regclass('public.runs') is not null
		and to_regclass('public.physical_calls') is not null
		and to_regclass('public.run_steps') is not null
		and to_regclass('business_meta.database_identity') is null`).Scan(&ready)
	if err != nil || !ready {
		return errors.New("control schema is unavailable")
	}
	return nil
}

func address(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type recoveryScanner interface {
	Sweep(context.Context, int) (int, error)
}

func runServers(ctx context.Context, server *http.Server, gateway *grpc.Server, httpListener, rpcListener net.Listener, store recoveryScanner) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Shutdown cancellation reaches active HTTP handlers immediately instead of
	// leaving their database/capture operations alive until the force-close bound.
	server.BaseContext = func(net.Listener) context.Context { return runCtx }
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	workers.Go(func() { failures <- server.Serve(httpListener) })
	workers.Go(func() { failures <- gateway.Serve(rpcListener) })
	workers.Go(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				scanCtx, stop := context.WithTimeout(runCtx, 5*time.Second)
				_, err := store.Sweep(scanCtx, 100)
				stop()
				if err != nil && runCtx.Err() == nil {
					slog.Warn("Run recovery scan failed; will retry next tick")
				}
			}
		}
	})
	var failure error
	select {
	case <-ctx.Done():
	case failure = <-failures:
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if server.Shutdown(shutdown) != nil {
		_ = server.Close()
	}
	stopped := make(chan struct{})
	go func() { gateway.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-shutdown.Done():
		gateway.Stop()
		<-stopped
	}
	workers.Wait()
	if failure != nil && !errors.Is(failure, http.ErrServerClosed) && !errors.Is(failure, grpc.ErrServerStopped) {
		return errors.New("run listener failed")
	}
	return nil
}
