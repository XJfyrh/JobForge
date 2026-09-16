package integration

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// TestRunHotPathPlansAndBaseline reports the new Run baseline, not a latency
// gate or an improvement over the independent, failed legacy W4 benchmark.
// Every persisted Run and ledger row comes through production admission and
// execution methods. The capture, model and tool results are labelled fixtures.
func TestRunHotPathPlansAndBaseline(t *testing.T) {
	h := setupRunHarness(t)
	n := 16
	if raw := os.Getenv("JOBFORGE_RUN_PERF_RUNS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 16 || parsed > 256 {
			t.Fatal("JOBFORGE_RUN_PERF_RUNS must be an integer in [16,256]")
		}
		n = parsed
	}
	var version, seqscan, planCache string
	if err := h.Pool.QueryRow(h.Ctx, "select version(),current_setting('enable_seqscan'),current_setting('plan_cache_mode')").Scan(&version, &seqscan, &planCache); err != nil {
		t.Fatal(err)
	}
	t.Logf("RUN-PERF environment: os=%s arch=%s go=%s cpus=%d gomaxprocs=%d postgres=%s pool_max=%d enable_seqscan=%s plan_cache_mode=%s",
		runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0), version, h.Pool.Config().MaxConns, seqscan, planCache)
	t.Logf("RUN-PERF scope: runs=%d tenants=2 workers=2 worker_capacity=2 tenant_capacity=2 profile_capacity=2; synthetic capture/tool/model results, no external HTTP; no direct SQL inserts", n)
	samples := &runPerfSamples{latencies: make(map[string][]time.Duration)}
	seedStart := time.Now()
	for i := range n {
		start := time.Now()
		h.submit(t, []string{"tenant-a", "tenant-b"}[i%2], fmt.Sprintf("run-perf-%04d", i))
		samples.record("submit", time.Since(start))
	}
	t.Logf("RUN-PERF seed: runs=%d elapsed=%s", n, time.Since(seedStart))
	runPerfAnalyze(t, h)
	runPerfExplain(t, h, "claim-ready", "lifecycle.go", "Claim", "where state='ready'",
		[]string{"tenant-a", "tenant-b"}, []string{h.Profile.ID}, h.Principal)

	secondSession, err := h.Store.Register(h.Ctx, "contract-worker-2", uuid.NewString(), "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	workers := []runHarness{*h, *h}
	workers[1].Principal, workers[1].Session = "contract-worker-2", secondSession
	var mu sync.Mutex
	claimedIDs := make(map[string]struct{}, n)
	var firstLease agentrun.Lease
	var wg sync.WaitGroup
	startWorkers := make(chan struct{})
	workStart := time.Now()
	for _, worker := range workers {
		wg.Go(func() {
			<-startWorkers
			for {
				if _, err := worker.Store.HeartbeatSession(worker.Ctx, worker.Principal, worker.Principal, worker.Session.ID); err != nil {
					t.Errorf("performance session heartbeat: %v", err)
					return
				}
				start := time.Now()
				claimed, err := worker.Store.Claim(worker.Ctx, worker.Principal, worker.Session.ID)
				elapsed := time.Since(start)
				if err != nil {
					t.Errorf("performance Claim: %v", err)
					return
				}
				if claimed == nil {
					samples.record("claim-empty", elapsed)
					return
				}
				samples.record("claim", elapsed)
				mu.Lock()
				_, duplicate := claimedIDs[claimed.Lease.RunID]
				claimedIDs[claimed.Lease.RunID] = struct{}{}
				if firstLease.RunID == "" {
					firstLease = claimed.Lease
				}
				mu.Unlock()
				if duplicate {
					t.Errorf("performance Claim returned a duplicate Run")
					return
				}
				runPerfFinish(t, &worker, claimed, samples)
			}
		})
	}
	close(startWorkers)
	wg.Wait()
	elapsed := time.Since(workStart)
	if len(claimedIDs) != n {
		t.Fatalf("performance claimed=%d want=%d", len(claimedIDs), n)
	}
	var succeeded, calls, tools, steps, slots int
	if err := h.Pool.QueryRow(h.Ctx, `select
		(select count(*) from runs where state='succeeded' and outcome='no_action'),
		(select count(*) from physical_calls),(select count(*) from tool_invocations),
		(select count(*) from run_steps),(select coalesce(sum(used),0) from execution_slots)`).Scan(&succeeded, &calls, &tools, &steps, &slots); err != nil {
		t.Fatal(err)
	}
	if succeeded != n || calls != 7*n || tools != 3*n || steps != 6*n || slots != 0 {
		t.Fatalf("performance facts: succeeded=%d calls=%d tools=%d steps=%d slots=%d", succeeded, calls, tools, steps, slots)
	}
	t.Logf("RUN-PERF complete: runs=%d calls=%d tools=%d steps=%d elapsed=%s runs_per_second=%.3f", n, calls, tools, steps, elapsed, float64(n)/elapsed.Seconds())
	samples.report(t)
	runPerfAnalyze(t, h)
	var callID, toolID string
	if err := h.Pool.QueryRow(h.Ctx, `select physical_call_id,tool_invocation_id from physical_calls
		where tenant_id=$1 and run_id=$2 and tool_invocation_id is not null order by ordinal limit 1`, firstLease.TenantID, firstLease.RunID).Scan(&callID, &toolID); err != nil {
		t.Fatal(err)
	}
	runPerfExplain(t, h, "call-identity-lock", "ledger.go", "readCall", "from physical_calls", firstLease.TenantID, firstLease.RunID, callID)
	runPerfExplain(t, h, "call-ordinal", "ledger.go", "ReserveCall", "select coalesce(max(ordinal)", firstLease.TenantID, firstLease.RunID)
	runPerfExplain(t, h, "tool-subcall-sequence", "ledger.go", "checkSubcall", "from physical_calls", firstLease.TenantID, firstLease.RunID, toolID)
	runPerfExplain(t, h, "run-usage", "query.go", "fillBudget", "from physical_calls", firstLease.TenantID, firstLease.RunID)
	runPerfExplain(t, h, "run-tool-count", "query.go", "fillBudget", "from tool_invocations", firstLease.TenantID, firstLease.RunID)
	runPerfExplain(t, h, "tool-identity-lock", "ledger.go", "readTool", "from tool_invocations", toolID)
	// A second realistic distribution keeps completed history and admits one
	// new Run. This plan-only fixture is outside the measured throughput and
	// does not manufacture a selective predicate or force an index scan.
	h.submit(t, "tenant-a", "run-perf-sparse-ready")
	runPerfAnalyze(t, h)
	t.Logf("RUN-PLAN sparse fixture: ready=1 succeeded=%d total_runs=%d", n, n+1)
	runPerfExplain(t, h, "claim-sparse-ready", "lifecycle.go", "Claim", "where state='ready'",
		[]string{"tenant-a", "tenant-b"}, []string{h.Profile.ID}, h.Principal)
}

