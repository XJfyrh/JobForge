package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store uses the permissions of its pool: runtime and loader use separate pools.
type Store struct{ pool *pgxpool.Pool }

// NewStore does not connect, migrate, or elevate the pool identity.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// CheckRuntimeRole rejects an accidentally configured migration or loader DSN.
// The shared runtime identity is trusted for tenant filtering, not source writes.
func (s *Store) CheckRuntimeRole(ctx context.Context) error {
	var allowed bool
	err := s.pool.QueryRow(ctx, `select not r.rolsuper and not r.rolcreatedb
 and not r.rolcreaterole and not r.rolreplication and not r.rolbypassrls
 and pg_has_role(current_user, 'jobforge_business_runtime', 'MEMBER')
 and not pg_has_role(current_user, 'jobforge_business_owner', 'MEMBER')
 and not pg_has_role(current_user, 'jobforge_business_loader', 'MEMBER')
 and current_schemas(false) = array['pg_catalog']::name[]
 and not has_database_privilege(current_user, current_database(), 'CREATE')
 and not has_schema_privilege(current_user, 'business', 'CREATE')
 and not has_schema_privilege(current_user, 'extensions', 'CREATE')
 and not has_schema_privilege(current_user, 'public', 'CREATE')
 and not has_table_privilege(current_user, 'business.tickets', 'INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user, 'business.orders', 'INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user, 'business.deliveries', 'INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user, 'business.snapshots', 'UPDATE,DELETE')
 and not has_table_privilege(current_user, 'business.policy_indexes', 'INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user, 'business.policy_chunks', 'INSERT,UPDATE,DELETE')
 from pg_catalog.pg_roles r where r.rolname = current_user`).Scan(&allowed)
	if err != nil {
		return ErrDependencyUnavailable
	}
	if !allowed {
		return ErrWrongDatabase
	}
	return nil
}

// CheckReady verifies purpose and exact schema/extension versions on startup.
func (s *Store) CheckReady(ctx context.Context) error {
	var ready bool
	err := s.pool.QueryRow(ctx, `select
 (select purpose = 'jobforge-business-v1' and database_name = current_database()
  from business_meta.database_identity where singleton)
 and (select max(version) = 2 from business_meta.business_schema_migrations)
 and exists (select 1 from pg_catalog.pg_extension e
 join pg_catalog.pg_namespace n on n.oid = e.extnamespace
 where e.extname = 'vector' and e.extversion = '0.8.6' and n.nspname = 'extensions')`).Scan(&ready)
	if err != nil || !ready {
		return ErrDependencyUnavailable
	}
	return nil
}

// CheckTenantReady requires bounded typed facts and every referenced policy to
// have its complete, compatible published index before a tenant is served.
func (s *Store) CheckTenantReady(ctx context.Context, tenant string) error {
	if !validID(tenant) {
		return ErrInvalidArgument
	}
	rows, err := s.pool.Query(ctx, `select t.body, p.body, i.profile, i.expected_chunks,
 (select count(*) from business.policy_chunks c
  where c.tenant_id = t.tenant_id and c.index_id = i.index_id)
 from business.tickets t
 left join business.policy_versions p on p.tenant_id = t.tenant_id
 and p.policy_version = t.body ->> 'policy_version'
 left join lateral (
  select x.index_id, x.profile, x.expected_chunks from business.policy_indexes x
  where x.tenant_id = t.tenant_id and x.policy_version = p.policy_version
  and x.corpus_sha256 = p.corpus_sha256 and x.published_at is not null
  order by x.published_at desc, x.index_id limit 1
 ) i on true
 where t.tenant_id = $1 order by t.ticket_id limit 41`, tenant)
	if err != nil {
		return ErrDependencyUnavailable
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var ticketJSON, policyJSON, profileJSON []byte
		var expected *int
		var actual int
		if err := rows.Scan(&ticketJSON, &policyJSON, &profileJSON, &expected, &actual); err != nil {
			return ErrDependencyUnavailable
		}
		var ticket Ticket
		var policy PolicyVersion
		var profile IndexProfile
		if count > 40 || json.Unmarshal(ticketJSON, &ticket) != nil || json.Unmarshal(policyJSON, &policy) != nil ||
			json.Unmarshal(profileJSON, &profile) != nil || expected == nil || *expected != actual || actual < 1 || actual > 64 ||
			ticket.TenantID != tenant || !validID(ticket.TicketID) || ticket.Revision < 1 || ticket.ObservedAt.IsZero() ||
			!validID(ticket.Status) || !validText(ticket.Subject, 256) || !validText(ticket.Description, 1024) ||
			(ticket.OrderID != nil && !validID(*ticket.OrderID)) ||
			policy.TenantID != tenant || policy.PolicyVersion != ticket.PolicyVersion || policy.Revision < 1 ||
			!validProfile(profile) || profile.PolicyVersion != policy.PolicyVersion || profile.CorpusSHA256 != policy.CorpusSHA256 {
			return ErrDependencyUnavailable
		}
	}
	if rows.Err() != nil || count == 0 {
		return ErrDependencyUnavailable
	}
	return nil
}

