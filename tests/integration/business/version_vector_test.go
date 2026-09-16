package business_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/xjfyrh/jobforge/internal/business"
)

func TestBusinessVersionVectorMissingFactsRemainFrozen(t *testing.T) {
	for _, missing := range []string{"order", "delivery"} {
		t.Run(missing, func(t *testing.T) {
			ctx, bootstrap, loaderPool, runtimePool := isolatedPools(t)
			loader, store := business.NewStore(loaderPool), business.NewStore(runtimePool)
			dataset, upload := fixture()
			incomplete := dataset
			incomplete.DatasetVersion = "missing-fact-v1"
			incomplete.Deliveries = dataset.Deliveries[1:]
			if missing == "order" {
				incomplete.Orders = dataset.Orders[1:]
			}
			seedConsistencyFixture(ctx, t, loader, incomplete, upload)
			req := business.SnapshotRequest{SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "before-fact-appears"}
			original, reused, err := store.CreateSnapshot(ctx, "tenant-north", req)
			if err != nil || reused {
				t.Fatalf("capture missing fact: reused=%v err=%v", reused, err)
			}
			vector := original.VersionVector()
			if vector.Order.ID == nil || *vector.Order.ID != "order-1" || vector.Delivery.Exists || vector.Delivery.AggregateRevision != nil {
				t.Fatal("missing facts lost their authorized relationship or null revision")
			}
			if missing == "order" {
				if vector.Order.Exists || vector.Order.Revision != nil || vector.Delivery.ID != nil {
					t.Fatal("missing order exposed a row or invented a delivery relationship")
				}
			} else if !vector.Order.Exists || vector.Order.Revision == nil || *vector.Order.Revision != 1 ||
				vector.Delivery.ID == nil || *vector.Delivery.ID != "delivery-1" {
				t.Fatal("missing delivery lost its expected relationship")
			}
			var storedBefore []byte
			if err := bootstrap.QueryRow(ctx, `select body from business.snapshots where snapshot_id = $1`, original.ID).Scan(&storedBefore); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(storedBefore, []byte(`"version_vector"`)) {
				t.Fatal("derived vector changed the legacy stored snapshot representation")
			}
			// The helper owns this isolated database. A second import only adds
			// previously absent facts, never alters a published demonstration.
			if err := loader.ImportDataset(ctx, dataset); err != nil {
				t.Fatalf("introduce the previously missing fact: %v", err)
			}
			repeated, reused, err := store.CreateSnapshot(ctx, "tenant-north", req)
			if err != nil || !reused || repeated.ID != original.ID || repeated.ContentHash != original.ContentHash ||
				!reflect.DeepEqual(repeated.VersionVector(), vector) {
				t.Fatalf("idempotent capture changed after source insertion: reused=%v err=%v", reused, err)
			}
			stored, err := store.GetSnapshot(ctx, "tenant-north", original.ID)
			if err != nil || stored.ContentHash != original.ContentHash || !reflect.DeepEqual(stored.VersionVector(), vector) {
				t.Fatalf("old snapshot vector changed after source insertion: %v", err)
			}
			var storedAfter []byte
			if err := bootstrap.QueryRow(ctx, `select body from business.snapshots where snapshot_id = $1`, original.ID).Scan(&storedAfter); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(storedBefore, storedAfter) {
				t.Fatal("metadata derivation rewrote the old stored snapshot")
			}
			req.RequestKey = "after-fact-appears"
			fresh, reused, err := store.CreateSnapshot(ctx, "tenant-north", req)
			if err != nil || reused {
				t.Fatalf("capture newly available fact: reused=%v err=%v", reused, err)
			}
			current := fresh.VersionVector()
			if reflect.DeepEqual(current, vector) || !current.Order.Exists || current.Order.Revision == nil ||
				*current.Order.Revision != 1 || !current.Delivery.Exists || current.Delivery.AggregateRevision == nil ||
				*current.Delivery.AggregateRevision != 1 {
				t.Fatal("new snapshot did not distinguish newly available facts")
			}
		})
	}
}