func runPerfFinish(t *testing.T, h *runHarness, claimed *agentrun.ClaimedRun, samples *runPerfSamples) {
	t.Helper()
	for {
		var result agentrun.StepResult
		if currentRunStep(*claimed).Kind == "model_proposal" {
			request := ledgerRequest(h, *claimed, agentrun.SubcallChat, "")
			start := time.Now()
			ledgerReserve(t, h, request)
			samples.record("reserve-chat", time.Since(start))
			usage := ledgerUsage(10, 5)
			start = time.Now()
			ledgerObserve(t, h, request, "response", "accepted", &usage)
			samples.record("observe-chat-with-usage", time.Since(start))
			snapshot := claimed.Checkpoint.Snapshot
			result = agentrun.StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, PhysicalCallID: request.PhysicalCallID,
				Proposal: &agentrun.Proposal{Decision: "no_action", Summary: "Synthetic performance fixture",
					EvidenceRefs: []string{"business-evidence:" + snapshot.ID + ":ticket", "business-policy:" + snapshot.IndexID + ":P01.1"}}}
		} else {
			result = checkpointFixtureResult(t, h, *claimed, "no_action", false)
		}
		request := checkpointCommitRequest(t, *claimed, result)
		start := time.Now()
		response, err := h.Store.CommitStep(h.Ctx, h.Principal, request)
		samples.record("commit-step", time.Since(start))
		if err != nil {
			t.Fatalf("performance CommitStep %s: %v", request.Step.Kind, err)
		}
		if response.AttemptClosed {
			return
		}
		start = time.Now()
		checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
		samples.record("get-checkpoint", time.Since(start))
		if err != nil {
			t.Fatalf("performance GetCheckpoint: %v", err)
		}
		claimed.Checkpoint = checkpoint
	}
}

