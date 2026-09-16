package business

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestVersionVectorPreservesMissingRelationshipIdentity(t *testing.T) {
	orderID, deliveryID := "expected-order", "expected-delivery"
	for _, tc := range []struct {
		name         string
		orderID      *string
		order        *Order
		delivery     *Delivery
		wantOrder    string
		wantDelivery string
	}{
		{"no-order-association", nil, nil, nil,
			`{"id":null,"exists":false,"revision":null}`,
			`{"id":null,"exists":false,"aggregate_revision":null}`},
		{"inaccessible-order", &orderID, nil, nil,
			`{"id":"expected-order","exists":false,"revision":null}`,
			`{"id":null,"exists":false,"aggregate_revision":null}`},
		{"no-delivery-association", &orderID, &Order{OrderID: orderID, Revision: 2}, nil,
			`{"id":"expected-order","exists":true,"revision":2}`,
			`{"id":null,"exists":false,"aggregate_revision":null}`},
		{"inaccessible-delivery", &orderID, &Order{OrderID: orderID, Revision: 2, DeliveryID: &deliveryID}, nil,
			`{"id":"expected-order","exists":true,"revision":2}`,
			`{"id":"expected-delivery","exists":false,"aggregate_revision":null}`},
		{"complete-facts", &orderID, &Order{OrderID: orderID, Revision: 2, DeliveryID: &deliveryID},
			&Delivery{DeliveryID: deliveryID, OrderID: orderID, AggregateRevision: 3},
			`{"id":"expected-order","exists":true,"revision":2}`,
			`{"id":"expected-delivery","exists":true,"aggregate_revision":3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &Snapshot{
				SchemaVersion: 1, ContentHash: "original-content-identity",
				Ticket: Ticket{TicketID: "ticket", Revision: 4, OrderID: tc.orderID},
				Order:  tc.order, Delivery: tc.delivery,
				Policy: PolicyVersion{PolicyVersion: "policy-v1", Revision: 5, CorpusSHA256: "corpus-hash"},
				Index:  PublishedIndex{ID: "index", ProfileHash: "profile-hash", ContentHash: "index-content-hash"},
			}
			before, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			vector := snapshot.VersionVector()
			order, err := json.Marshal(vector.Order)
			if err != nil || string(order) != tc.wantOrder {
				t.Fatalf("order identity = %s, want %s: %v", order, tc.wantOrder, err)
			}
			delivery, err := json.Marshal(vector.Delivery)
			if err != nil || string(delivery) != tc.wantDelivery {
				t.Fatalf("delivery identity = %s, want %s: %v", delivery, tc.wantDelivery, err)
			}
			if vector.SchemaVersion != 1 || vector.Ticket != (TicketVersion{ID: "ticket", Revision: 4}) ||
				vector.Policy != (PolicyRevision{Version: "policy-v1", Revision: 5, CorpusSHA256: "corpus-hash"}) ||
				vector.Index != (IndexVersion{ID: "index", ProfileHash: "profile-hash", ContentHash: "index-content-hash"}) {
				t.Fatalf("incomplete version vector: %+v", vector)
			}
			metadata, err := json.Marshal(snapshotMetadata(snapshot))
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				ContentHash   string        `json:"content_hash"`
				VersionVector VersionVector `json:"version_vector"`
			}
			if err := json.Unmarshal(metadata, &response); err != nil ||
				!reflect.DeepEqual(response.VersionVector, vector) || response.ContentHash != snapshot.ContentHash {
				t.Fatalf("metadata lost snapshot identity or vector: %v", err)
			}
			// Returned nullable values must not let a metadata consumer mutate the
			// source snapshot or change its legacy serialization/hash inputs.
			if vector.Order.ID != nil {
				*vector.Order.ID = "changed"
			}
			if vector.Order.Revision != nil {
				*vector.Order.Revision++
			}
			if vector.Delivery.ID != nil {
				*vector.Delivery.ID = "changed"
			}
			if vector.Delivery.AggregateRevision != nil {
				*vector.Delivery.AggregateRevision++
			}
			after, err := json.Marshal(snapshot)
			if err != nil || !bytes.Equal(before, after) || bytes.Contains(after, []byte(`"version_vector"`)) {
				t.Fatal("derived metadata changed the original snapshot serialization")
			}
		})
	}
}
