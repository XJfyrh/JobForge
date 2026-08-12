package config

import (
	"strings"
	"testing"
	"time"
)

func clearConsumerEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"JOBFORGE_CONSUMER_GROUP",
		"JOBFORGE_CONSUMER_NAME",
		"JOBFORGE_CONSUMER_BLOCK_TIMEOUT",
		"JOBFORGE_CONSUMER_PENDING_SCAN_INTERVAL",
		"JOBFORGE_CONSUMER_PENDING_MIN_IDLE",
		"JOBFORGE_CONSUMER_PROCESS_TIMEOUT",
		"JOBFORGE_CONSUMER_MAX_DELIVERIES",
		"JOBFORGE_CONSUMER_RETRY_BASE",
		"JOBFORGE_CONSUMER_RETRY_MAX",
		"JOBFORGE_CONSUMER_POISON_STREAM",
		"JOBFORGE_REDIS_STREAM_KEY",
	} {
		t.Setenv(key, "")
	}
}

func TestConsumerConfigDefaults(t *testing.T) {
	clearConsumerEnvironment(t)
	config, err := Load()
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if config.ConsumerGroup != "jobforge-reference-v1" {
		t.Fatalf("consumer group = %q", config.ConsumerGroup)
	}
	if !consumerIdentifierPattern.MatchString(config.ConsumerName) ||
		!strings.Contains(config.ConsumerName, "-") {
		t.Fatalf("consumer name = %q", config.ConsumerName)
	}
	if config.ConsumerBlockTimeout != 2*time.Second ||
		config.ConsumerPendingScanInterval != 5*time.Second ||
		config.ConsumerPendingMinIdle != 30*time.Second ||
		config.ConsumerProcessTimeout != 10*time.Second ||
		config.ConsumerMaxDeliveries != 5 ||
		config.ConsumerRetryBase != time.Second ||
		config.ConsumerRetryMax != 30*time.Second {
		t.Fatalf("unexpected consumer defaults: %+v", config)
	}
	if config.ConsumerPoisonStream != "jobforge:events:poison" {
		t.Fatalf("poison stream = %q", config.ConsumerPoisonStream)
	}
}

func TestConsumerConfigValidation(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"unsafe_group", "JOBFORGE_CONSUMER_GROUP", "bad group"},
		{"unsafe_name", "JOBFORGE_CONSUMER_NAME", "bad/name"},
		{"idle_not_above_process", "JOBFORGE_CONSUMER_PENDING_MIN_IDLE", "10s"},
		{"zero_deliveries", "JOBFORGE_CONSUMER_MAX_DELIVERIES", "0"},
		{"retry_base_above_max", "JOBFORGE_CONSUMER_RETRY_BASE", "31s"},
		{"invalid_duration", "JOBFORGE_CONSUMER_BLOCK_TIMEOUT", "soon"},
		{"invalid_deliveries", "JOBFORGE_CONSUMER_MAX_DELIVERIES", "many"},
		{"poison_matches_source", "JOBFORGE_CONSUMER_POISON_STREAM", "jobforge:events"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConsumerEnvironment(t)
			t.Setenv(test.key, test.value)
			if _, err := Load(); err == nil {
				t.Fatal("expected invalid consumer configuration")
			}
		})
	}
}

func TestConsumerPoisonStreamDerivesFromEventStream(t *testing.T) {
	clearConsumerEnvironment(t)
	t.Setenv("JOBFORGE_REDIS_STREAM_KEY", "tenant:events")
	config, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.ConsumerPoisonStream != "tenant:events:poison" {
		t.Fatalf("poison stream = %q", config.ConsumerPoisonStream)
	}
}

func TestHeartbeatIntervalConfiguration(t *testing.T) {
	tests := []struct {
		name          string
		heartbeat     string
		leaseTTL      string
		wantHeartbeat time.Duration
		wantLeaseTTL  time.Duration
		wantExplicit  bool
	}{
		{name: "default", wantHeartbeat: 5 * time.Second, wantLeaseTTL: 30 * time.Second},
		{name: "explicit non-default", heartbeat: "2s", leaseTTL: "17s", wantHeartbeat: 2 * time.Second, wantLeaseTTL: 17 * time.Second, wantExplicit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConsumerEnvironment(t)
			t.Setenv("JOBFORGE_HEARTBEAT_INTERVAL", tt.heartbeat)
			t.Setenv("JOBFORGE_LEASE_TTL", tt.leaseTTL)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.HeartbeatInterval != tt.wantHeartbeat || cfg.HeartbeatIntervalExplicit != tt.wantExplicit {
				t.Fatalf("heartbeat config = (%v, explicit=%v), want (%v, explicit=%v)",
					cfg.HeartbeatInterval, cfg.HeartbeatIntervalExplicit, tt.wantHeartbeat, tt.wantExplicit)
			}
			if cfg.LeaseTTL != tt.wantLeaseTTL {
				t.Fatalf("lease TTL = %v, want %v", cfg.LeaseTTL, tt.wantLeaseTTL)
			}
		})
	}
}

func TestHeartbeatIntervalRejectsInvalidExplicitValue(t *testing.T) {
	for _, value := range []string{"soon", "0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			clearConsumerEnvironment(t)
			t.Setenv("JOBFORGE_HEARTBEAT_INTERVAL", value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected %q to be rejected", value)
			}
		})
	}
}
