package business

import (
	"context"
	"fmt"
	"strings"
)

// PolicyBundleMaxBytes bounds the complete source-bearing policy response.
const PolicyBundleMaxBytes = 32 * 1024

// PolicyBundleAliases returns the fixed policy contract, not a model selection.
func PolicyBundleAliases() []string {
	aliases := make([]string, 0, 20)
	for policy := 1; policy <= 10; policy++ {
		for paragraph := 1; paragraph <= 2; paragraph++ {
			aliases = append(aliases, fmt.Sprintf("P%02d.%d", policy, paragraph))
		}
	}
	return aliases
}

// ReadPolicyBundle reads every required paragraph from the authorized frozen index.
func (s *Store) ReadPolicyBundle(ctx context.Context, tenant, id string) ([]PolicyHit, error) {
	snapshot, err := s.GetSnapshot(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	aliases := PolicyBundleAliases()
	rows, err := s.pool.Query(ctx, `select c.chunk_id, c.source, c.body
 from business.policy_chunks c join business.policy_indexes i
 on i.tenant_id = c.tenant_id and i.index_id = c.index_id
 where c.tenant_id = $1 and c.index_id = $2 and i.published_at is not null
 order by c.chunk_id limit 21`, tenant, snapshot.Index.ID)
	if err != nil {
		return nil, publicDBError(err)
	}
	defer rows.Close()
	hits := make([]PolicyHit, 0, len(aliases))
	for rows.Next() {
		hit := policyHit(snapshot, "")
		if err := rows.Scan(&hit.ChunkID, &hit.Source, &hit.Text); err != nil {
			return nil, ErrInternal
		}
		if len(hits) >= len(aliases) || hit.ChunkID != aliases[len(hits)] ||
			!validText(hit.Text, 768) || strings.TrimSpace(hit.Text) == "" || hit.Source == "" {
			return nil, ErrProfileUnavailable
		}
		hit.EvidenceRef = fmt.Sprintf("business-policy:%s:%s", hit.IndexID, hit.ChunkID)
		hits = append(hits, hit)
	}
	if rows.Err() != nil {
		return nil, ErrDependencyUnavailable
	}
	if len(hits) != len(aliases) {
		return nil, ErrProfileUnavailable
	}
	return hits, nil
}
