package ctl

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/jackc/pgx/v5/pgxpool"
)

// QuotaDriftRow is one tenant whose derived inflight counter disagrees with
// the jobs aggregation (PRD v0.3 FR-724, ADR-0007 §7).
type QuotaDriftRow struct {
	TenantID string `json:"tenant_id"`
	Counter  int64  `json:"counter"`
	Actual   int64  `json:"actual"`
}

// QuotaReconcileResult summarizes a quota reconcile run.
type QuotaReconcileResult struct {
	Drift    []QuotaDriftRow `json:"drift"`
	Repaired int             `json:"repaired"` // counter rows changed; 0 unless --repair
}

// ctlQuotaDrift mirrors the scheduler store's drift comparison: the derived
// counters are checked against the jobs aggregation (the source of truth).
const ctlQuotaDrift = `
with actual as (
    select tenant_id, count(*) as n
    from jobs
    where state in ('running', 'cancelling')
    group by tenant_id
)
select coalesce(c.tenant_id, a.tenant_id) as tenant_id,
       coalesce(c.inflight, 0) as counter,
       coalesce(a.n, 0) as actual
from tenant_quota_counters c
full outer join actual a on a.tenant_id = c.tenant_id
where coalesce(c.inflight, 0) <> coalesce(a.n, 0)
order by tenant_id
`

// ctlQuotaRepair mirrors the scheduler store's repair: counters are
// overwritten from the jobs aggregation; tenants without inflight jobs are
// reset to zero. Returns the number of counter rows changed.
const ctlQuotaRepair = `
with actual as (
    select tenant_id, count(*) as n
    from jobs
    where state in ('running', 'cancelling')
    group by tenant_id
),
upserted as (
    insert into tenant_quota_counters (tenant_id, inflight, updated_at)
    select tenant_id, n, now() from actual
    on conflict (tenant_id) do update
    set inflight = excluded.inflight,
        updated_at = now()
    where tenant_quota_counters.inflight <> excluded.inflight
    returning tenant_id
),
zeroed as (
    update tenant_quota_counters c
    set inflight = 0,
        updated_at = now()
    where c.inflight > 0
      and not exists (select 1 from actual a where a.tenant_id = c.tenant_id)
    returning c.tenant_id
)
select (select count(*) from upserted) + (select count(*) from zeroed)
`

// ReconcileQuotaCounters connects to PostgreSQL using the given DSN, compares
// tenant_quota_counters against the jobs aggregation and (with repair=true)
// overwrites divergent counters from the aggregation. The connection is
// short-lived and closed before returning. Like the other database-backed ctl
// commands this requires database credentials, not the HTTP API.
func ReconcileQuotaCounters(ctx context.Context, dsn string, repair bool) (*QuotaReconcileResult, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	res := &QuotaReconcileResult{Drift: []QuotaDriftRow{}}

	rows, err := pool.Query(ctx, ctlQuotaDrift)
	if err != nil {
		return nil, fmt.Errorf("query quota drift: %w", err)
	}
	for rows.Next() {
		var r QuotaDriftRow
		if err := rows.Scan(&r.TenantID, &r.Counter, &r.Actual); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan drift row: %w", err)
		}
		res.Drift = append(res.Drift, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("drift rows: %w", err)
	}

	if repair && len(res.Drift) > 0 {
		if err := pool.QueryRow(ctx, ctlQuotaRepair).Scan(&res.Repaired); err != nil {
			return nil, fmt.Errorf("repair quota counters: %w", err)
		}
	}
	return res, nil
}

// RenderQuotaReconcile renders the reconcile result: one row per divergent
// tenant (or an explicit "no drift" line) plus the repair count when --repair
// was used.
func RenderQuotaReconcile(w io.Writer, format string, res *QuotaReconcileResult) error {
	if format == OutputJSON {
		return writeIndentedJSON(w, res)
	}

	lw := &lineWriter{}
	if len(res.Drift) == 0 {
		lw.printf(w, "no quota counter drift detected\n")
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		lw.printf(tw, "TENANT\tCOUNTER\tACTUAL\tDRIFT\n")
		for _, r := range res.Drift {
			lw.printf(tw, "%s\t%d\t%d\t%+d\n", r.TenantID, r.Counter, r.Actual, r.Counter-r.Actual)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("flush table: %w", err)
		}
	}
	if res.Repaired > 0 {
		lw.printf(w, "\nrepaired %d counter row(s) from jobs aggregation\n", res.Repaired)
	}
	return lw.err
}
