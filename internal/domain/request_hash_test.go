package domain

import (
	"testing"
	"time"
)

// hashTestParams returns a baseline set of submission parameters.
func hashTestParams() NewJobParams {
	return NewJobParams{
		TenantID: "test-tenant",
		Queue:    "default",
		Type:     "demo.echo",
		Payload:  []byte(`{"a":1,"b":2}`),
	}
}

// TestRequestHashPayloadCanonicalization verifies that semantically equal
// payloads (differing only in key order or whitespace) hash identically.
func TestRequestHashPayloadCanonicalization(t *testing.T) {
	a := hashTestParams()
	b := hashTestParams()
	b.Payload = []byte(`{ "b" : 2, "a" : 1 }`)

	if RequestHash(a) != RequestHash(b) {
		t.Fatal("semantically equal payloads must hash identically")
	}
}

// TestRequestHashDifferentParameters verifies that differing parameters
// produce different hashes.
func TestRequestHashDifferentParameters(t *testing.T) {
	base := RequestHash(hashTestParams())

	diffPayload := hashTestParams()
	diffPayload.Payload = []byte(`{"a":1,"b":3}`)
	if RequestHash(diffPayload) == base {
		t.Fatal("different payload values must hash differently")
	}

	diffPriority := hashTestParams()
	diffPriority.Priority = 5
	if RequestHash(diffPriority) == base {
		t.Fatal("different priority must hash differently")
	}

	diffQueue := hashTestParams()
	diffQueue.Queue = "other"
	if RequestHash(diffQueue) == base {
		t.Fatal("different queue must hash differently")
	}

	diffType := hashTestParams()
	diffType.Type = "demo.sleep"
	if RequestHash(diffType) == base {
		t.Fatal("different type must hash differently")
	}
}

// TestRequestHashRunAtSentinel verifies that an omitted run_at hashes via a
// sentinel (server-side now() must not leak in, otherwise a legitimate
// resubmission would conflict), and that explicit values are canonicalized
// to UTC.
func TestRequestHashRunAtSentinel(t *testing.T) {
	omitted1 := hashTestParams()
	omitted2 := hashTestParams()
	if RequestHash(omitted1) != RequestHash(omitted2) {
		t.Fatal("two submissions omitting run_at must hash identically")
	}

	explicit := hashTestParams()
	runAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	explicit.RunAt = &runAt
	if RequestHash(explicit) == RequestHash(omitted1) {
		t.Fatal("explicit run_at must hash differently from omitted run_at")
	}

	// The same instant expressed in another timezone hashes identically.
	explicitOtherTZ := hashTestParams()
	runAtTZ := runAt.In(time.FixedZone("UTC+8", 8*3600))
	explicitOtherTZ.RunAt = &runAtTZ
	if RequestHash(explicitOtherTZ) != RequestHash(explicit) {
		t.Fatal("the same instant in different zones must hash identically")
	}
}

// TestRequestHashDefaultedFields verifies that omitting max_attempts and
// timeout_seconds hashes identically to passing their defaults.
func TestRequestHashDefaultedFields(t *testing.T) {
	omitted := hashTestParams()

	explicitDefaults := hashTestParams()
	explicitDefaults.MaxAttempts = DefaultMaxAttempts
	explicitDefaults.TimeoutSeconds = int(DefaultTimeout.Seconds())

	if RequestHash(omitted) != RequestHash(explicitDefaults) {
		t.Fatal("omitting max_attempts/timeout_seconds must equal passing their defaults")
	}
}

// TestRequestHashLargeIntegerFidelity verifies that payload numbers keep
// their original text in canonicalization. Before the UseNumber fix,
// integers above 2^53 were rounded through float64 and distinct payloads
// collided on the same hash, so a second submission with different
// parameters was silently deduplicated instead of rejected as a conflict.
func TestRequestHashLargeIntegerFidelity(t *testing.T) {
	a := hashTestParams()
	a.Payload = []byte(`{"n":9007199254740993}`)
	b := hashTestParams()
	b.Payload = []byte(`{"n":9007199254740994}`)

	if RequestHash(a) == RequestHash(b) {
		t.Fatal("distinct large integers must not collide in the request hash")
	}

	// Key-order canonicalization must still hold alongside big integers.
	reordered := hashTestParams()
	reordered.Payload = []byte(`{ "n" : 9007199254740993, "a" : 1 }`)
	withKey := hashTestParams()
	withKey.Payload = []byte(`{"a":1,"n":9007199254740993}`)
	if RequestHash(reordered) != RequestHash(withKey) {
		t.Fatal("key order and whitespace must stay insignificant for big integers")
	}

	// The exact integer text survives canonicalization: adjacent values
	// around the float64 rounding boundary must stay distinct too.
	c := hashTestParams()
	c.Payload = []byte(`{"n":9007199254740992}`)
	if RequestHash(a) == RequestHash(c) {
		t.Fatal("adjacent large integers must not collide in the request hash")
	}
}

// TestRequestHashTrailingGarbageNotCanonical verifies that a payload with
// trailing garbage after the JSON value is treated as non-canonicalizable
// raw bytes (matching json.Unmarshal strictness), not silently truncated.
func TestRequestHashTrailingGarbageNotCanonical(t *testing.T) {
	clean := hashTestParams()
	clean.Payload = []byte(`{"a":1}`)
	garbage := hashTestParams()
	garbage.Payload = []byte(`{"a":1} trailing`)

	if RequestHash(clean) == RequestHash(garbage) {
		t.Fatal("payload with trailing garbage must not hash like the clean payload")
	}
}
