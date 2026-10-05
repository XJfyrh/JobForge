package business

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

func TestActionSharedContractVectors(t *testing.T) {
	raw, err := os.ReadFile("../../api/business-action/v1/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Action            json.RawMessage `json:"action"`
		PublicKey         string          `json:"public_key"`
		PublicKeySHA256   string          `json:"public_key_sha256"`
		AuthorizationHash string          `json:"authorization_hash"`
		ParametersHash    string          `json:"parameters_hash"`
		Receipt           ActionReceipt   `json:"receipt"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid shared fixture")
	}
	action, err := DecodeSignedAction(fixture.Action)
	if err != nil {
		t.Fatal(err)
	}
	public, err := base64.RawURLEncoding.DecodeString(fixture.PublicKey)
	if err != nil || action.Verify(ed25519.PublicKey(public)) != nil || PublicKeyHash(public) != fixture.PublicKeySHA256 {
		t.Fatal("shared Ed25519 identity")
	}
	ah, _ := action.Authorization.Hash()
	ph, _ := action.Parameters.Hash()
	if ah != fixture.AuthorizationHash || ph != fixture.ParametersHash || fixture.Receipt.Validate(action) != nil {
		t.Fatal("shared hash/receipt")
	}
	for _, change := range []string{"no_action", "empty_action", "ticket_zero", "ticket_overflow", "policy_zero", "order_zero", "delivery_zero"} {
		t.Run(change, func(t *testing.T) {
			var a SignedAction
			_ = json.Unmarshal(fixture.Action, &a)
			switch change {
			case "no_action":
				a.Parameters.Decision = "no_action"
			case "empty_action":
				a.Parameters.Action = ""
			case "ticket_zero":
				a.Authorization.VersionVector.Ticket.Revision = 0
			case "ticket_overflow":
				a.Authorization.VersionVector.Ticket.Revision = actionSafeInteger
			case "policy_zero":
				a.Authorization.VersionVector.Policy.Revision = 0
			case "order_zero":
				id, rev := "order-1", int64(0)
				a.Authorization.VersionVector.Order = OrderVersion{ID: &id, Exists: true, Revision: &rev}
			case "delivery_zero":
				id, order, rev, good := "delivery-1", "order-1", int64(0), int64(1)
				a.Authorization.VersionVector.Order = OrderVersion{ID: &order, Exists: true, Revision: &good}
				a.Authorization.VersionVector.Delivery = DeliveryVersion{ID: &id, Exists: true, AggregateRevision: &rev}
			}
			b, _ := json.Marshal(a)
			if _, err := DecodeSignedAction(b); err == nil {
				t.Fatal("invalid shared action accepted")
			}
		})
	}
}
