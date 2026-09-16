package business

// VersionVector identifies every fact and missing relationship frozen by a
// snapshot. It supplements, but never replaces, snapshot ID and content hash.
type VersionVector struct {
	SchemaVersion int             `json:"schema_version"`
	Ticket        TicketVersion   `json:"ticket"`
	Order         OrderVersion    `json:"order"`
	Delivery      DeliveryVersion `json:"delivery"`
	Policy        PolicyRevision  `json:"policy"`
	Index         IndexVersion    `json:"index"`
}

// TicketVersion identifies the required, authorized ticket copy.
type TicketVersion struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// OrderVersion preserves the expected relationship ID even if no row was
// accessible. A missing row always has a null revision, never revision zero.
type OrderVersion struct {
	ID       *string `json:"id"`
	Exists   bool    `json:"exists"`
	Revision *int64  `json:"revision"`
}

// DeliveryVersion identifies the authorized delivery aggregate, including
// missing relationships and the aggregate revision covering all its events.
type DeliveryVersion struct {
	ID                *string `json:"id"`
	Exists            bool    `json:"exists"`
	AggregateRevision *int64  `json:"aggregate_revision"`
}

// PolicyRevision binds the frozen policy to its complete registered corpus.
type PolicyRevision struct {
	Version      string `json:"version"`
	Revision     int64  `json:"revision"`
	CorpusSHA256 string `json:"corpus_sha256"`
}

// IndexVersion identifies the complete published index captured by a snapshot.
type IndexVersion struct {
	ID          string `json:"id"`
	ProfileHash string `json:"profile_hash"`
	ContentHash string `json:"content_hash"`
}

// VersionVector derives metadata exclusively from the immutable snapshot copy.
// Keeping it outside Snapshot's JSON preserves the original stored body and
// content hash, including snapshots created before version vectors existed.
func (s *Snapshot) VersionVector() VersionVector {
	vector := VersionVector{
		SchemaVersion: SchemaVersion,
		Ticket:        TicketVersion{ID: s.Ticket.TicketID, Revision: s.Ticket.Revision},
		Policy:        PolicyRevision{Version: s.Policy.PolicyVersion, Revision: s.Policy.Revision, CorpusSHA256: s.Policy.CorpusSHA256},
		Index:         IndexVersion{ID: s.Index.ID, ProfileHash: s.Index.ProfileHash, ContentHash: s.Index.ContentHash},
	}
	if s.Ticket.OrderID != nil {
		id := *s.Ticket.OrderID
		vector.Order.ID = &id
		if s.Order != nil {
			revision := s.Order.Revision
			vector.Order.Exists, vector.Order.Revision = true, &revision
			if s.Order.DeliveryID != nil {
				deliveryID := *s.Order.DeliveryID
				vector.Delivery.ID = &deliveryID
				if s.Delivery != nil {
					aggregateRevision := s.Delivery.AggregateRevision
					vector.Delivery.Exists, vector.Delivery.AggregateRevision = true, &aggregateRevision
				}
			}
		}
	}
	return vector
}
