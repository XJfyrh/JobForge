// Package business_test exercises the isolated business database with real
// pgvector. Its synthetic vectors test mechanics, never model quality.
package business_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/business"
)

func isolatedPools(t *testing.T) (context.Context, *pgxpool.Pool, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("JOBFORGE_BUSINESS_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated pgvector database not configured; not business acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("test database configuration rejected")
	}
	t.Cleanup(admin.Close)
	name := "jobforge_business_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatalf("create isolated test database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "drop database "+quoted+" with (force)"); err != nil {
			t.Errorf("drop owned isolated test database: %v", err)
		}
	})
	newPool := func(user string) *pgxpool.Pool {
		config, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal("invalid test configuration")
		}
		config.ConnConfig.Database = name
		if user != "" {
			config.ConnConfig.User, config.ConnConfig.Password = user, user
		}
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal("cannot construct isolated pool")
		}
		t.Cleanup(pool.Close)
		return pool
	}
	bootstrap := newPool("")
	migrator := business.NewMigrator(bootstrap)
	if err := migrator.Up(ctx); err == nil {
		t.Fatal("unmarked database accepted")
	}
	if err := migrator.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := migrator.Down(ctx); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate re-up: %v", err)
	}
	return ctx, bootstrap, newPool("jobforge_business_loader_login"), newPool("jobforge_business_reader")
}

func fixture() (business.Dataset, business.IndexUpload) {
	observed := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	orderID, deliveryID := "order-1", "delivery-1"
	corpus := strings.Repeat("a", 64)
	dataset := business.Dataset{SchemaVersion: 1, DatasetVersion: "business-contract-v1"}
	for _, tenant := range []string{"tenant-north", "tenant-south"} {
		dataset.Policies = append(dataset.Policies, business.PolicyVersion{
			TenantID: tenant, PolicyVersion: "policy-v1", Revision: 1, CorpusSHA256: corpus,
		})
		dataset.Tickets = append(dataset.Tickets, business.Ticket{TenantID: tenant, TicketID: "ticket-1",
			Revision: 1, ObservedAt: observed, OrderID: &orderID, PolicyVersion: "policy-v1",
			Subject: "Delivery inquiry", Description: "Synthetic contract fixture", Status: "open"})
		dataset.Orders = append(dataset.Orders, business.Order{TenantID: tenant, OrderID: orderID,
			Revision: 1, DeliveryID: &deliveryID, Status: "in_transit", OrderedAt: observed.Add(-72 * time.Hour),
			PromisedDeliveryAt: observed.Add(-24 * time.Hour)})
		dataset.Deliveries = append(dataset.Deliveries, business.Delivery{TenantID: tenant, DeliveryID: deliveryID,
			OrderID: orderID, AggregateRevision: 1, Status: "in_transit", Events: []business.DeliveryEvent{{
				EventID: "event-1", OccurredAt: observed.Add(-48 * time.Hour), Status: "shipped", Note: "Synthetic dispatch"}}})
	}
	upload := business.IndexUpload{SchemaVersion: 1, TenantID: "tenant-north", PrepareID: uuid.NewString(),
		Profile: business.IndexProfile{PolicyVersion: "policy-v1", CorpusSHA256: corpus,
			ChunkerVersion: business.ChunkerVersion, EmbeddingModel: business.EmbeddingModel,
			EmbeddingDigest: business.EmbeddingDigest, Dimensions: business.Dimensions}}
	for i, id := range []string{"chunk-a", "chunk-b", "chunk-c", "chunk-d"} {
		vector := make([]float64, business.Dimensions)
		vector[i] = 1
		upload.Chunks = append(upload.Chunks, business.IndexChunk{ChunkID: id, Source: "contract.md", Text: id, Embedding: vector})
	}
	return dataset, upload
}

