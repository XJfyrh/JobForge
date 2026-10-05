package business_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/business"
)

func actionPools(t *testing.T, admin *pgxpool.Pool) (*business.Store, *business.Store) {
	t.Helper()
	stores := make([]*business.Store, 0, 2)
	for _, user := range []string{"jobforge_business_writer_login", "jobforge_business_receipt_reader"} {
		config := admin.Config().Copy()
		config.ConnConfig.User, config.ConnConfig.Password = user, user
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal("cannot construct action pool")
		}
		t.Cleanup(pool.Close)
		stores = append(stores, business.NewStore(pool))
	}
	return stores[0], stores[1]
}

func authorizeFixture(ctx context.Context, t *testing.T, admin *pgxpool.Pool, snapshot *business.Snapshot) (business.SignedAction, map[string]business.TrustedActionKey) {
	t.Helper()
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	var now time.Time
	if err := admin.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	p := business.ResolutionParameters{TicketID: snapshot.Ticket.TicketID, Decision: "proposal", Action: "record_conclusion", Conclusion: "on_time",
		RequestedFields: []string{}, TargetTicketStatus: snapshot.Ticket.Status, Summary: "Synthetic approved conclusion", EvidenceRefs: []string{"business-evidence:" + snapshot.ID + ":ticket"},
	}
	p.Claims, _ = json.Marshal([]map[string]any{{"kind": "timing", "test": "delivered_not_late", "event_id": "event-1", "refs": []map[string]string{{"evidence_ref": p.EvidenceRefs[0], "source_pointer": "/ticket/status"}}}})
	hash, err := p.Hash()
	if err != nil {
		t.Fatal(err)
	}
	a := business.ActionAuthorization{SchemaVersion: 1, KeyID: "test-action-key", Operation: business.ResolutionOperation,
		TenantID: snapshot.TenantID, BusinessRequestID: uuid.NewString(), BusinessRequestCreatedAt: now.Add(-time.Minute).UnixMicro(),
		OperationID: uuid.NewString(), RunID: uuid.NewString(), ApprovalID: uuid.NewString(), ActorID: "test-approver",
		DecidedAt: now.Add(-time.Second).UnixMicro(), ProposalHash: business.ActionFingerprint("fixture.proposal", "1"), ParametersHash: hash,
		SnapshotID: snapshot.ID, SnapshotHash: snapshot.ContentHash, VersionVector: snapshot.VersionVector(), AuthorizedAt: now.UnixMicro(),
		PermissionExpiresAt: now.Add(time.Hour).UnixMicro(), AuthorizationExpiresAt: now.Add(time.Hour).UnixMicro(), RunDeadline: now.Add(time.Hour).UnixMicro()}
	action, err := business.SignAction(a, p, private)
	if err != nil {
		t.Fatal(err)
	}
	return action, map[string]business.TrustedActionKey{a.KeyID: {PublicKey: private.Public().(ed25519.PublicKey), Tenants: map[string]bool{a.TenantID: true}}}
}

func actionFixture(t *testing.T) (context.Context, *pgxpool.Pool, *pgxpool.Pool, *business.Store, *business.Store, business.SignedAction, map[string]business.TrustedActionKey) {
	t.Helper()
	ctx, admin, loaderPool, runtimePool := isolatedPools(t)
	dataset, upload := fixture()
	seedConsistencyFixture(ctx, t, business.NewStore(loaderPool), dataset, upload)
	snapshot, _, err := business.NewStore(runtimePool).CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{SchemaVersion: 1, TicketID: "ticket-1", RequestKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	action, keys := authorizeFixture(ctx, t, admin, snapshot)
	writer, reader := actionPools(t, admin)
	return ctx, admin, loaderPool, writer, reader, action, keys
}

func TestBusinessActionAtomicDedupAndPermissions(t *testing.T) {
	ctx, admin, _, writer, reader, action, keys := actionFixture(t)
	if writer.CheckActionRole(ctx, true) != nil || reader.CheckActionRole(ctx, false) != nil || writer.CheckActionRole(ctx, false) == nil || reader.CheckActionRole(ctx, true) == nil {
		t.Fatal("action role isolation failed")
	}
	if _, err := admin.Exec(ctx, `grant update on business.tickets to jobforge_business_writer`); err != nil {
		t.Fatal(err)
	}
	if writer.CheckActionRole(ctx, true) == nil {
		t.Fatal("whole-table ticket update privilege accepted")
	}
	if _, err := admin.Exec(ctx, `revoke update on business.tickets from jobforge_business_writer`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `grant update (revision, body) on business.tickets to jobforge_business_writer`); err != nil {
		t.Fatal(err)
	}
	if writer.CheckActionRole(ctx, true) != nil {
		t.Fatal("restricted writer did not recover")
	}
	for _, table := range []string{"ticket_resolutions", "snapshots", "tickets", "policy_chunks"} {
		if _, err := admin.Exec(ctx, "grant truncate on business."+table+" to jobforge_business_writer"); err != nil {
			t.Fatal(err)
		}
		if writer.CheckActionRole(ctx, true) == nil {
			t.Fatal("truncate privilege accepted", table)
		}
		if _, err := admin.Exec(ctx, "revoke truncate on business."+table+" from jobforge_business_writer"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, "grant insert on business.policy_chunks to jobforge_business_writer"); err != nil {
		t.Fatal(err)
	}
	if writer.CheckActionRole(ctx, true) == nil {
		t.Fatal("policy chunk write accepted")
	}
	if _, err := admin.Exec(ctx, "revoke insert on business.policy_chunks from jobforge_business_writer"); err != nil {
		t.Fatal(err)
	}
	var joined sync.WaitGroup
	results := make(chan business.ActionReceipt, 12)
	failures := make(chan error, 12)
	for range 12 {
		joined.Add(1)
		go func() {
			defer joined.Done()
			r, err := writer.ApplyResolution(ctx, action.Authorization.TenantID, action, keys)
			results <- r
			failures <- err
		}()
	}
	joined.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("same-operation repeat failed: %v", err)
		}
	}
	var first business.ActionReceipt
	for r := range results {
		if first.ReceiptHash == "" {
			first = r
		}
		if r.ReceiptHash != first.ReceiptHash || r.Validate(action) != nil {
			t.Fatal("duplicate did not return the first receipt")
		}
	}
	var revision, count int
	if err := admin.QueryRow(ctx, `select revision,(select count(*) from business.ticket_resolutions)
 from business.tickets where tenant_id=$1 and ticket_id=$2`, action.Authorization.TenantID, action.Parameters.TicketID).Scan(&revision, &count); err != nil || revision != 2 || count != 1 {
		t.Fatalf("duplicate effect: revision=%d count=%d err=%v", revision, count, err)
	}
	stored, err := reader.Receipt(ctx, action.Authorization.TenantID, action.Authorization.OperationID)
	if err != nil || stored.ReceiptHash != first.ReceiptHash {
		t.Fatal("receipt reader lost first effect")
	}
	if _, err := reader.Receipt(ctx, "tenant-south", action.Authorization.OperationID); !errors.Is(err, business.ErrNotFound) {
		t.Fatal("receipt crossed tenant boundary")
	}
	changed := action
	changed.Authorization.ActorID = "another-approver"
	if _, err := writer.ApplyResolution(ctx, action.Authorization.TenantID, changed, keys); !errors.Is(err, business.ErrActionConflict) {
		t.Fatalf("same operation changed content: %v", err)
	}
	if err := business.NewMigrator(admin).Down(ctx); err == nil {
		t.Fatal("destructive rollback erased an applied receipt")
	}
}

