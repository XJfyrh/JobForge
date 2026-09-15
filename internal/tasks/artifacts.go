package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Artifact is an immutable, tenant-scoped business object, separate from jobs.
type Artifact struct {
	TenantID    string          `json:"-"`
	TaskType    string          `json:"task_type"`
	BusinessKey string          `json:"business_key"`
	ResultRef   string          `json:"result_ref"`
	Fingerprint string          `json:"fingerprint"`
	Body        json.RawMessage `json:"body"`
	PublishedAt time.Time       `json:"published_at"`
}

// PostgresArtifacts is the small reference business store, not a queue store.
type PostgresArtifacts struct{ pool *pgxpool.Pool }

// NewPostgresArtifacts binds an existing business database pool.
func NewPostgresArtifacts(pool *pgxpool.Pool) *PostgresArtifacts {
	return &PostgresArtifacts{pool: pool}
}

const artifactColumns = `tenant_id, task_type, business_key, result_ref, fingerprint, body, published_at`

func scanArtifact(row pgx.Row) (*Artifact, error) {
	var a Artifact
	err := row.Scan(&a.TenantID, &a.TaskType, &a.BusinessKey, &a.ResultRef, &a.Fingerprint, &a.Body, &a.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, transient("ARTIFACT_UNAVAILABLE")
	}
	return &a, nil
}

// Lookup resolves an artifact by its tenant-scoped business identity.
func (s *PostgresArtifacts) Lookup(ctx context.Context, tenant, taskType, key string) (*Artifact, error) {
	return scanArtifact(s.pool.QueryRow(ctx, `select `+artifactColumns+` from task_artifacts
		where tenant_id = $1 and task_type = $2 and business_key = $3`, tenant, taskType, key))
}

// Get resolves a reference only within the authenticated tenant.
func (s *PostgresArtifacts) Get(ctx context.Context, tenant, ref string) (*Artifact, error) {
	return scanArtifact(s.pool.QueryRow(ctx, `select `+artifactColumns+` from task_artifacts
		where tenant_id = $1 and result_ref = $2`, tenant, ref))
}

// Publish uses a unique business key as the publication linearization point.
// A concurrent loser returns the winner. The queue ACK is a later transaction;
// a crash in between is resolved by Lookup, never by publishing a second row.
func (s *PostgresArtifacts) Publish(ctx context.Context, a *Artifact) (*Artifact, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(a.Body) > MaxArtifactBytes || !json.Valid(a.Body) {
		return nil, false, permanent("ARTIFACT_TOO_LARGE")
	}
	stored, err := scanArtifact(s.pool.QueryRow(ctx, `insert into task_artifacts
		(tenant_id, task_type, business_key, result_ref, fingerprint, body)
		values ($1, $2, $3, $4, $5, $6)
		on conflict do nothing
		returning `+artifactColumns, a.TenantID, a.TaskType, a.BusinessKey, a.ResultRef, a.Fingerprint, a.Body))
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	stored, err = s.Lookup(ctx, a.TenantID, a.TaskType, a.BusinessKey)
	// Both the business key and reference are unique. Absorb either index's
	// concurrent conflict, then verify the complete business identity. A
	// reference collision belonging to another key must fail closed.
	if errors.Is(err, ErrNotFound) {
		return nil, false, permanent("BUSINESS_KEY_CONFLICT")
	}
	if err != nil {
		return nil, false, err
	}
	if stored.Fingerprint != a.Fingerprint {
		return nil, false, permanent("BUSINESS_KEY_CONFLICT")
	}
	return stored, false, nil
}
