package business_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/business"
)

func seedConsistencyFixture(ctx context.Context, t *testing.T, loader *business.Store, dataset business.Dataset, upload business.IndexUpload) *business.PublishedIndex {
	t.Helper()
	if err := loader.ImportDataset(ctx, dataset); err != nil {
		t.Fatalf("seed consistency fixture: %v", err)
	}
	// Orthogonal fixture vectors verify database mechanics, not model quality.
	index, reused, err := loader.PublishIndex(ctx, upload)
	if err != nil || reused {
		t.Fatalf("publish synthetic consistency index: reused=%v err=%v", reused, err)
	}
	return index
}

type snapshotOrderReadKey struct{}

// snapshotReadBarrier commits the writer after the snapshot has read its order,
// but before it can read delivery. Read committed would mix two revisions.
type snapshotReadBarrier struct {
	orderRead chan struct{}
	committed chan struct{}
}

func (b *snapshotReadBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "select body from business.orders ") {
		return context.WithValue(ctx, snapshotOrderReadKey{}, true)
	}
	return ctx
}

func (b *snapshotReadBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if marked, _ := ctx.Value(snapshotOrderReadKey{}).(bool); !marked || data.Err != nil {
		return
	}
	select {
	case b.orderRead <- struct{}{}:
	case <-ctx.Done():
		return
	}
	select {
	case <-b.committed:
	case <-ctx.Done():
	}
}

func TestBusinessSnapshotConsistentAcrossConcurrentSourceCommit(t *testing.T) {
	parentCtx, _, loaderPool, runtimePool := isolatedPools(t)
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()
	dataset, upload := fixture()
	seedConsistencyFixture(ctx, t, business.NewStore(loaderPool), dataset, upload)

	barrier := &snapshotReadBarrier{orderRead: make(chan struct{}), committed: make(chan struct{})}
	config := runtimePool.Config()
	config.ConnConfig.Tracer = barrier
	tracedPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("construct traced runtime pool: %v", err)
	}
	defer tracedPool.Close()
	store := business.NewStore(tracedPool)

	const rounds = 12
	staged := make(chan int64)
	writerDone := make(chan error, 1)
	var writer sync.WaitGroup
	writer.Go(func() {
		for revision := int64(2); revision <= rounds+1; revision++ {
			if err := advanceSourcePair(ctx, loaderPool, barrier, staged, revision); err != nil {
				writerDone <- err
				cancel()
				return
			}
		}
		writerDone <- nil
	})
	defer func() {
		cancel()
		writer.Wait()
	}()

	for round := range rounds {
		var revision int64
		select {
		case revision = <-staged:
		case <-ctx.Done():
			t.Fatalf("writer did not stage round %d: %v", round, ctx.Err())
		}
		snapshot, reused, err := store.CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{
			SchemaVersion: 1, TicketID: "ticket-1", RequestKey: fmt.Sprintf("consistent-view-%d", round),
		})
		if err != nil || reused {
			t.Fatalf("capture concurrent round %d: reused=%v err=%v", round, reused, err)
		}
		if snapshot.Order == nil || snapshot.Delivery == nil {
			t.Fatalf("round %d omitted associated source facts", round)
		}
		if snapshot.Order.Revision != revision-1 || snapshot.Delivery.AggregateRevision != revision-1 {
			t.Fatalf("mixed database view in round %d: order=%d delivery=%d want=%d", round,
				snapshot.Order.Revision, snapshot.Delivery.AggregateRevision, revision-1)
		}
		vector := snapshot.VersionVector()
		if vector.Order.Revision == nil || vector.Delivery.AggregateRevision == nil ||
			*vector.Order.Revision != revision-1 || *vector.Delivery.AggregateRevision != revision-1 {
			t.Fatalf("version vector mixed concurrent source revisions in round %d", round)
		}
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("concurrent source writer: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("writer did not finish: %v", ctx.Err())
	}
}