func publicDBError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrConflict
		case "40001", "40P01", "57014", "53300", "57P01", "08006":
			return ErrDependencyUnavailable
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrDependencyUnavailable
	}
	return ErrInternal
}

func validUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func readJSON(ctx context.Context, tx pgx.Tx, query string, args []any, value any) error {
	var body []byte
	if err := tx.QueryRow(ctx, query, args...).Scan(&body); err != nil {
		return err
	}
	if err := json.Unmarshal(body, value); err != nil {
		return ErrInternal
	}
	return nil
}

// CreateSnapshot captures all facts from one repeatable-read database view.
// The boolean reports reuse, including a winner of a concurrent request.
func (s *Store) CreateSnapshot(ctx context.Context, tenant string, req SnapshotRequest) (*Snapshot, bool, error) {
	if !validID(tenant) || req.SchemaVersion != SchemaVersion || !validID(req.TicketID) || !validID(req.RequestKey) {
		return nil, false, ErrInvalidArgument
	}
	requestHash := fingerprint(struct {
		Version int
		Ticket  string
	}{req.SchemaVersion, req.TicketID})
	for range 4 {
		snapshot, reused, err := s.createSnapshotOnce(ctx, tenant, req, requestHash)
		if err == nil {
			return snapshot, reused, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "23505" || pgErr.Code == "40P01") {
			if ctx.Err() != nil {
				return nil, false, ErrDependencyUnavailable
			}
			continue
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, ErrDependencyUnavailable) || errors.Is(err, ErrInvalidArgument) {
			return nil, false, err
		}
		return nil, false, publicDBError(err)
	}
	// A new read after bounded transaction retries may observe the winning insert.
	var body []byte
	var storedHash string
	err := s.pool.QueryRow(ctx, `select request_hash, body from business.snapshots where tenant_id = $1 and request_key = $2`, tenant, req.RequestKey).Scan(&storedHash, &body)
	if err != nil {
		return nil, false, ErrDependencyUnavailable
	}
	if storedHash != requestHash {
		return nil, false, ErrConflict
	}
	var snapshot Snapshot
	if json.Unmarshal(body, &snapshot) != nil {
		return nil, false, ErrInternal
	}
	return &snapshot, true, nil
}

