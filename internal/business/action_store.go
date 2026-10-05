package business

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const actionSourceGuard int64 = 7483921656

// TrustedActionKey comes from deployment, never from a signed request.
type TrustedActionKey struct {
	PublicKey ed25519.PublicKey
	Tenants   map[string]bool
}

func lockActionSources(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, actionSourceGuard)
	return err
}

// CheckActionRole verifies separate low-privilege writer and receipt pools.
func (s *Store) CheckActionRole(ctx context.Context, writer bool) error {
	role := "jobforge_business_action_reader"
	if writer {
		role = "jobforge_business_writer"
	}
	var allowed bool
	err := s.pool.QueryRow(ctx, `select not r.rolsuper and not r.rolcreatedb
 and not r.rolcreaterole and not r.rolreplication and not r.rolbypassrls
 and pg_has_role(current_user,$1,'MEMBER')
 and not pg_has_role(current_user,'jobforge_business_owner','MEMBER')
 and not pg_has_role(current_user,'jobforge_business_loader','MEMBER')
 and not pg_has_role(current_user,'jobforge_business_runtime','MEMBER')
 and current_schemas(false)=array['pg_catalog']::name[]
 and not has_database_privilege(current_user,current_database(),'CREATE')
 and not has_schema_privilege(current_user,'business','CREATE')
 and not has_schema_privilege(current_user,'public','CREATE')
 and not has_table_privilege(current_user,'business.tickets','INSERT,DELETE')
 and not exists(select 1 from pg_catalog.pg_attribute a
   where a.attrelid='business.tickets'::regclass and a.attnum>0 and not a.attisdropped
   and a.attname not in ('revision','body')
   and has_column_privilege(current_user,'business.tickets',a.attname,'UPDATE'))
 and not has_table_privilege(current_user,'business.orders','INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user,'business.deliveries','INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user,'business.policy_versions','INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user,'business.policy_indexes','INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user,'business.policy_chunks','INSERT,UPDATE,DELETE')
 and not has_table_privilege(current_user,'business.snapshots','INSERT,UPDATE,DELETE')
 and not exists(select 1 from unnest(array['business.tickets','business.orders','business.deliveries',
   'business.policy_versions','business.policy_indexes','business.policy_chunks','business.snapshots',
   'business.ticket_resolutions']) as restricted(table_name)
   where has_table_privilege(current_user,restricted.table_name,'TRUNCATE'))
 and not has_table_privilege(current_user,'business.ticket_resolutions','UPDATE,DELETE')
 and has_table_privilege(current_user,'business.ticket_resolutions','SELECT')
 and case when $2 then
   has_column_privilege(current_user,'business.tickets','revision','UPDATE')
   and has_column_privilege(current_user,'business.tickets','body','UPDATE')
   and has_table_privilege(current_user,'business.ticket_resolutions','INSERT')
 else
   not has_column_privilege(current_user,'business.tickets','revision','UPDATE')
   and not has_column_privilege(current_user,'business.tickets','body','UPDATE')
   and not has_table_privilege(current_user,'business.ticket_resolutions','INSERT')
 end
 from pg_catalog.pg_roles r where r.rolname=current_user`, role, writer).Scan(&allowed)
	if err != nil {
		return ErrDependencyUnavailable
	}
	if !allowed {
		return ErrWrongDatabase
	}
	return s.CheckReady(ctx)
}

// Receipt returns immutable evidence. No source read or expiry test precedes it.
func (s *Store) Receipt(ctx context.Context, tenant, operationID string) (ActionReceipt, error) {
	var receipt ActionReceipt
	if !validID(tenant) || !validUUID(operationID) {
		return receipt, ErrInvalidArgument
	}
	var body []byte
	err := s.pool.QueryRow(ctx, `select receipt from business.ticket_resolutions
 where tenant_id=$1 and operation_id=$2`, tenant, operationID).Scan(&body)
	if err != nil {
		return receipt, publicDBError(err)
	}
	if json.Unmarshal(body, &receipt) != nil || receipt.ReceiptHash != receipt.Hash() {
		return receipt, ErrInternal
	}
	return receipt, nil
}

