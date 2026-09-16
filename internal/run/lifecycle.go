package run

import "time"

// HeartbeatResult grants continued execution only while Continue is true.
// A stopping response retains the old expiry and never renews its authority.
type HeartbeatResult struct {
	Continue         bool
	LeaseUntil       time.Time
	SessionExpiresAt time.Time
	StopReason       string
	// AuthorityObservedAt anchors the returned expiries to the database clock,
	// including stop responses that grant no further execution authority.
	AuthorityObservedAt time.Time
}

// StopResult confirms the first attempt closure, not renewed execution rights.
type StopResult struct {
	Run            Run
	AttemptOutcome string
}