func (s *Store) createSnapshotOnce(ctx context.Context, tenant string, req SnapshotRequest, hash string) (*Snapshot, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var existing []byte
	var existingHash string
	err = tx.QueryRow(ctx, `select request_hash, body from business.snapshots where tenant_id = $1 and request_key = $2`, tenant, req.RequestKey).Scan(&existingHash, &existing)
	if err == nil {
		if existingHash != hash {
			return nil, false, ErrConflict
		}
		var snapshot Snapshot
		if json.Unmarshal(existing, &snapshot) != nil {
			return nil, false, ErrInternal
		}
		return &snapshot, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	snapshot := &Snapshot{ID: uuid.NewString(), TenantID: tenant, SchemaVersion: SchemaVersion}
	if err := tx.QueryRow(ctx, `select transaction_timestamp()`).Scan(&snapshot.CreatedAt); err != nil {
		return nil, false, err
	}
	err = readJSON(ctx, tx, `select body from business.tickets where tenant_id = $1 and ticket_id = $2`, []any{tenant, req.TicketID}, &snapshot.Ticket)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	snapshot.AsOf = snapshot.Ticket.ObservedAt
	if snapshot.Ticket.OrderID != nil {
		var order Order
		err = readJSON(ctx, tx, `select body from business.orders where tenant_id = $1 and order_id = $2`, []any{tenant, *snapshot.Ticket.OrderID}, &order)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, err
		}
		if err == nil {
			snapshot.Order = &order
			if order.DeliveryID != nil {
				var delivery Delivery
				err = readJSON(ctx, tx, `select body from business.deliveries where tenant_id = $1 and delivery_id = $2 and order_id = $3`, []any{tenant, *order.DeliveryID, order.OrderID}, &delivery)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return nil, false, err
				}
				if err == nil {
					snapshot.Delivery = &delivery
				}
			}
		}
	}
	err = readJSON(ctx, tx, `select body from business.policy_versions where tenant_id = $1 and policy_version = $2`, []any{tenant, snapshot.Ticket.PolicyVersion}, &snapshot.Policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrDependencyUnavailable
	}
	if err != nil {
		return nil, false, err
	}
	var profile []byte
	err = tx.QueryRow(ctx, `select index_id::text, profile_hash, content_hash, profile, published_at
 from business.policy_indexes where tenant_id = $1 and policy_version = $2
 and corpus_sha256 = $3 and published_at is not null order by published_at desc, index_id limit 1`,
		tenant, snapshot.Policy.PolicyVersion, snapshot.Policy.CorpusSHA256).Scan(&snapshot.Index.ID, &snapshot.Index.ProfileHash, &snapshot.Index.ContentHash, &profile, &snapshot.Index.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrDependencyUnavailable
	}
	if err != nil {
		return nil, false, err
	}
	if json.Unmarshal(profile, &snapshot.Index.Profile) != nil || !validProfile(snapshot.Index.Profile) ||
		snapshot.Index.Profile.PolicyVersion != snapshot.Policy.PolicyVersion || snapshot.Index.Profile.CorpusSHA256 != snapshot.Policy.CorpusSHA256 {
		return nil, false, ErrDependencyUnavailable
	}
	snapshot.Index.TenantID = tenant
	snapshot.ContentHash = fingerprint(snapshot)
	body, err := json.Marshal(snapshot)
	if err != nil || len(body) > maxSnapshotBytes {
		return nil, false, ErrInvalidArgument
	}
	_, err = tx.Exec(ctx, `insert into business.snapshots
 (snapshot_id, tenant_id, request_key, request_hash, index_id, content_hash, body)
 values ($1, $2, $3, $4, $5, $6, $7)`, snapshot.ID, tenant, req.RequestKey, hash, snapshot.Index.ID, snapshot.ContentHash, body)
	if err != nil {
		return nil, false, err
	}
	return snapshot, false, tx.Commit(ctx)
}

// GetSnapshot never consults mutable source facts.
func (s *Store) GetSnapshot(ctx context.Context, tenant, id string) (*Snapshot, error) {
	if !validID(tenant) || !validUUID(id) {
		return nil, ErrNotFound
	}
	var body []byte
	if err := s.pool.QueryRow(ctx, `select body from business.snapshots where tenant_id = $1 and snapshot_id = $2`, tenant, id).Scan(&body); err != nil {
		return nil, publicDBError(err)
	}
	var snapshot Snapshot
	if json.Unmarshal(body, &snapshot) != nil {
		return nil, ErrInternal
	}
	return &snapshot, nil
}