func advanceSourcePair(ctx context.Context, pool *pgxpool.Pool, barrier *snapshotReadBarrier, staged chan<- int64, revision int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `update business.orders set revision = $1,
 body = jsonb_set(body, '{revision}', to_jsonb($1::bigint))
 where tenant_id = 'tenant-north' and order_id = 'order-1'`, revision); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update business.deliveries set aggregate_revision = $1,
 body = jsonb_set(body, '{aggregate_revision}', to_jsonb($1::bigint))
 where tenant_id = 'tenant-north' and delivery_id = 'delivery-1'`, revision); err != nil {
		return err
	}
	select {
	case staged <- revision:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-barrier.orderRead:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	select {
	case barrier.committed <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBusinessSnapshotRejectsForeignSourceRelationships(t *testing.T) {
	ctx, _, loaderPool, runtimePool := isolatedPools(t)
	dataset, upload := fixture()
	crossOrderID := "south-only-order"
	crossDeliveryID := "south-only-delivery"
	wrongDeliveryID := "north-delivery-for-another-order"

	southOrder := dataset.Orders[1]
	southOrder.OrderID = crossOrderID
	dataset.Orders = append(dataset.Orders, southOrder)
	crossDeliveryOrder := dataset.Orders[0]
	crossDeliveryOrder.OrderID, crossDeliveryOrder.DeliveryID = "north-cross-delivery-order", &crossDeliveryID
	wrongDeliveryOrder := dataset.Orders[0]
	wrongDeliveryOrder.OrderID, wrongDeliveryOrder.DeliveryID = "north-wrong-delivery-order", &wrongDeliveryID
	dataset.Orders = append(dataset.Orders, crossDeliveryOrder, wrongDeliveryOrder)
	southDelivery := dataset.Deliveries[1]
	southDelivery.DeliveryID, southDelivery.OrderID = crossDeliveryID, crossDeliveryOrder.OrderID
	wrongDelivery := dataset.Deliveries[0]
	wrongDelivery.DeliveryID = wrongDeliveryID
	dataset.Deliveries = append(dataset.Deliveries, southDelivery, wrongDelivery)

	cases := []struct {
		name       string
		orderID    *string
		wantOrder  bool
		orderCause string
	}{
		{"cross-tenant-order", &crossOrderID, false, "record_not_found"},
		{"cross-tenant-delivery", &crossDeliveryOrder.OrderID, true, ""},
		{"wrong-order-delivery", &wrongDeliveryOrder.OrderID, true, ""},
		{"no-order-association", nil, false, "not_associated"},
	}
	for _, tc := range cases {
		ticket := dataset.Tickets[0]
		ticket.TicketID, ticket.OrderID = tc.name, tc.orderID
		dataset.Tickets = append(dataset.Tickets, ticket)
	}
	seedConsistencyFixture(ctx, t, business.NewStore(loaderPool), dataset, upload)
	store := business.NewStore(runtimePool)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, _, err := store.CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{
				SchemaVersion: 1, TicketID: tc.name, RequestKey: "relationship-" + tc.name,
			})
			if err != nil {
				t.Fatalf("capture relationship: %v", err)
			}
			if (snapshot.Order != nil) != tc.wantOrder || snapshot.Delivery != nil {
				t.Fatal("snapshot exposed an unrelated order or delivery")
			}
			vector := snapshot.VersionVector()
			if !reflect.DeepEqual(vector.Order.ID, tc.orderID) || vector.Order.Exists != tc.wantOrder ||
				vector.Delivery.Exists || vector.Delivery.AggregateRevision != nil {
				t.Fatal("version vector exposed inaccessible facts or lost the expected order ID")
			}
			if tc.wantOrder {
				if !reflect.DeepEqual(vector.Delivery.ID, snapshot.Order.DeliveryID) || vector.Order.Revision == nil {
					t.Fatal("version vector lost the authorized delivery relationship")
				}
			} else if vector.Order.Revision != nil || vector.Delivery.ID != nil {
				t.Fatal("missing order must have a null revision and no known delivery ID")
			}
			order, err := store.GetOrder(ctx, "tenant-north", snapshot.ID)
			if err != nil || order.Missing == tc.wantOrder || order.MissingReason != tc.orderCause {
				t.Fatalf("order missing semantics: evidence=%+v err=%v", order, err)
			}
			if tc.wantOrder && (order.Order == nil || order.Order.TenantID != "tenant-north" || order.Order.OrderID != *tc.orderID) {
				t.Fatal("order evidence did not retain the authorized relationship")
			}
			delivery, err := store.GetDelivery(ctx, "tenant-north", snapshot.ID)
			if err != nil || !delivery.Missing || delivery.Delivery != nil {
				t.Fatalf("unrelated delivery evidence: evidence=%+v err=%v", delivery, err)
			}
			if tc.wantOrder && delivery.MissingReason != "record_not_found" {
				t.Fatal("unavailable associated delivery was reported as unassociated")
			}
			if order.EvidenceRef != "business-evidence:"+snapshot.ID+":order" ||
				delivery.EvidenceRef != "business-evidence:"+snapshot.ID+":delivery" {
				t.Fatal("missing evidence escaped the snapshot binding")
			}
		})
	}
}

func TestBusinessPolicyUpgradePreservesExistingSnapshotIndex(t *testing.T) {
	ctx, _, loaderPool, runtimePool := isolatedPools(t)
	loader, store := business.NewStore(loaderPool), business.NewStore(runtimePool)
	dataset, upload := fixture()
	policyV2 := dataset.Policies[0]
	policyV2.PolicyVersion, policyV2.CorpusSHA256 = "policy-v2", strings.Repeat("b", 64)
	dataset.Policies = append(dataset.Policies, policyV2)
	oldIndex := seedConsistencyFixture(ctx, t, loader, dataset, upload)
	oldRequest := business.SnapshotRequest{SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "before-policy-upgrade"}
	oldSnapshot, _, err := store.CreateSnapshot(ctx, "tenant-north", oldRequest)
	if err != nil {
		t.Fatalf("capture original policy: %v", err)
	}

	upload.Profile.PolicyVersion, upload.Profile.CorpusSHA256 = policyV2.PolicyVersion, policyV2.CorpusSHA256
	upload.Chunks = append([]business.IndexChunk(nil), upload.Chunks...)
	for i := range upload.Chunks {
		upload.Chunks[i].Text += " revised policy"
		if i > 0 {
			upload.Chunks[i].ChunkID = "v2-" + upload.Chunks[i].ChunkID
		}
	}
	// Keep synthetic vectors fixed so only index/version authorization changes.
	newIndex, reused, err := loader.PublishIndex(ctx, upload)
	if err != nil || reused || newIndex.ID == oldIndex.ID {
		t.Fatalf("publish upgraded synthetic index: reused=%v err=%v", reused, err)
	}
	ticket := dataset.Tickets[0]
	ticket.Revision, ticket.PolicyVersion = 2, policyV2.PolicyVersion
	body, err := json.Marshal(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaderPool.Exec(ctx, `update business.tickets set revision = $1, body = $2
 where tenant_id = $3 and ticket_id = $4`, ticket.Revision, body, ticket.TenantID, ticket.TicketID); err != nil {
		t.Fatalf("upgrade ticket policy binding: %v", err)
	}
	newSnapshot, _, err := store.CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{
		SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "after-policy-upgrade",
	})
	if err != nil || newSnapshot.Ticket.Revision != 2 || newSnapshot.Policy.PolicyVersion != policyV2.PolicyVersion || newSnapshot.Index.ID != newIndex.ID {
		t.Fatalf("capture upgraded policy: %v", err)
	}
	repeated, reused, err := store.CreateSnapshot(ctx, "tenant-north", oldRequest)
	if err != nil || !reused || repeated.ID != oldSnapshot.ID || repeated.ContentHash != oldSnapshot.ContentHash || repeated.Index.ID != oldIndex.ID {
		t.Fatalf("old request changed after policy upgrade: reused=%v err=%v", reused, err)
	}
	if !reflect.DeepEqual(repeated.VersionVector(), oldSnapshot.VersionVector()) ||
		newSnapshot.VersionVector().Policy == oldSnapshot.VersionVector().Policy ||
		newSnapshot.VersionVector().Index == oldSnapshot.VersionVector().Index {
		t.Fatal("version vector did not preserve the frozen policy and index identities")
	}
	search := business.SearchRequest{EmbeddingModel: business.EmbeddingModel, EmbeddingDigest: business.EmbeddingDigest, QueryVector: upload.Chunks[0].Embedding}
	for _, tc := range []struct {
		name, id, indexID, version, text, foreignChunk string
	}{
		{"old", oldSnapshot.ID, oldIndex.ID, "policy-v1", "chunk-a", "v2-chunk-b"},
		{"new", newSnapshot.ID, newIndex.ID, "policy-v2", "chunk-a revised policy", "chunk-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := store.GetSnapshot(ctx, "tenant-north", tc.id)
			if err != nil || snapshot.Index.ID != tc.indexID || snapshot.Policy.PolicyVersion != tc.version {
				t.Fatalf("persisted policy binding: %v", err)
			}
			hit, err := store.GetPolicy(ctx, "tenant-north", tc.id, "chunk-a")
			if err != nil || hit.Text != tc.text || hit.PolicyVersion != tc.version || hit.EvidenceRef != "business-policy:"+tc.indexID+":chunk-a" {
				t.Fatalf("versioned policy evidence: hit=%+v err=%v", hit, err)
			}
			if _, err := store.GetPolicy(ctx, "tenant-north", tc.id, tc.foreignChunk); !errors.Is(err, business.ErrNotFound) {
				t.Fatalf("chunk from another index resolved: %v", err)
			}
			hits, err := store.SearchPolicies(ctx, "tenant-north", tc.id, search)
			if err != nil || len(hits) != 3 || hits[0].Text != tc.text {
				t.Fatalf("search bound policy version: %v", err)
			}
			for _, hit := range hits {
				if hit.IndexID != tc.indexID || hit.PolicyVersion != tc.version || hit.EvidenceRef != "business-policy:"+tc.indexID+":"+hit.ChunkID {
					t.Fatal("search crossed the frozen policy index")
				}
			}
		})
	}
}

func TestBusinessDeliveryEventsRequireAggregateRevision(t *testing.T) {
	ctx, _, loaderPool, runtimePool := isolatedPools(t)
	dataset, upload := fixture()
	seedConsistencyFixture(ctx, t, business.NewStore(loaderPool), dataset, upload)
	delivery := dataset.Deliveries[0]
	delivery.Events = append(delivery.Events, business.DeliveryEvent{
		EventID: "event-2", OccurredAt: dataset.Tickets[0].ObservedAt,
		Status: "delivered", Note: "Synthetic second event requiring a new aggregate revision.",
	})
	body, err := json.Marshal(delivery)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loaderPool.Exec(ctx, `update business.deliveries set body = $1
 where tenant_id = $2 and delivery_id = $3`, body, delivery.TenantID, delivery.DeliveryID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("event update without revision was not rejected by the database guard: %v", err)
	}
	store := business.NewStore(runtimePool)
	unchanged, _, err := store.CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{
		SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "after-rejected-event",
	})
	if err != nil || unchanged.Delivery == nil || unchanged.Delivery.AggregateRevision != 1 || len(unchanged.Delivery.Events) != 1 {
		t.Fatalf("rejected event changed stored aggregate: %v", err)
	}
	delivery.AggregateRevision = 2
	body, err = json.Marshal(delivery)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaderPool.Exec(ctx, `update business.deliveries set aggregate_revision = $1, body = $2
 where tenant_id = $3 and delivery_id = $4`, delivery.AggregateRevision, body, delivery.TenantID, delivery.DeliveryID); err != nil {
		t.Fatalf("versioned delivery event rejected: %v", err)
	}
	updated, _, err := store.CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{
		SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "after-versioned-event",
	})
	if err != nil || updated.Delivery == nil || updated.Delivery.AggregateRevision != 2 || len(updated.Delivery.Events) != 2 {
		t.Fatalf("new snapshot omitted the new aggregate revision: %v", err)
	}
	oldEvidence, err := store.GetDelivery(ctx, "tenant-north", unchanged.ID)
	if err != nil || oldEvidence.Delivery == nil || oldEvidence.Delivery.AggregateRevision != 1 || len(oldEvidence.Delivery.Events) != 1 {
		t.Fatalf("event addition changed old snapshot evidence: %v", err)
	}
}
