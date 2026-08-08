// Package config loads application configuration from environment variables
// and optional config files. P0 uses static API key mappings per ADR-0001.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string

	// HTTPAddr is the listen address for the HTTP API server.
	HTTPAddr string

	// GRPCAddr is the listen address for the gRPC Worker Gateway.
	GRPCAddr string

	// LeaseTTL is the duration of a job lease before it expires.
	LeaseTTL time.Duration

	// HeartbeatInterval is the suggested Worker heartbeat interval.
	HeartbeatInterval time.Duration

	// ScanInterval is the Scheduler scan period for promoting jobs.
	ScanInterval time.Duration

	// APIKeys maps API key strings to tenant IDs. P0 static configuration.
	// In production, keys come from environment; never log these values.
	APIKeys map[string]string

	// QueueSoftLimit is the soft threshold for queue depth warning.
	QueueSoftLimit int

	// QueueHardLimit is the hard threshold above which submissions are rejected.
	QueueHardLimit int

	// TenantMaxInflight is the maximum number of running jobs per tenant.
	// If <= 0, no limit is enforced.
	TenantMaxInflight int

	// OTelExporterType selects the trace exporter: "stdout" or "none".
	OTelExporterType string

	// OTelSampleRatio is the trace sampling ratio in [0.0, 1.0].
	OTelSampleRatio float64

	// MetricsAddr is the listen address for pprof + Prometheus /metrics.
	// Defaults to 127.0.0.1:6060 (localhost only, PRD 11.4).
	MetricsAddr string

	// OutboxPollInterval is the minimum interval between outbox publish
	// rounds while there is backlog (PRD v0.2 FR-610 / §11.4).
	OutboxPollInterval time.Duration

	// OutboxBatchSize bounds how many events one publish round claims.
	OutboxBatchSize int

	// OutboxRetention is how long published outbox events are kept before
	// cleanup (PRD v0.2 FR-613).
	OutboxRetention time.Duration

	// OutboxChannel is the PostgreSQL NOTIFY channel used to deliver outbox
	// event hints (PRD v0.2 FR-611, ADR-0003).
	OutboxChannel string

	// SchedulerLeadershipTimeout bounds how long the Scheduler leader may go
	// without a heartbeat before standbys take over the leadership lease
	// (ADR-0005).
	SchedulerLeadershipTimeout time.Duration
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:       getEnv("JOBFORGE_DATABASE_URL", "postgres://jobforge:jobforge@localhost:5432/jobforge?sslmode=disable"),
		HTTPAddr:          getEnv("JOBFORGE_HTTP_ADDR", ":8080"),
		GRPCAddr:          getEnv("JOBFORGE_GRPC_ADDR", ":9090"),
		LeaseTTL:          getDurationEnv("JOBFORGE_LEASE_TTL", 30*time.Second),
		HeartbeatInterval: getDurationEnv("JOBFORGE_HEARTBEAT_INTERVAL", 10*time.Second),
		ScanInterval:      getDurationEnv("JOBFORGE_SCAN_INTERVAL", 1*time.Second),
		QueueSoftLimit:    getIntEnv("JOBFORGE_QUEUE_SOFT_LIMIT", 10000),
		QueueHardLimit:    getIntEnv("JOBFORGE_QUEUE_HARD_LIMIT", 50000),
		TenantMaxInflight: getIntEnv("JOBFORGE_TENANT_MAX_INFLIGHT", 100),
		OTelExporterType:  getEnv("JOBFORGE_OTEL_EXPORTER", "stdout"),
		OTelSampleRatio:   getFloatEnv("JOBFORGE_OTEL_SAMPLE_RATIO", 1.0),
		MetricsAddr:       getEnv("JOBFORGE_METRICS_ADDR", "127.0.0.1:6060"),

		OutboxPollInterval: getDurationEnv("JOBFORGE_OUTBOX_POLL_INTERVAL", 1*time.Second),
		OutboxBatchSize:    getIntEnv("JOBFORGE_OUTBOX_BATCH_SIZE", 100),
		OutboxRetention:    getDurationEnv("JOBFORGE_OUTBOX_RETENTION", 7*24*time.Hour),
		OutboxChannel:      getEnv("JOBFORGE_OUTBOX_CHANNEL", "jobforge_outbox"),

		SchedulerLeadershipTimeout: getDurationEnv("JOBFORGE_SCHEDULER_LEADERSHIP_TIMEOUT", 10*time.Second),

		APIKeys: make(map[string]string),
	}

	// Parse API keys from environment.
	// Format: JOBFORGE_API_KEYS="key1=tenant1,key2=tenant2"
	if raw := os.Getenv("JOBFORGE_API_KEYS"); raw != "" {
		pairs := strings.Split(raw, ",")
		for _, pair := range pairs {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) != 2 {
				return nil, fmt.Errorf("invalid API key pair: %q (expected key=tenant_id)", pair)
			}
			cfg.APIKeys[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}

	// Default development key if none configured.
	if len(cfg.APIKeys) == 0 {
		cfg.APIKeys["dev-api-key"] = "dev-tenant"
	}

	return cfg, nil
}

// TenantForKey looks up the tenant ID for a given API key.
// Returns empty string if the key is not found.
func (c *Config) TenantForKey(apiKey string) (string, bool) {
	tenant, ok := c.APIKeys[apiKey]
	return tenant, ok
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getDurationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func getIntEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getFloatEnv(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}