// GetEvidence resolves only an allowlisted kind in the authorized snapshot.
func (s *Store) GetEvidence(ctx context.Context, tenant, id, kind string) (*Evidence, error) {
	if kind != "ticket" && kind != "order" && kind != "delivery" {
		return nil, ErrNotFound
	}
	snapshot, err := s.GetSnapshot(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	result := &Evidence{SnapshotID: id, Kind: kind, EvidenceRef: fmt.Sprintf("business-evidence:%s:%s", id, kind)}
	switch kind {
	case "ticket":
		result.Ticket = &snapshot.Ticket
	case "order":
		result.Order = snapshot.Order
		if snapshot.Order == nil {
			result.Missing, result.MissingReason = true, "record_not_found"
			if snapshot.Ticket.OrderID == nil {
				result.MissingReason = "not_associated"
			}
		}
	case "delivery":
		result.Delivery = snapshot.Delivery
		if snapshot.Delivery == nil {
			result.Missing, result.MissingReason = true, "record_not_found"
			if snapshot.Order == nil || snapshot.Order.DeliveryID == nil {
				result.MissingReason = "not_associated"
			}
		}
	}
	return result, nil
}

// GetOrder returns the snapshot's order copy or an explicit missing fact.
func (s *Store) GetOrder(ctx context.Context, tenant, id string) (*OrderEvidence, error) {
	return s.GetEvidence(ctx, tenant, id, "order")
}

// GetDelivery returns the snapshot's authorized delivery aggregate copy.
func (s *Store) GetDelivery(ctx context.Context, tenant, id string) (*DeliveryEvidence, error) {
	return s.GetEvidence(ctx, tenant, id, "delivery")
}

// SearchPolicies performs exact cosine search, with stable ties and fixed top-k.
func (s *Store) SearchPolicies(ctx context.Context, tenant, id string, req SearchRequest) ([]PolicyHit, error) {
	if req.EmbeddingModel != EmbeddingModel || req.EmbeddingDigest != EmbeddingDigest {
		return nil, ErrProfileUnavailable
	}
	vector, err := vectorLiteral(req.QueryVector)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.GetSnapshot(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `select c.chunk_id, c.source, c.body,
 c.embedding operator(extensions.<=>) $3::extensions.vector(384) as distance
 from business.policy_chunks c join business.policy_indexes i
 on i.tenant_id = c.tenant_id and i.index_id = c.index_id
 where c.tenant_id = $1 and c.index_id = $2 and i.published_at is not null
 order by distance, c.chunk_id limit 3`, tenant, snapshot.Index.ID, vector)
	if err != nil {
		return nil, publicDBError(err)
	}
	defer rows.Close()
	hits := make([]PolicyHit, 0, 3)
	for rows.Next() {
		hit := policyHit(snapshot, "")
		var distance float64
		if err := rows.Scan(&hit.ChunkID, &hit.Source, &hit.Text, &distance); err != nil {
			return nil, ErrInternal
		}
		hit.EvidenceRef = fmt.Sprintf("business-policy:%s:%s", hit.IndexID, hit.ChunkID)
		hit.Distance = &distance
		hits = append(hits, hit)
	}
	if rows.Err() != nil {
		return nil, ErrDependencyUnavailable
	}
	if len(hits) == 0 {
		return nil, ErrDependencyUnavailable
	}
	return hits, nil
}

func policyHit(snapshot *Snapshot, chunk string) PolicyHit {
	return PolicyHit{IndexID: snapshot.Index.ID, ChunkID: chunk, PolicyVersion: snapshot.Policy.PolicyVersion,
		EvidenceRef: fmt.Sprintf("business-policy:%s:%s", snapshot.Index.ID, chunk)}
}

// GetPolicy cannot resolve a chunk from an index outside the snapshot binding.
func (s *Store) GetPolicy(ctx context.Context, tenant, id, chunk string) (*PolicyHit, error) {
	if !validID(chunk) {
		return nil, ErrNotFound
	}
	snapshot, err := s.GetSnapshot(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	hit := policyHit(snapshot, chunk)
	err = s.pool.QueryRow(ctx, `select source, body from business.policy_chunks where tenant_id = $1 and index_id = $2 and chunk_id = $3`, tenant, snapshot.Index.ID, chunk).Scan(&hit.Source, &hit.Text)
	if err != nil {
		return nil, publicDBError(err)
	}
	return &hit, nil
}
