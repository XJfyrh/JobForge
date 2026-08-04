package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/xjfyrh/jobforge/internal/store"
)

// Cleaner periodically removes published outbox events older than the
// retention period (PRD v0.2 FR-613). Unpublished events are never removed:
// the cleanup condition requires published_at IS NOT NULL.
type Cleaner struct {
	store     store.OutboxStore
	retention time.Duration
	interval  time.Duration
	logger    *slog.Logger
}

// NewCleaner creates a retention cleaner.
func NewCleaner(st store.OutboxStore, retention, interval time.Duration, logger *slog.Logger) *Cleaner {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Cleaner{store: st, retention: retention, interval: interval, logger: logger}
}

// Run executes the cleanup loop until ctx is cancelled.
func (c *Cleaner) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			deleted, err := c.store.CleanupPublished(ctx, c.retention)
			if err != nil {
				if ctx.Err() == nil {
					c.logger.Warn("outbox retention cleanup failed", "error", err)
				}
				continue
			}
			if deleted > 0 {
				c.logger.Info("outbox retention cleanup",
					"deleted", deleted, "retention", c.retention.String())
			}
		}
	}
}
