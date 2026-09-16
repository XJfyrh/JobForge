package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func validUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func (s *Store) readOnly(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return dbError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return dbError(err)
	}
	return dbError(tx.Commit(ctx))
}

func fillBudget(ctx context.Context, tx pgx.Tx, r *agentrun.Run) error {
	accounts, err := loadAccounts(ctx, tx, r.TenantID, r.BusinessRequestID, false)
	if err != nil {
		return err
	}
	r.Budget.Family, r.Budget.Tenant, r.Budget.Batch = accounts[0].Account, accounts[1].Account, accounts[2].Account
	u := &r.Budget.RunUsage
	err = tx.QueryRow(ctx, `select
		count(*) filter (where kind='chat'),
		count(*) filter (where kind='query_embedding'),
		count(*) filter (where kind='profile_metadata_http'),
		count(*) filter (where kind='business_tool_http'),
		count(*), count(*) filter (where step_kind='protocol_correction'),
		coalesce(sum(case when status='known' then known_tokens else reserved_tokens end),0)::bigint,
		coalesce(sum(case when status='known' then known_cost_microyuan else reserved_cost_microyuan end),0)::bigint
		from physical_calls where tenant_id=$1 and run_id=$2`, r.TenantID, r.ID).
		Scan(&u.Chat, &u.QueryEmbedding, &u.ProfileMetadataHTTP, &u.BusinessToolHTTP,
			&u.PhysicalHTTP, &u.ProtocolCorrections, &u.Tokens, &u.CostMicroyuan)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, "select count(*) from tool_invocations where tenant_id=$1 and run_id=$2", r.TenantID, r.ID).Scan(&u.LogicalTools)
}

func getView(ctx context.Context, tx pgx.Tx, tenant, id string) (agentrun.Run, error) {
	r, _, err := readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2", tenant, id))
	if err != nil {
		return r, err
	}
	err = fillBudget(ctx, tx, &r)
	return r, err
}

// Get returns a consistent Run/budget view under one repeatable-read snapshot.
func (s *Store) Get(ctx context.Context, tenant, id string) (agentrun.Run, error) {
	var r agentrun.Run
	if !validUUID(id) {
		return r, agentrun.ErrInvalidArgument
	}
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = getView(ctx, tx, tenant, id)
		return err
	})
	return r, err
}

