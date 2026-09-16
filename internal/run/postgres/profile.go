package postgres

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// EnsureProfiles binds each deployment profile ID to its first immutable
// definition. Availability is deployment state, excluded from that definition.
func (s *Store) EnsureProfiles(ctx context.Context) error {
	for _, p := range s.profiles {
		if err := agentrun.ValidateSupportProfile(p); err != nil {
			return err
		}
		definition, err := json.Marshal(p)
		if err != nil {
			return agentrun.ErrInvalidArgument
		}
		if err := s.transact(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `insert into agent_profiles(profile_id,profile_hash,definition)
				values($1,$2,$3) on conflict (profile_id) do nothing`, p.ID, p.Hash, definition); err != nil {
				return err
			}
			var matches bool
			if err := tx.QueryRow(ctx, `select profile_hash=$2 and definition=$3::jsonb
				from agent_profiles where profile_id=$1`, p.ID, p.Hash, definition).Scan(&matches); err != nil {
				return err
			}
			if !matches {
				return agentrun.ErrConflict
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// Profile returns only currently executable, registered capabilities. Existing
// accepted operation lookup and late settlement do not depend on this method.
func (s *Store) Profile(_ context.Context, id string) (agentrun.Profile, error) {
	p, ok := s.profiles[id]
	if !ok || !p.Executable {
		return agentrun.Profile{}, agentrun.ErrProfileUnavailable
	}
	if err := agentrun.ValidateSupportProfile(p); err != nil {
		return agentrun.Profile{}, err
	}
	p.Definition = slices.Clone(p.Definition)
	return p, nil
}