type runPerfSamples struct {
	mu        sync.Mutex
	latencies map[string][]time.Duration
}

func (s *runPerfSamples) record(name string, elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latencies[name] = append(s.latencies[name], elapsed)
}

func (s *runPerfSamples) report(t *testing.T) {
	t.Helper()
	names := make([]string, 0, len(s.latencies))
	for name := range s.latencies {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		values := slices.Clone(s.latencies[name])
		slices.Sort(values)
		var total time.Duration
		for _, value := range values {
			total += value
		}
		// Nearest-rank percentiles include all successful calls, including the
		// first calls. Empty Claims are reported separately, never discarded.
		percentile := func(p int) time.Duration { return values[(len(values)*p+99)/100-1] }
		t.Logf("RUN-PERF %s: count=%d sum=%s p50=%s p95=%s p99=%s max=%s", name, len(values), total,
			percentile(50), percentile(95), percentile(99), values[len(values)-1])
	}
}

func runPerfAnalyze(t *testing.T, h *runHarness) {
	t.Helper()
	for _, query := range []string{"analyze runs", "analyze execution_slots", "analyze physical_calls", "analyze tool_invocations"} {
		if _, err := h.Pool.Exec(h.Ctx, query); err != nil {
			t.Fatal(err)
		}
	}
}

// runPerfExplain plans the source query unchanged, including FOR UPDATE, under
// the natural optimizer settings. Rollback releases acquired locks. Reading the
// Go AST avoids a second SQL contract silently drifting away from production.
func runPerfExplain(t *testing.T, h *runHarness, label, file, function, match string, args ...any) {
	t.Helper()
	query := runPerfSourceQuery(t, file, function, match)
	tx, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(h.Ctx) }()
	rows, err := tx.Query(h.Ctx, "explain (analyze, buffers) "+query, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", label, err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "Execution Time:") || !strings.Contains(plan.String(), "Buffers:") {
		t.Fatalf("explain %s did not return analyzed timing/buffers: %s", label, plan.String())
	}
	t.Logf("RUN-PLAN %s (%s:%s):\n%s", label, file, function, plan.String())
}

func runPerfSourceQuery(t *testing.T, name, function, match string) string {
	t.Helper()
	base := filepath.Join("..", "..", "internal", "run", "postgres")
	parse := func(name string) *ast.File {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(base, name), nil, 0)
		if err != nil {
			t.Fatalf("parse production Run SQL source: %v", err)
		}
		return file
	}
	constants := make(map[string]string)
	// Shared projection columns can live beside the production query. Resolve
	// those source constants as well as the original store-level projections;
	// do not replace an unrecognized query with a second hand-written SQL copy.
	for _, source := range []string{"store.go", name} {
		ast.Inspect(parse(source), func(node ast.Node) bool {
			if value, ok := node.(*ast.ValueSpec); ok && len(value.Names) == 1 && len(value.Values) == 1 {
				if literal, ok := value.Values[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
					constants[value.Names[0].Name], _ = strconv.Unquote(literal.Value)
				}
			}
			return true
		})
	}
	var evaluate func(ast.Expr) (string, bool)
	evaluate = func(expr ast.Expr) (string, bool) {
		switch value := expr.(type) {
		case *ast.BasicLit:
			if value.Kind == token.STRING {
				decoded, err := strconv.Unquote(value.Value)
				return decoded, err == nil
			}
		case *ast.Ident:
			text, exists := constants[value.Name]
			return text, exists
		case *ast.BinaryExpr:
			if value.Op == token.ADD {
				left, leftOK := evaluate(value.X)
				right, rightOK := evaluate(value.Y)
				return left + right, leftOK && rightOK
			}
		}
		return "", false
	}
	var matches []string
	for _, declaration := range parse(name).Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "QueryRow" && selector.Sel.Name != "Query") {
				return true
			}
			if query, ok := evaluate(call.Args[1]); ok && strings.Contains(query, match) {
				matches = append(matches, query)
			}
			return true
		})
	}
	if len(matches) != 1 {
		t.Fatalf("production SQL %s:%s containing %q: got %d matches, want 1", name, function, match, len(matches))
	}
	return matches[0]
}