func TestBusinessActionRejectsEveryChangedDependency(t *testing.T) {
	changes := map[string]string{
		"ticket":         `update business.tickets set revision=revision+1,body=jsonb_set(body,'{revision}',to_jsonb(revision+1)) where tenant_id='tenant-north'`,
		"order":          `update business.orders set revision=revision+1,body=jsonb_set(body,'{revision}',to_jsonb(revision+1)) where tenant_id='tenant-north'`,
		"delivery-event": `update business.deliveries set aggregate_revision=aggregate_revision+1,body=jsonb_set(jsonb_set(body,'{aggregate_revision}',to_jsonb(aggregate_revision+1)),'{events}',body->'events'||'{"event_id":"new-event","occurred_at":"2026-10-05T00:00:00Z","status":"delivered","note":"new fact"}'::jsonb) where tenant_id='tenant-north'`,
		"policy":         `update business.policy_versions set revision=revision+1,body=jsonb_set(body,'{revision}',to_jsonb(revision+1)) where tenant_id='tenant-north'`,
		"relationship":   `update business.orders set revision=revision+1,body=jsonb_set(jsonb_set(body,'{revision}',to_jsonb(revision+1)),'{delivery_id}','null') where tenant_id='tenant-north'`,
	}
	for name, sql := range changes {
		t.Run(name, func(t *testing.T) {
			ctx, admin, _, writer, _, action, keys := actionFixture(t)
			if _, err := admin.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.ApplyResolution(ctx, action.Authorization.TenantID, action, keys); !errors.Is(err, business.ErrActionConflict) {
				t.Fatalf("changed dependency accepted: %v", err)
			}
			var count int
			if err := admin.QueryRow(ctx, `select count(*) from business.ticket_resolutions`).Scan(&count); err != nil || count != 0 {
				t.Fatal("conflicting action produced an effect")
			}
		})
	}
}

func TestBusinessActionMissingDeliveryWithWrongOrderConflicts(t *testing.T) {
	ctx, admin, loaderPool, runtimePool := isolatedPools(t)
	dataset, upload := fixture()
	incomplete := dataset
	incomplete.Deliveries = dataset.Deliveries[1:]
	seedConsistencyFixture(ctx, t, business.NewStore(loaderPool), incomplete, upload)
	snapshot, _, err := business.NewStore(runtimePool).CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "missing-delivery"})
	if err != nil || snapshot.VersionVector().Delivery.Exists {
		t.Fatal("missing delivery fixture failed")
	}
	action, keys := authorizeFixture(ctx, t, admin, snapshot)
	dataset.DatasetVersion = "new-wrong-relationship"
	dataset.Deliveries[0].OrderID = "another-order"
	if err := business.NewStore(loaderPool).ImportDataset(ctx, dataset); err != nil {
		t.Fatal(err)
	}
	writer, _ := actionPools(t, admin)
	if _, err := writer.ApplyResolution(ctx, "tenant-north", action, keys); !errors.Is(err, business.ErrActionConflict) {
		t.Fatalf("new wrong relationship was treated as still missing: %v", err)
	}
}