func TestBusinessSnapshotsHTTPAndPermissions(t *testing.T) {
	ctx, bootstrap, loaderPool, runtimePool := isolatedPools(t)
	loader, runtime := business.NewStore(loaderPool), business.NewStore(runtimePool)
	if err := runtime.CheckRuntimeRole(ctx); err != nil {
		t.Fatalf("runtime identity: %v", err)
	}
	if err := business.NewStore(bootstrap).CheckRuntimeRole(ctx); err == nil {
		t.Fatal("bootstrap may not serve HTTP")
	}
	if err := loader.CheckRuntimeRole(ctx); err == nil {
		t.Fatal("loader may not serve HTTP")
	}
	if err := runtime.CheckReady(ctx); err != nil {
		t.Fatalf("runtime readiness: %v", err)
	}
	dataset, upload := fixture()
	if err := runtime.CheckTenantReady(ctx, "tenant-north"); !errors.Is(err, business.ErrDependencyUnavailable) {
		t.Fatalf("unseeded tenant ready: %v", err)
	}
	if err := loader.ImportDataset(ctx, dataset); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := loader.ImportDataset(ctx, dataset); err != nil {
		t.Fatalf("repeat seed: %v", err)
	}
	if err := runtime.CheckTenantReady(ctx, "tenant-north"); !errors.Is(err, business.ErrDependencyUnavailable) {
		t.Fatalf("unpublished tenant ready: %v", err)
	}
	req := business.SnapshotRequest{SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "request-1"}
	if _, _, err := runtime.CreateSnapshot(ctx, "tenant-north", req); !errors.Is(err, business.ErrDependencyUnavailable) {
		t.Fatalf("snapshot without published index: %v", err)
	}
	index, reused, err := loader.PublishIndex(ctx, upload)
	if err != nil || reused {
		t.Fatalf("publish: reused=%v err=%v", reused, err)
	}
	upload.PrepareID = uuid.NewString()
	second, reused, err := loader.PublishIndex(ctx, upload)
	if err != nil || !reused || second.ID != index.ID {
		t.Fatalf("publication idempotency: reused=%v err=%v", reused, err)
	}
	upload.Chunks[0].Text = "conflicting paragraph"
	if _, _, err := loader.PublishIndex(ctx, upload); !errors.Is(err, business.ErrConflict) {
		t.Fatalf("same profile replacement: %v", err)
	}
	upload.Chunks[0].Text = "chunk-a"
	upload.TenantID = "tenant-south"
	if _, _, err := loader.PublishIndex(ctx, upload); err != nil {
		t.Fatalf("second tenant publication: %v", err)
	}
	if err := runtime.CheckTenantReady(ctx, "tenant-north"); err != nil {
		t.Fatalf("published tenant not ready: %v", err)
	}
	var wait sync.WaitGroup
	ids := make(chan string, 12)
	for range 12 {
		wait.Go(func() {
			snapshot, _, err := runtime.CreateSnapshot(ctx, "tenant-north", req)
			if err != nil {
				t.Errorf("concurrent snapshot: %v", err)
				return
			}
			ids <- snapshot.ID
		})
	}
	wait.Wait()
	close(ids)
	var snapshotID string
	for id := range ids {
		if snapshotID != "" && snapshotID != id {
			t.Fatal("concurrent idempotency created multiple snapshots")
		}
		snapshotID = id
	}
	if snapshotID == "" {
		t.Fatal("no accepted snapshot")
	}
	req.TicketID = "different-ticket"
	if _, _, err := runtime.CreateSnapshot(ctx, "tenant-north", req); !errors.Is(err, business.ErrConflict) {
		t.Fatalf("same key changed input: %v", err)
	}
	if _, err := runtime.GetSnapshot(ctx, "tenant-south", snapshotID); !errors.Is(err, business.ErrNotFound) {
		t.Fatalf("cross tenant snapshot: %v", err)
	}
	// Changing mutable facts must not change already captured evidence.
	if _, err := bootstrap.Exec(ctx, `update business.orders set revision=2,
 body=jsonb_set(jsonb_set(body,'{revision}','2'),'{status}','"delivered"')
 where tenant_id='tenant-north' and order_id='order-1'`); err != nil {
		t.Fatalf("source update: %v", err)
	}
	order, err := runtime.GetOrder(ctx, "tenant-north", snapshotID)
	if err != nil || order.Order.Revision != 1 || order.Order.Status != "in_transit" {
		t.Fatalf("immutable order copy: %v", err)
	}
	req.TicketID, req.RequestKey = "ticket-1", "fresh-request"
	fresh, _, err := runtime.CreateSnapshot(ctx, "tenant-north", req)
	if err != nil || fresh.Order.Revision != 2 || !fresh.AsOf.Equal(dataset.Tickets[0].ObservedAt) {
		t.Fatalf("fresh facts and fixed observation time: %v", err)
	}
	search := business.SearchRequest{EmbeddingModel: business.EmbeddingModel,
		EmbeddingDigest: business.EmbeddingDigest, QueryVector: upload.Chunks[0].Embedding}
	hits, err := runtime.SearchPolicies(ctx, "tenant-north", snapshotID, search)
	if err != nil || len(hits) != 3 || hits[0].ChunkID != "chunk-a" || *hits[0].Distance != 0 || hits[1].ChunkID != "chunk-b" {
		t.Fatalf("exact cosine and tie ordering: %v", err)
	}
	for _, vector := range [][]float64{{1}, make([]float64, business.Dimensions)} {
		search.QueryVector = vector
		if _, err := runtime.SearchPolicies(ctx, "tenant-north", snapshotID, search); !errors.Is(err, business.ErrInvalidArgument) {
			t.Fatalf("invalid vector accepted: %v", err)
		}
	}
	search.QueryVector = make([]float64, business.Dimensions)
	search.QueryVector[0] = math.NaN()
	if _, err := runtime.SearchPolicies(ctx, "tenant-north", snapshotID, search); !errors.Is(err, business.ErrInvalidArgument) {
		t.Fatalf("nonfinite vector accepted: %v", err)
	}
	for _, extreme := range []float64{1e38, 1e-30} {
		search.QueryVector[0] = extreme
		if _, err := runtime.SearchPolicies(ctx, "tenant-north", snapshotID, search); !errors.Is(err, business.ErrInvalidArgument) {
			t.Fatalf("unsafe pgvector norm accepted: %v", err)
		}
	}
	for _, query := range []string{
		"create table business.forbidden(id int)", "create schema forbidden", "create role forbidden",
		"update business.orders set revision=999", "delete from business.snapshots",
		"update business.policy_chunks set body='replacement'", "set role jobforge_business_owner",
	} {
		if _, err := runtimePool.Exec(ctx, query); err == nil {
			t.Fatalf("runtime privilege escaped: %s", query)
		}
	}
	if _, err := loaderPool.Exec(ctx, "create table business.forbidden(id int)"); err == nil {
		t.Fatal("loader has DDL privilege")
	}
	testHTTP(t, runtime, snapshotID)
}

