package tasks

import (
	"context"
	"strings"
	"time"
)

var orderSchema, _ = fixtures.ReadFile("fixtures/purchase-order-v1.schema.json")

// PurchaseOrder is the versioned extraction output. Required scalar values
// are validated after strict decoding; the source quote must be verbatim.
type PurchaseOrder struct {
	OrderID      string  `json:"order_id"`
	Supplier     string  `json:"supplier"`
	Quantity     int     `json:"quantity"`
	Currency     string  `json:"currency"`
	TotalAmount  float64 `json:"total_amount"`
	DeliveryDate string  `json:"delivery_date"`
	SourceQuote  string  `json:"source_quote"`
}

// Extraction preserves output, provenance, and the bounded inference count.
type Extraction struct {
	DocumentVersion string        `json:"document_version"`
	DocumentSHA256  string        `json:"document_sha256"`
	SchemaVersion   string        `json:"schema_version"`
	SchemaSHA256    string        `json:"schema_sha256"`
	Model           string        `json:"model"`
	ModelDigest     string        `json:"model_digest"`
	ModelCalls      int           `json:"model_calls"`
	SourceRef       string        `json:"source_ref"`
	Order           PurchaseOrder `json:"order"`
}

func validateOrder(raw, document string) (PurchaseOrder, error) {
	var order PurchaseOrder
	if len(raw) > maxOutputBytes || strictJSON([]byte(raw), &order) != nil {
		return order, permanent("OUTPUT_SCHEMA_INVALID")
	}
	_, dateErr := time.Parse("2006-01-02", order.DeliveryDate)
	if order.OrderID == "" || order.Supplier == "" || order.Quantity <= 0 || order.Currency != "USD" || order.TotalAmount <= 0 || dateErr != nil || len(order.SourceQuote) < 10 ||
		!strings.Contains(document, order.SourceQuote) || !strings.Contains(document, order.OrderID) || !strings.Contains(document, order.Supplier) || !strings.Contains(document, order.DeliveryDate) {
		return order, permanent("OUTPUT_SCHEMA_INVALID")
	}
	return order, nil
}

func extractOrder(ctx context.Context, model Model) (*Extraction, error) {
	document, err := fixtures.ReadFile("fixtures/purchase-order-v1.txt")
	if err != nil || len(document) > maxDocumentBytes {
		return nil, permanent("DOCUMENT_INVALID")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		correction := ""
		if attempt == 2 {
			correction = "OUTPUT_SCHEMA_INVALID"
		}
		raw, modelErr := model.Extract(ctx, string(document), correction)
		if modelErr != nil {
			return nil, modelErr
		}
		order, validationErr := validateOrder(raw, string(document))
		if validationErr == nil {
			return &Extraction{DocumentVersion: "purchase-order-v1", DocumentSHA256: digest(document), SchemaVersion: "purchase-order-v1", SchemaSHA256: digest(orderSchema), Model: ChatModel, ModelDigest: ChatDigest,
				ModelCalls: attempt, SourceRef: "fixture:purchase-order-v1", Order: order}, nil
		}
	}
	return nil, permanent("OUTPUT_SCHEMA_INVALID")
}
