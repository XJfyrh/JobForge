package observability

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Fixed labels contain no Run, tenant, actor, operation or trace identifiers.
var (
	RunStepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "jobforge_run_step_duration_seconds", Help: "Registered step including process cleanup and commit.", Buckets: []float64{.01, .05, .1, .5, 1, 5, 10, 30, 60, 180}}, []string{"kind", "result"})
	RunRPCDuration  = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "jobforge_run_control_rpc_duration_seconds", Help: "One physical control RPC; checkpoint commit includes its ACK.", Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 2}}, []string{"method", "result"})
	RunHTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "jobforge_run_http_requests_total", Help: "Authenticated public requests by registered route and stable outcome."}, []string{"route", "result"})
)

type runMetric struct {
	desc   *prometheus.Desc
	labels int
}

// RunCollector exports a read-only, bounded PG snapshot. Failure emits no
// invented zeros; scrape_success distinguishes missing state from empty state.
type RunCollector struct {
	pool    *pgxpool.Pool
	metrics map[string]runMetric
	success *prometheus.Desc
}

// NewRunCollector constructs the finite v3 snapshot collector.
func NewRunCollector(pool *pgxpool.Pool) *RunCollector {
	c := &RunCollector{pool: pool, metrics: make(map[string]runMetric), success: prometheus.NewDesc("jobforge_run_snapshot_success", "One means all read-only PG snapshot queries succeeded.", nil, nil)}
	for name, item := range map[string]struct {
		help   string
		labels []string
	}{
		"current":               {"Current persisted Run count.", []string{"state"}},
		"oldest_wait_seconds":   {"Age of oldest pending Run from its last transition.", []string{"state"}},
		"workers_live":          {"Distinct Workers with an unexpired registered session.", nil},
		"recoveries":            {"Persisted recovery_count summed over retained Runs.", nil},
		"budget_cost_microyuan": {"Retained account exposure; scopes overlap and must not be added together.", []string{"scope", "kind"}},
		"budget_frozen":         {"Frozen retained accounts.", []string{"scope"}},
		"budget_max_ratio":      {"Maximum individual account known plus held exposure divided by limit.", []string{"scope"}},
		"calls":                 {"Persisted physical calls; unknown remains a full hold.", []string{"kind", "status"}},
		"action_calls":          {"Persisted independent action physical calls.", []string{"kind", "status"}},
	} {
		c.metrics[name] = runMetric{prometheus.NewDesc("jobforge_run_"+name, item.help, item.labels, nil), len(item.labels)}
	}
	return c
}

// Describe declares all descriptors independently of database availability.
func (c *RunCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.success
	for _, item := range c.metrics {
		ch <- item.desc
	}
}

// Collect emits only complete snapshots, with an explicit availability metric.
func (c *RunCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.pool.Query(ctx, runSnapshotSQL)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.success, prometheus.GaugeValue, 0)
		return
	}
	defer rows.Close()
	var metrics []prometheus.Metric
	for rows.Next() {
		var name, first, second string
		var value float64
		if err = rows.Scan(&name, &first, &second, &value); err != nil {
			break
		}
		item, exists := c.metrics[name]
		if !exists {
			err = errors.New("unknown metric")
			break
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(item.desc, prometheus.GaugeValue, value, []string{first, second}[:item.labels]...))
	}
	if err != nil || rows.Err() != nil {
		ch <- prometheus.MustNewConstMetric(c.success, prometheus.GaugeValue, 0)
		return
	}
	for _, metric := range metrics {
		ch <- metric
	}
	ch <- prometheus.MustNewConstMetric(c.success, prometheus.GaugeValue, 1)
}

// RunMetricsHandler keeps v3 metrics separate from historical Job metrics.
func RunMetricsHandler(pool *pgxpool.Pool) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(RunStepDuration, RunRPCDuration, RunHTTPRequests)
	if pool != nil {
		registry.MustRegister(NewRunCollector(pool))
	}
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{MaxRequestsInFlight: 1, Timeout: 5 * time.Second})
}

// ServeRunMetrics exposes only /metrics on a dedicated private listener. Docker
// may bind its private network explicitly; host publication stays loopback.
func ServeRunMetrics(ctx context.Context, address string, pool *pgxpool.Pool) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", RunMetricsHandler(pool))
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 6 * time.Second, MaxHeaderBytes: 4096}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-stopped:
		}
	}()
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.New("run metrics listener unavailable")
}

const runSnapshotSQL = `
select 'current',s.state,'',count(r.run_id)::float8 from
 (values ('ready'),('running'),('stopping'),('retry_wait'),('awaiting_approval'),('succeeded'),('failed'),('cancelled')) s(state)
 left join runs r on r.state=s.state group by s.state
union all select 'oldest_wait_seconds',s.state,'',coalesce(extract(epoch from clock_timestamp()-min(r.updated_at)),0)::float8 from
 (values ('ready'),('retry_wait'),('awaiting_approval')) s(state)
 left join runs r on r.state=s.state group by s.state
union all select 'workers_live','','',count(distinct worker_id)::float8 from worker_sessions where expires_at>clock_timestamp()
union all select 'recoveries','','',coalesce(sum(recovery_count),0)::float8 from runs
union all select 'budget_cost_microyuan',scope,'known',sum(known_cost_microyuan)::float8 from budget_accounts group by scope
union all select 'budget_cost_microyuan',scope,'held',sum(held_cost_microyuan)::float8 from budget_accounts group by scope
union all select 'budget_cost_microyuan',scope,'limit',sum(limit_cost_microyuan)::float8 from budget_accounts group by scope
union all select 'budget_frozen',scope,'',count(*) filter(where frozen)::float8 from budget_accounts group by scope
union all select 'budget_max_ratio',scope,'',max(case when limit_cost_microyuan=0 then case when used_cost_microyuan=0 then 0 else 2 end
 else used_cost_microyuan::float8/limit_cost_microyuan end) from budget_accounts group by scope
union all select 'calls',kind,status,count(*)::float8 from physical_calls group by kind,status
union all select 'action_calls',kind,status,count(*)::float8 from action_calls group by kind,status`
