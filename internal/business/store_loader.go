package business

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ValidateDataset checks bounded runtime facts before any transaction starts.
func ValidateDataset(dataset Dataset) error {
	if dataset.SchemaVersion != SchemaVersion || !validID(dataset.DatasetVersion) || len(dataset.Policies) == 0 || len(dataset.Policies) > 8 ||
		len(dataset.Tickets) == 0 || len(dataset.Tickets) > 40 || len(dataset.Orders) > 40 || len(dataset.Deliveries) > 40 {
		return ErrInvalidArgument
	}
	seen := make(map[string]bool)
	checkKey := func(kind, tenant, id string) bool {
		key := kind + "/" + tenant + "/" + id
		if !validID(tenant) || !validID(id) || seen[key] {
			return false
		}
		seen[key] = true
		return true
	}
	for _, p := range dataset.Policies {
		if !checkKey("policy", p.TenantID, p.PolicyVersion) || p.Revision < 1 || !validHash(p.CorpusSHA256) {
			return ErrInvalidArgument
		}
	}
	for _, ticket := range dataset.Tickets {
		if !checkKey("ticket", ticket.TenantID, ticket.TicketID) || ticket.Revision < 1 || ticket.ObservedAt.IsZero() || !validID(ticket.PolicyVersion) ||
			(ticket.OrderID != nil && !validID(*ticket.OrderID)) || !validText(ticket.Subject, 256) || !validText(ticket.Description, 1024) || !validID(ticket.Status) ||
			!seen["policy/"+ticket.TenantID+"/"+ticket.PolicyVersion] {
			return ErrInvalidArgument
		}
	}
	for _, order := range dataset.Orders {
		if !checkKey("order", order.TenantID, order.OrderID) || order.Revision < 1 || !validID(order.Status) ||
			(order.DeliveryID != nil && !validID(*order.DeliveryID)) || order.OrderedAt.IsZero() || order.PromisedDeliveryAt.IsZero() {
			return ErrInvalidArgument
		}
	}
	for _, delivery := range dataset.Deliveries {
		if !checkKey("delivery", delivery.TenantID, delivery.DeliveryID) || delivery.AggregateRevision < 1 || !validID(delivery.OrderID) || !validID(delivery.Status) || len(delivery.Events) > 12 {
			return ErrInvalidArgument
		}
		events := make(map[string]bool)
		for _, event := range delivery.Events {
			if !validID(event.EventID) || events[event.EventID] || event.OccurredAt.IsZero() || !validID(event.Status) || !validText(event.Note, 256) {
				return ErrInvalidArgument
			}
			events[event.EventID] = true
		}
		body, _ := json.Marshal(delivery)
		if len(body) > 3072 {
			return ErrInvalidArgument
		}
	}
	return nil
}

// ImportDataset is a trusted, all-or-nothing seed operation. Reimport never
// overwrites source changes made after the first successful import.
func (s *Store) ImportDataset(ctx context.Context, dataset Dataset) error {
	if err := ValidateDataset(dataset); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return publicDBError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	hash := fingerprint(dataset)
	var storedHash string
	err = tx.QueryRow(ctx, `insert into business.dataset_imports (dataset_version, content_hash)
 values ($1, $2) on conflict (dataset_version) do nothing returning content_hash`, dataset.DatasetVersion, hash).Scan(&storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `select content_hash from business.dataset_imports where dataset_version = $1`, dataset.DatasetVersion).Scan(&storedHash)
		if err != nil {
			return publicDBError(err)
		}
		if hash != storedHash {
			return ErrConflict
		}
		return publicCommit(ctx, tx)
	}
	if err != nil {
		return publicDBError(err)
	}
	for _, p := range dataset.Policies {
		body, _ := json.Marshal(p)
		if err := insertSource(ctx, tx, `insert into business.policy_versions (tenant_id, policy_version, revision, corpus_sha256, body)
 values ($1, $2, $3, $4, $5) on conflict (tenant_id, policy_version) do nothing`,
			`select body from business.policy_versions where tenant_id = $1 and policy_version = $2`, p.TenantID, p.PolicyVersion, body, p.Revision, p.CorpusSHA256); err != nil {
			return err
		}
	}
	for _, ticket := range dataset.Tickets {
		body, _ := json.Marshal(ticket)
		if err := insertSource(ctx, tx, `insert into business.tickets (tenant_id, ticket_id, revision, body)
 values ($1, $2, $3, $4) on conflict (tenant_id, ticket_id) do nothing`,
			`select body from business.tickets where tenant_id = $1 and ticket_id = $2`, ticket.TenantID, ticket.TicketID, body, ticket.Revision); err != nil {
			return err
		}
	}
	for _, order := range dataset.Orders {
		body, _ := json.Marshal(order)
		if err := insertSource(ctx, tx, `insert into business.orders (tenant_id, order_id, revision, body)
 values ($1, $2, $3, $4) on conflict (tenant_id, order_id) do nothing`,
			`select body from business.orders where tenant_id = $1 and order_id = $2`, order.TenantID, order.OrderID, body, order.Revision); err != nil {
			return err
		}
	}
	for _, delivery := range dataset.Deliveries {
		body, _ := json.Marshal(delivery)
		if err := insertSource(ctx, tx, `insert into business.deliveries (tenant_id, delivery_id, aggregate_revision, order_id, body)
 values ($1, $2, $3, $4, $5) on conflict (tenant_id, delivery_id) do nothing`,
			`select body from business.deliveries where tenant_id = $1 and delivery_id = $2`, delivery.TenantID, delivery.DeliveryID, body, delivery.AggregateRevision, delivery.OrderID); err != nil {
			return err
		}
	}
	return publicCommit(ctx, tx)
}

