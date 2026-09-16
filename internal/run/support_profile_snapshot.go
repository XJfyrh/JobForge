package run

import (
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
)

// ValidateSupportSnapshot runs after one trusted capture and again before new
// admission commits. It does not claim to prove seed or executable source bytes.
func ValidateSupportSnapshot(p Profile, s SnapshotBinding) error {
	if p.Strategy != SupportFixedStrategy || !p.AuditEnabled() {
		return nil
	}
	if ValidateSupportProfile(p) != nil {
		return ErrProfileUnavailable
	}
	var ticket business.Ticket
	var vector business.VersionVector
	if len(s.Ticket)+len(s.VersionVector) > MaxCheckpointBytes ||
		jsonstrict.Decode(s.Ticket, &ticket) != nil || jsonstrict.Decode(s.VersionVector, &vector) != nil ||
		!supportDigest(s.ContentHash) || !supportDigest(s.IndexProfileHash) ||
		!supportDigest(vector.Policy.CorpusSHA256) || !supportDigest(vector.Index.ContentHash) {
		return ErrDependencyUnavailable
	}
	if _, _, err := supportSnapshot(s); err != nil || ticket.Revision < 1 || ticket.Revision > MaxSafeInteger {
		return ErrDependencyUnavailable
	}
	d, _ := DecodeSupportDefinition(p.Definition)
	r := d.Resources
	observed, _ := time.Parse(time.RFC3339, r.ObservedAt)
	if ticket.PolicyVersion != r.PolicyVersion || vector.Policy.Version != r.PolicyVersion ||
		vector.Policy.CorpusSHA256 != r.CorpusSHA256 || s.IndexProfileHash != r.IndexProfileHash || !ticket.ObservedAt.Equal(observed) {
		return ErrProfileUnavailable
	}
	for _, tenant := range r.Tenants {
		if tenant.TenantID == s.TenantID {
			if vector.Policy.Revision != tenant.PolicyRevision || s.IndexID != tenant.IndexID || vector.Index.ContentHash != tenant.IndexContentHash {
				return ErrProfileUnavailable
			}
			return nil
		}
	}
	return ErrProfileUnavailable
}
