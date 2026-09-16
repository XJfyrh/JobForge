package run

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strconv"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidIdentifier is shared by idempotency keys and registered resource names.
func ValidIdentifier(value string) bool { return identifierPattern.MatchString(value) }

// Fingerprint uses an explicit domain and uint64 big-endian UTF-8 byte lengths.
// Delimiters in user identifiers cannot produce ambiguous concatenations.
func Fingerprint(domain string, fields ...string) string {
	h := sha256.New()
	for _, field := range append([]string{domain}, fields...) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Validate requires callers to fill the frozen timeout default before hashing.
func (r SubmitRequest) Validate() error {
	if r.SchemaVersion != 1 || !ValidIdentifier(r.TicketID) || !ValidIdentifier(r.BusinessRequestKey) ||
		!ValidIdentifier(r.ProfileID) || !ValidIdentifier(r.BudgetBatchID) || r.RunTimeoutSeconds < 1 || r.RunTimeoutSeconds > 86400 {
		return ErrInvalidArgument
	}
	return nil
}

// Hash excludes transport timeout, tracing, and the operation idempotency key.
func (r SubmitRequest) Hash() string {
	return Fingerprint("jobforge.run.submit.v1", "1", r.TicketID, r.BusinessRequestKey,
		r.ProfileID, r.BudgetBatchID, strconv.FormatInt(r.RunTimeoutSeconds, 10))
}

// Validate applies the same frozen timeout range to a new retry Run.
func (r RetryRequest) Validate() error {
	if r.SchemaVersion != 1 || r.RunTimeoutSeconds < 1 || r.RunTimeoutSeconds > 86400 {
		return ErrInvalidArgument
	}
	return nil
}

// Hash binds a retry command to its source Run, not an arbitrary new identity.
func (r RetryRequest) Hash(sourceID string) string {
	return Fingerprint("jobforge.run.retry.v1", "1", sourceID, strconv.FormatInt(r.RunTimeoutSeconds, 10))
}

// SnapshotKey preserves a capture identity across an uncertain submit response.
func SnapshotKey(tenant, operation, key, sourceID string) string {
	return Fingerprint("jobforge.run.snapshot.v1", tenant, operation, key, sourceID)
}
