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
		APIKeys:           make(map[string]string),
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
