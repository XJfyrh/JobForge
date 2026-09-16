package business

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestVectorRejectsInvalidDatabaseRepresentations(t *testing.T) {
	valid := make([]float64, Dimensions)
	valid[0] = 1
	if literal, err := vectorLiteral(valid); err != nil || !strings.HasPrefix(literal, "[1,0,") {
		t.Fatalf("valid vector: %s %v", literal, err)
	}
	for _, name := range []string{"short", "zero", "nan", "infinity", "overflow", "underflow", "squared-underflow", "squared-overflow"} {
		t.Run(name, func(t *testing.T) {
			vector := append([]float64(nil), valid...)
			switch name {
			case "short":
				vector = vector[:Dimensions-1]
			case "zero":
				vector[0] = 0
			case "nan":
				vector[0] = math.NaN()
			case "infinity":
				vector[0] = math.Inf(1)
			case "overflow":
				vector[0] = math.MaxFloat64
			case "underflow":
				vector[0] = math.SmallestNonzeroFloat64
			case "squared-underflow":
				for i := range vector {
					vector[i] = 1e-23
				}
			case "squared-overflow":
				vector[0] = 1e38
			}
			if _, err := vectorLiteral(vector); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
}

func TestDatabaseErrorsNeverExposeServerContents(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"23505", ErrConflict}, {"40001", ErrDependencyUnavailable}, {"57014", ErrDependencyUnavailable}, {"23514", ErrInternal}} {
		err := publicDBError(&pgconn.PgError{Code: tc.code, Message: "sensitive original row contents", Detail: "private diagnostic"})
		if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("unsafe mapping: %v", err)
		}
	}
	if !errors.Is(publicDBError(context.DeadlineExceeded), ErrDependencyUnavailable) {
		t.Fatal("deadline did not become a bounded dependency failure")
	}
}

func TestSourceComparisonPreservesLargeRevisions(t *testing.T) {
	first, err := sourceFingerprint([]byte(`{"revision":9007199254740992,"status":"open"}`))
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := sourceFingerprint([]byte(`{"status":"open","revision":9007199254740992}`))
	if err != nil || first != reordered {
		t.Fatal("JSONB object order changed source identity")
	}
	next, err := sourceFingerprint([]byte(`{"revision":9007199254740993,"status":"open"}`))
	if err != nil || first == next {
		t.Fatal("distinct bigint revisions lost precision")
	}
}

func TestDevDatasetSatisfiesImportContract(t *testing.T) {
	body, err := os.ReadFile("../../examples/support-agent/runtime/seed.json")
	if err != nil {
		t.Fatal(err)
	}
	var dataset Dataset
	if err := json.Unmarshal(body, &dataset); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDataset(dataset); err != nil {
		t.Fatal(err)
	}
	if len(dataset.Tickets) != 40 {
		t.Fatalf("development cases: %d", len(dataset.Tickets))
	}
	for _, ticket := range dataset.Tickets {
		if ticket.ObservedAt.IsZero() {
			t.Fatal("missing fixed evaluation instant")
		}
	}
	// Source inconsistencies are facts to investigate, not importer failures.
	dataset.Deliveries[0].Events = append(dataset.Deliveries[0].Events, DeliveryEvent{
		EventID: "contradictory-event", OccurredAt: dataset.Deliveries[0].Events[0].OccurredAt,
		Status: "in_transit", Note: "Contradicts the carrier status at the same instant.",
	})
	if err := ValidateDataset(dataset); err != nil {
		t.Fatalf("business contradiction rejected as schema: %v", err)
	}
	dataset.Tickets[1].TicketID = dataset.Tickets[0].TicketID
	dataset.Tickets[1].TenantID = dataset.Tickets[0].TenantID
	if !errors.Is(ValidateDataset(dataset), ErrInvalidArgument) {
		t.Fatal("duplicate source identity accepted")
	}
}