type pageCursor struct {
	Version   int            `json:"version"`
	TenantID  string         `json:"tenant_id"`
	State     agentrun.State `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
	ID        string         `json:"run_id"`
}

func pageLimit(limit int) (int, error) {
	if limit == 0 {
		return 20, nil
	}
	if limit < 1 || limit > 100 {
		return 0, agentrun.ErrInvalidArgument
	}
	return limit, nil
}

// List uses a tenant/filter-bound keyset. A cursor is never an authorization.
func (s *Store) List(ctx context.Context, tenant string, filter agentrun.ListFilter) (agentrun.Page, error) {
	page := agentrun.Page{Items: []agentrun.Run{}}
	limit, err := pageLimit(filter.Limit)
	if err != nil || (filter.State != "" && !filter.State.Valid()) {
		return page, agentrun.ErrInvalidArgument
	}
	var cursor pageCursor
	if filter.Cursor != "" {
		if len(filter.Cursor) > 2048 {
			return page, agentrun.ErrInvalidArgument
		}
		decoded, err := base64.RawURLEncoding.DecodeString(filter.Cursor)
		if err != nil || jsonstrict.Decode(decoded, &cursor) != nil || cursor.Version != 1 ||
			cursor.TenantID != tenant || cursor.State != filter.State || !validUUID(cursor.ID) || cursor.CreatedAt.IsZero() {
			return page, agentrun.ErrInvalidArgument
		}
	}
	err = s.readOnly(ctx, func(tx pgx.Tx) error {
		query := "select " + runColumns + " from runs where tenant_id=$1"
		args := []any{tenant}
		if filter.State != "" {
			query += " and state=$2"
			args = append(args, filter.State)
		}
		if filter.Cursor != "" {
			position := len(args) + 1
			query += " and (created_at,run_id)<(" + placeholder(position) + "," + placeholder(position+1) + ")"
			args = append(args, cursor.CreatedAt, cursor.ID)
		}
		query += " order by created_at desc,run_id desc limit " + placeholder(len(args)+1)
		args = append(args, limit+1)
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			r, _, err := readRun(rows)
			if err != nil {
				rows.Close()
				return err
			}
			page.Items = append(page.Items, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			last := page.Items[len(page.Items)-1]
			encoded, err := json.Marshal(pageCursor{Version: 1, TenantID: tenant, State: filter.State, CreatedAt: last.CreatedAt, ID: last.ID})
			if err != nil {
				return err
			}
			next := base64.RawURLEncoding.EncodeToString(encoded)
			page.NextCursor = &next
		}
		for i := range page.Items {
			if err := fillBudget(ctx, tx, &page.Items[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return page, err
}

// Steps checks the parent tenant before exposing any protected checkpoint data.
func (s *Store) Steps(ctx context.Context, tenant, id string, after int64, limit int) (agentrun.StepPage, error) {
	page := agentrun.StepPage{Items: []agentrun.Step{}}
	limit, err := pageLimit(limit)
	if err != nil || !validUUID(id) || after < 0 || after > agentrun.MaxSafeInteger {
		return page, agentrun.ErrInvalidArgument
	}
	err = s.readOnly(ctx, func(tx pgx.Tx) error {
		if _, _, err := readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2", tenant, id)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select step_id,sequence,kind,input_hash,profile_hash,snapshot_hash,
			commit_hash,output_ref,output,cursor_version,created_at from run_steps
			where tenant_id=$1 and run_id=$2 and sequence>$3 order by sequence limit $4`, tenant, id, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var step agentrun.Step
			if err := rows.Scan(&step.ID, &step.Sequence, &step.Kind, &step.InputHash, &step.ProfileHash, &step.SnapshotHash,
				&step.CommitHash, &step.OutputRef, &step.Output, &step.CursorVersion, &step.CreatedAt); err != nil {
				return err
			}
			page.Items = append(page.Items, step)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			next := page.Items[len(page.Items)-1].Sequence
			page.NextAfter = &next
		}
		return nil
	})
	return page, err
}

// Events lists only fixed metadata, never model or document content.
func (s *Store) Events(ctx context.Context, tenant, id string, after int64, limit int) (agentrun.EventPage, error) {
	page := agentrun.EventPage{Items: []agentrun.Event{}}
	limit, err := pageLimit(limit)
	if err != nil || !validUUID(id) || after < 0 || after > agentrun.MaxSafeInteger {
		return page, agentrun.ErrInvalidArgument
	}
	err = s.readOnly(ctx, func(tx pgx.Tx) error {
		if _, _, err := readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2", tenant, id)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `select sequence,event_type,state,attempt_no,cursor_version,created_at
			from run_events where tenant_id=$1 and run_id=$2 and sequence>$3 order by sequence limit $4`, tenant, id, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var event agentrun.Event
			if err := rows.Scan(&event.Sequence, &event.Type, &event.State, &event.AttemptNo, &event.CursorVersion, &event.CreatedAt); err != nil {
				return err
			}
			page.Items = append(page.Items, event)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			next := page.Items[len(page.Items)-1].Sequence
			page.NextAfter = &next
		}
		return nil
	})
	return page, err
}

// Result returns an explicit absence before a result is accepted. A retained
// proposal remains distinguishable from a successful business write.
func (s *Store) Result(ctx context.Context, tenant, id string) (agentrun.Result, error) {
	var result agentrun.Result
	if !validUUID(id) {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.pool.QueryRow(ctx, "select result_kind,result_ref from runs where tenant_id=$1 and run_id=$2", tenant, id).Scan(&result.Kind, &result.Ref)
	result.Available = err == nil && result.Ref != nil
	return result, dbError(err)
}