func insertSource(ctx context.Context, tx pgx.Tx, insert, lookup, tenant, id string, body []byte, fields ...any) error {
	args := append([]any{tenant, id}, fields...)
	args = append(args, body)
	tag, err := tx.Exec(ctx, insert, args...)
	if err != nil {
		return publicDBError(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existing []byte
	if err := tx.QueryRow(ctx, lookup, tenant, id).Scan(&existing); err != nil {
		return publicDBError(err)
	}
	oldHash, oldErr := sourceFingerprint(existing)
	newHash, newErr := sourceFingerprint(body)
	if oldErr != nil || newErr != nil {
		return ErrInternal
	}
	if oldHash != newHash {
		return ErrConflict
	}
	return nil
}

func sourceFingerprint(body []byte) (string, error) {
	// JSONB may reorder object keys. Preserve bigint revisions while normalizing
	// object order; decoding numbers as float64 could collapse distinct versions.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", ErrInternal
	}
	return fingerprint(value), nil
}

func publicCommit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return publicDBError(err)
	}
	return nil
}

// ValidateIndexUpload rejects unsupported profiles and malformed vectors.
func ValidateIndexUpload(upload IndexUpload) error {
	if upload.SchemaVersion != SchemaVersion || !validID(upload.TenantID) || (upload.PrepareID != "" && !validUUID(upload.PrepareID)) || len(upload.Chunks) == 0 || len(upload.Chunks) > 64 {
		return ErrInvalidArgument
	}
	if !validProfile(upload.Profile) {
		return ErrProfileUnavailable
	}
	seen := make(map[string]bool)
	total := 0
	for _, chunk := range upload.Chunks {
		if !validID(chunk.ChunkID) || seen[chunk.ChunkID] || !validText(chunk.Source, 128) || chunk.Source == "" || !validText(chunk.Text, 768) || chunk.Text == "" {
			return ErrInvalidArgument
		}
		seen[chunk.ChunkID] = true
		total += len(chunk.Text)
		if _, err := vectorLiteral(chunk.Embedding); err != nil {
			return err
		}
	}
	if total > 64*1024 {
		return ErrInvalidArgument
	}
	return nil
}

// PublishIndex commits metadata and every vector atomically. The preparation
// report ID is provenance only and never changes the idempotency fingerprint.
func (s *Store) PublishIndex(ctx context.Context, upload IndexUpload) (*PublishedIndex, bool, error) {
	if err := ValidateIndexUpload(upload); err != nil {
		return nil, false, err
	}
	chunks := append([]IndexChunk(nil), upload.Chunks...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].ChunkID < chunks[j].ChunkID })
	contentHash := fingerprint(struct {
		Profile IndexProfile
		Chunks  []IndexChunk
	}{upload.Profile, chunks})
	profileHash := fingerprint(upload.Profile)
	profileJSON, _ := json.Marshal(upload.Profile)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, publicDBError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var corpus string
	err = tx.QueryRow(ctx, `select corpus_sha256 from business.policy_versions where tenant_id = $1 and policy_version = $2`, upload.TenantID, upload.Profile.PolicyVersion).Scan(&corpus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrDependencyUnavailable
	}
	if err != nil {
		return nil, false, publicDBError(err)
	}
	if corpus != upload.Profile.CorpusSHA256 {
		return nil, false, ErrConflict
	}
	index := &PublishedIndex{ID: uuid.NewString(), TenantID: upload.TenantID, Profile: upload.Profile, ProfileHash: profileHash, ContentHash: contentHash}
	tag, err := tx.Exec(ctx, `insert into business.policy_indexes
 (index_id, tenant_id, policy_version, corpus_sha256, profile_hash, content_hash, profile, expected_chunks)
 values ($1, $2, $3, $4, $5, $6, $7, $8) on conflict (tenant_id, profile_hash) do nothing`,
		index.ID, index.TenantID, upload.Profile.PolicyVersion, corpus, profileHash, contentHash, profileJSON, len(chunks))
	if err != nil {
		return nil, false, publicDBError(err)
	}
	if tag.RowsAffected() == 0 {
		var published *string
		err = tx.QueryRow(ctx, `select index_id::text, content_hash, published_at::text from business.policy_indexes where tenant_id = $1 and profile_hash = $2`, index.TenantID, profileHash).Scan(&index.ID, &index.ContentHash, &published)
		if err != nil {
			return nil, false, publicDBError(err)
		}
		if index.ContentHash != contentHash {
			return nil, false, ErrConflict
		}
		if published == nil {
			return nil, false, ErrDependencyUnavailable
		}
		if err := tx.QueryRow(ctx, `select published_at from business.policy_indexes where tenant_id = $1 and index_id = $2`, index.TenantID, index.ID).Scan(&index.PublishedAt); err != nil {
			return nil, false, publicDBError(err)
		}
		return index, true, publicCommit(ctx, tx)
	}
	for _, chunk := range chunks {
		vector, _ := vectorLiteral(chunk.Embedding)
		_, err = tx.Exec(ctx, `insert into business.policy_chunks (tenant_id, index_id, chunk_id, source, body, embedding)
 values ($1, $2, $3, $4, $5, $6::extensions.vector(384))`, index.TenantID, index.ID, chunk.ChunkID, chunk.Source, chunk.Text, vector)
		if err != nil {
			return nil, false, publicDBError(err)
		}
	}
	err = tx.QueryRow(ctx, `update business.policy_indexes set published_at = clock_timestamp()
 where tenant_id = $1 and index_id = $2 returning published_at`, index.TenantID, index.ID).Scan(&index.PublishedAt)
	if err != nil {
		return nil, false, publicDBError(err)
	}
	return index, false, publicCommit(ctx, tx)
}