func testHTTP(t *testing.T, store *business.Store, snapshotID string) {
	t.Helper()
	keys := map[string]business.Identity{
		"contract-north-reader":   {TenantID: "tenant-north", Role: "reader"},
		"contract-south-reader":   {TenantID: "tenant-south", Role: "reader"},
		"contract-north-operator": {TenantID: "tenant-north", Role: "operator"},
	}
	handler, err := business.NewHTTPHandler(store, keys)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	path := "/business/v1/snapshots/" + snapshotID
	tinyVector := make([]float64, business.Dimensions)
	for i := range tinyVector {
		tinyVector[i] = 1e-23
	}
	tinyBody, err := json.Marshal(business.SearchRequest{EmbeddingModel: business.EmbeddingModel,
		EmbeddingDigest: business.EmbeddingDigest, QueryVector: tinyVector})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path, key, body string
		status                        int
	}{
		{"authorized", "GET", path, "contract-north-reader", "", 200},
		{"no key", "GET", path, "", "", 401},
		{"cross tenant", "GET", path, "contract-south-reader", "", 404},
		{"reader cannot create", "POST", "/business/v1/snapshots", "contract-north-reader", `{}`, 403},
		{"unknown field", "POST", "/business/v1/snapshots", "contract-north-operator", `{"tenant_id":"tenant-south"}`, 400},
		{"trailing json", "POST", "/business/v1/snapshots", "contract-north-operator", `{} {}`, 400},
		{"oversized", "POST", "/business/v1/snapshots", "contract-north-operator", strings.Repeat(" ", 4097), 400},
		{"query override", "GET", path + "?tenant_id=tenant-south", "contract-north-reader", "", 400},
		{"unknown evidence", "GET", path + "/evidence/secret", "contract-north-reader", "", 404},
		{"order", "GET", path + "/order", "contract-north-reader", "", 200},
		{"delivery", "GET", path + "/delivery", "contract-north-reader", "", 200},
		{"policy reference", "GET", path + "/policies/chunk-a", "contract-north-reader", "", 200},
		{"bad profile", "POST", path + "/policies/search", "contract-north-reader", `{"embedding_model":"unregistered","embedding_digest":"bad","query_vector":[1]}`, 409},
		{"squared underflow", "POST", path + "/policies/search", "contract-north-reader", string(tinyBody), 400},
		{"first snapshot", "POST", "/business/v1/snapshots", "contract-north-operator", `{"schema_version":1,"ticket_id":"ticket-1","request_key":"http-key"}`, 201},
		{"repeated snapshot", "POST", "/business/v1/snapshots", "contract-north-operator", `{"schema_version":1,"ticket_id":"ticket-1","request_key":"http-key"}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tc.method, server.URL+tc.path, bytes.NewBufferString(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				req.Header.Set("Authorization", "Bearer "+tc.key)
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(io.LimitReader(resp.Body, business.MaxToolBytes+1))
			if err != nil || len(body) > business.MaxToolBytes || resp.StatusCode != tc.status {
				t.Fatalf("status=%d expected=%d bytes=%d read=%v", resp.StatusCode, tc.status, len(body), err)
			}
			if !json.Valid(body) || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("invalid protected response")
			}
			if tc.status >= 400 && !bytes.Contains(body, []byte(`"error":{"code":`)) {
				t.Fatal("missing stable error envelope")
			}
		})
	}
}
