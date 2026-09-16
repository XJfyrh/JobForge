package grpcapi

import (
	"errors"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/run"
)

func TestAuthorityObservationCannotFallBackToTransportWallClock(t *testing.T) {
	for _, invalid := range []time.Time{{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		stamp, err := authorityTimeToWire(invalid)
		if !errors.Is(err, run.ErrInternal) || stamp != nil {
			t.Fatal("missing or invalid storage clock became execution authority")
		}
	}
	// A deliberately unrelated database clock must survive unchanged. Mapping
	// it against this transport host's current wall time would inflate authority.
	databaseClock := time.Date(2020, 2, 3, 4, 5, 6, 789000, time.UTC)
	stamp, err := authorityTimeToWire(databaseClock)
	if err != nil || !stamp.AsTime().Equal(databaseClock) {
		t.Fatal("transport replaced the authoritative database clock")
	}
}