// ApplyResolution uses a READ COMMITTED view acquired after the statement guard.
// All source mutations acquire the same guard before row locks, including missing
// relationship inserts. HTTP and control-plane transactions stay outside it.
func (s *Store) ApplyResolution(ctx context.Context, tenant string, action SignedAction, trusted map[string]TrustedActionKey) (ActionReceipt, error) {
	var receipt ActionReceipt
	a := action.Authorization
	if tenant != a.TenantID {
		return receipt, ErrNotFound
	}
	authHash, err := a.Hash()
	if err != nil {
		return receipt, err
	}
	parameterHash, err := action.Parameters.Hash()
	if err != nil {
		return receipt, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return receipt, publicDBError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `set local lock_timeout='5s'; set local statement_timeout='10s'`); err != nil {
		return receipt, publicDBError(err)
	}
	if err = lockActionSources(ctx, tx); err != nil {
		return receipt, publicDBError(err)
	}
	var body []byte
	var storedHash string
	err = tx.QueryRow(ctx, `select authorization_hash,receipt from business.ticket_resolutions
 where tenant_id=$1 and operation_id=$2`, tenant, a.OperationID).Scan(&storedHash, &body)
	if err == nil {
		if storedHash != authHash || parameterHash != a.ParametersHash || json.Unmarshal(body, &receipt) != nil || receipt.Validate(action) != nil {
			return ActionReceipt{}, ErrActionConflict
		}
		return receipt, publicCommit(ctx, tx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return receipt, publicDBError(err)
	}
	key, ok := trusted[a.KeyID]
	if !ok || !key.Tenants[tenant] || action.Verify(key.PublicKey) != nil {
		return receipt, ErrActionForbidden
	}
	ticket, err := currentActionTicket(ctx, tx, action)
	if err != nil {
		return receipt, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		return receipt, publicDBError(err)
	}
	if now.UnixMicro() >= a.AuthorizationExpiresAt {
		return receipt, ErrAuthorizationExpired
	}
	if now.UnixMicro() < a.AuthorizedAt {
		return receipt, ErrActionForbidden
	}
	before := ticket.Revision
	ticket.Revision++
	if action.Parameters.Action != "record_conclusion" {
		ticket.Status = action.Parameters.TargetTicketStatus
	}
	updated, _ := json.Marshal(ticket)
	if _, err = tx.Exec(ctx, `update business.tickets set revision=$3,body=$4
 where tenant_id=$1 and ticket_id=$2`, tenant, ticket.TicketID, ticket.Revision, updated); err != nil {
		return receipt, publicDBError(err)
	}
	receipt = ActionReceipt{SchemaVersion: 1, TenantID: tenant, OperationID: a.OperationID,
		BusinessRequestID: a.BusinessRequestID, AuthorizationHash: authHash, ProposalHash: a.ProposalHash,
		ParametersHash: a.ParametersHash, ApprovalID: a.ApprovalID, TicketID: ticket.TicketID,
		BeforeRevision: before, AfterRevision: ticket.Revision, TicketStatus: ticket.Status,
		AppliedAt: now.UnixMicro(), RetainUntil: now.Add(ActionReceiptRetention).UnixMicro()}
	receipt.ReceiptHash = receipt.Hash()
	parameters, _ := actionJSON(action.Parameters)
	body, _ = json.Marshal(receipt)
	_, err = tx.Exec(ctx, `insert into business.ticket_resolutions
 (tenant_id,operation_id,business_request_id,ticket_id,authorization_hash,parameters,receipt,applied_at,retain_until)
 values($1,$2,$3,$4,$5,$6,$7,$8,$9)`, tenant, a.OperationID, a.BusinessRequestID, ticket.TicketID,
		authHash, parameters, body, now, now.Add(ActionReceiptRetention))
	if err != nil {
		if errors.Is(publicDBError(err), ErrConflict) {
			return ActionReceipt{}, ErrActionConflict
		}
		return ActionReceipt{}, publicDBError(err)
	}
	return receipt, publicCommit(ctx, tx)
}

func currentActionTicket(ctx context.Context, tx pgx.Tx, action SignedAction) (Ticket, error) {
	a := action.Authorization
	var snapshot Snapshot
	err := readJSON(ctx, tx, `select body from business.snapshots where tenant_id=$1 and snapshot_id=$2`, []any{a.TenantID, a.SnapshotID}, &snapshot)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Ticket{}, ErrActionConflict
		}
		return Ticket{}, publicDBError(err)
	}
	vector, _ := actionJSON(a.VersionVector)
	captured, _ := actionJSON(snapshot.VersionVector())
	if snapshot.ContentHash != a.SnapshotHash || snapshot.TenantID != a.TenantID || !bytes.Equal(vector, captured) ||
		snapshot.Ticket.TicketID != action.Parameters.TicketID ||
		(action.Parameters.Action == "record_conclusion" && action.Parameters.TargetTicketStatus != snapshot.Ticket.Status) {
		return Ticket{}, ErrActionConflict
	}
	current := Snapshot{}
	if err = readJSON(ctx, tx, `select body from business.tickets where tenant_id=$1 and ticket_id=$2`, []any{a.TenantID, action.Parameters.TicketID}, &current.Ticket); err != nil {
		return Ticket{}, actionFactError(err)
	}
	if current.Ticket.OrderID != nil {
		var order Order
		err = readJSON(ctx, tx, `select body from business.orders where tenant_id=$1 and order_id=$2`, []any{a.TenantID, *current.Ticket.OrderID}, &order)
		if err == nil {
			current.Order = &order
			if order.DeliveryID != nil {
				var delivery Delivery
				err = readJSON(ctx, tx, `select body from business.deliveries where tenant_id=$1 and delivery_id=$2`, []any{a.TenantID, *order.DeliveryID}, &delivery)
				if err == nil {
					if delivery.OrderID != order.OrderID {
						return Ticket{}, ErrActionConflict
					}
					current.Delivery = &delivery
				}
			}
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Ticket{}, publicDBError(err)
		}
	}
	if err = readJSON(ctx, tx, `select body from business.policy_versions where tenant_id=$1 and policy_version=$2`, []any{a.TenantID, current.Ticket.PolicyVersion}, &current.Policy); err != nil {
		return Ticket{}, actionFactError(err)
	}
	err = tx.QueryRow(ctx, `select index_id::text,profile_hash,content_hash from business.policy_indexes
 where tenant_id=$1 and index_id=$2 and policy_version=$3 and corpus_sha256=$4 and published_at is not null`,
		a.TenantID, a.VersionVector.Index.ID, current.Policy.PolicyVersion, current.Policy.CorpusSHA256).
		Scan(&current.Index.ID, &current.Index.ProfileHash, &current.Index.ContentHash)
	if err != nil {
		return Ticket{}, actionFactError(err)
	}
	actual, _ := actionJSON(current.VersionVector())
	if !bytes.Equal(actual, vector) {
		return Ticket{}, ErrActionConflict
	}
	return current.Ticket, nil
}

func actionFactError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrActionConflict
	}
	return publicDBError(err)
}
