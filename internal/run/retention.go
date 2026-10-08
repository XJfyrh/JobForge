package run

import "time"

// TerminalContentRetention is the minimum after the first terminal transition.
const TerminalContentRetention = 7 * 24 * time.Hour

// CleanupResult reports identities and removed bytes, never protected content.
type CleanupResult struct {
	Applied      bool      `json:"applied"`
	RunIDs       []string  `json:"run_ids"`
	ContentBytes int64     `json:"content_bytes"`
	CheckedAt    time.Time `json:"checked_at"`
}

// CanPurgeContent excludes active states, already-expired content and open
// batches whose admission guard still needs canonical committed checkpoints.
func CanPurgeContent(r Run, batchUntil, now time.Time) bool {
	return r.State.Terminal() && r.TerminalAt != nil && r.ContentPurgedAt == nil &&
		!r.TerminalAt.Add(TerminalContentRetention).After(now) && !batchUntil.After(now)
}
