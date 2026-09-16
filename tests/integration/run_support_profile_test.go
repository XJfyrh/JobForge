package integration

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

// setupSupportProfileHarness exercises the real fixed registration and accounts;
// capture contents and artifact digests are explicitly synthetic mechanism evidence.
func setupSupportProfileHarness(t *testing.T, profileID string) *runHarness {
	t.Helper()
	ctx, pool := setupRunDB(t)
	raw, err := os.ReadFile("../../api/support/profile-v1/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profile agentrun.Profile `json:"profile"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	p := fixture.Profile
	p.ID, p.Executable = profileID, true
	p.Hash, err = agentrun.SupportProfileHash(p)
	if err != nil {
		t.Fatal(err)
	}
	principal := "c3b-runtime-worker"
	options := runpostgres.Options{Profiles: []agentrun.Profile{p}, Workers: []agentrun.WorkerConfig{
		{ID: principal, Tenants: []string{"tenant-north", "tenant-south"}, ProfileIDs: []string{p.ID}, Capacity: 1}},
		TenantCapacity: 1, ProfileCapacity: 1}
	store, err := runpostgres.New(pool, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProfiles(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	limits := agentrun.Usage{Chat: 480, LogicalTools: 320, QueryEmbedding: 320, ProfileMetadataHTTP: 640,
		BusinessToolHTTP: 320, PhysicalHTTP: 1760, ProtocolCorrections: 40, Tokens: 503808000, CostMicroyuan: 5000000}
	batchID := uuid.NewString()
	if err := store.CreateBudget(ctx, agentrun.BudgetSpec{ID: batchID, Scope: "batch", Key: "contract-batch",
		ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(6*time.Hour - time.Minute), Limits: limits}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"tenant-north", "tenant-south"} {
		tenantID := uuid.NewString()
		if err := store.CreateBudget(ctx, agentrun.BudgetSpec{ID: tenantID, Scope: "tenant", Key: tenant,
			ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(6*time.Hour - time.Minute), Limits: limits}); err != nil {
			t.Fatal(err)
		}
		if err := store.BindBudgetTenant(ctx, tenant, batchID, tenantID); err != nil {
			t.Fatal(err)
		}
	}
	capture := &runCaptureFixture{snapshots: make(map[string]agentrun.SnapshotBinding)}
	service, err := agentrun.NewService(store, capture, []string{"tenant-north", "tenant-south"})
	if err != nil {
		t.Fatal(err)
	}
	return &runHarness{Ctx: ctx, Pool: pool, Store: store, Service: service, Profile: p,
		Principal: principal, Capture: capture, Options: options}
}

func supportProfileCapture(t *testing.T, h *runHarness, kind, key, sourceID string, withOrder bool) agentrun.SnapshotBinding {
	t.Helper()
	d, err := agentrun.DecodeSupportDefinition(h.Profile.Definition)
	if err != nil {
		t.Fatal(err)
	}
	observed, _ := time.Parse(time.RFC3339, d.Resources.ObservedAt)
	ticket := business.Ticket{TenantID: "tenant-north", TicketID: "ticket-1", Revision: 1, ObservedAt: observed,
		PolicyVersion: d.Resources.PolicyVersion, Subject: "Synthetic contract ticket", Description: "Deterministic transport fixture", Status: "open"}
	v := business.VersionVector{SchemaVersion: 1, Ticket: business.TicketVersion{ID: ticket.TicketID, Revision: 1},
		Policy: business.PolicyRevision{Version: d.Resources.PolicyVersion, Revision: 2, CorpusSHA256: d.Resources.CorpusSHA256},
		Index:  business.IndexVersion{ID: d.Resources.Tenants[0].IndexID, ProfileHash: d.Resources.IndexProfileHash, ContentHash: d.Resources.Tenants[0].IndexContentHash}}
	if withOrder {
		orderID, deliveryID, revision := "order-1", "delivery-1", int64(1)
		ticket.OrderID = &orderID
		v.Order = business.OrderVersion{ID: &orderID, Exists: true, Revision: &revision}
		v.Delivery = business.DeliveryVersion{ID: &deliveryID, Exists: true, AggregateRevision: &revision}
	}
	ticketRaw, _ := json.Marshal(ticket)
	vectorRaw, _ := json.Marshal(v)
	s := agentrun.SnapshotBinding{TenantID: ticket.TenantID, TicketID: ticket.TicketID, ID: uuid.NewString(),
		ContentHash: agentrun.Fingerprint("synthetic-support-capture", string(ticketRaw), string(vectorRaw)),
		Ticket:      ticketRaw, VersionVector: vectorRaw, IndexID: v.Index.ID, IndexProfileHash: v.Index.ProfileHash}
	h.Capture.snapshots["tenant-north:"+agentrun.SnapshotKey("tenant-north", kind, key, sourceID)] = s
	return s
}

func TestRunSupportProfileAdmissionRejectsWrongResourcesWithoutIdentity(t *testing.T) {
	h := setupSupportProfileHarness(t, "support-admission")
	s := supportProfileCapture(t, h, "submit", "wrong-resource", "", true)
	s.VersionVector = []byte(strings.Replace(string(s.VersionVector), `"revision":2`, `"revision":3`, 1))
	h.Capture.snapshots["tenant-north:"+agentrun.SnapshotKey("tenant-north", "submit", "wrong-resource", "")] = s
	request := submitFixture(h, "wrong-resource")
	if _, err := h.Service.Submit(h.Ctx, "tenant-north", "wrong-resource", request); err != agentrun.ErrProfileUnavailable {
		t.Fatalf("wrong resource: %v", err)
	}
	input := agentrun.Admission{TenantID: "tenant-north", OperationKey: "direct", RequestHash: request.Hash(), Submit: request,
		RunID: uuid.NewString(), BusinessID: uuid.NewString(), FamilyAccountID: uuid.NewString(), OperationID: uuid.NewString(), FirstStepID: uuid.NewString(),
		Profile: h.Profile, Snapshot: s}
	if _, err := h.Store.Admit(h.Ctx, input); err != agentrun.ErrProfileUnavailable {
		t.Fatalf("direct Store bypass: %v", err)
	}
	input.Snapshot = supportProfileCapture(t, h, "submit", "unused", "", true)
	input.Profile.FamilyCostMicroyuan++
	if _, err := h.Store.Admit(h.Ctx, input); err != agentrun.ErrProfileUnavailable {
		t.Fatalf("caller profile tampering: %v", err)
	}
	var count int
	if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from runs)+(select count(*) from business_requests)+
		(select count(*) from budget_accounts where scope='family')+(select count(*) from run_operations)`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected resource persisted identity: count=%d error=%v", count, err)
	}
}

func TestRunSupportProfileRetryAndAcceptedReplay(t *testing.T) {
	h := setupSupportProfileHarness(t, "support-retry")
	supportProfileCapture(t, h, "submit", "submit-root", "", true)
	root := h.submit(t, "tenant-north", "root")
	if _, err := h.Store.Cancel(h.Ctx, "tenant-north", root.ID, "cancel-root"); err != nil {
		t.Fatal(err)
	}
	bad := supportProfileCapture(t, h, "retry", "bad-retry", root.ID, true)
	bad.VersionVector = []byte(strings.Replace(string(bad.VersionVector), `"revision":2`, `"revision":3`, 1))
	h.Capture.snapshots["tenant-north:"+agentrun.SnapshotKey("tenant-north", "retry", "bad-retry", root.ID)] = bad
	retry := agentrun.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 3600}
	if _, err := h.Service.Retry(h.Ctx, "tenant-north", root.ID, "bad-retry", retry); err != agentrun.ErrProfileUnavailable {
		t.Fatalf("wrong retry resource: %v", err)
	}
	supportProfileCapture(t, h, "retry", "good-retry", root.ID, true)
	child, err := h.Service.Retry(h.Ctx, "tenant-north", root.ID, "good-retry", retry)
	if err != nil || child.Run.SnapshotID == root.SnapshotID || child.Run.ProfileHash != root.ProfileHash ||
		child.Run.Budget.Family.ID != root.Budget.Family.ID || child.Run.Budget.Tenant.ID != root.Budget.Tenant.ID || child.Run.Budget.Batch.ID != root.Budget.Batch.ID {
		t.Fatalf("retry did not retain original capability/accounts: %v", err)
	}
	count := h.Capture.count()
	h.Options.Profiles[0].Executable = false
	h.Store, err = runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	h.Capture.unavailable = true
	h.Service, err = agentrun.NewService(h.Store, h.Capture, []string{"tenant-north"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, `update budget_accounts set valid_until=clock_timestamp()-interval '1 second' where scope='batch'`); err != nil {
		t.Fatal(err)
	}
	accepted, err := h.Service.Submit(h.Ctx, "tenant-north", "submit-root", submitFixture(h, "root"))
	if err != nil || !accepted.Reused || accepted.Run.ID != root.ID {
		t.Fatalf("accepted submit lost precedence: %v", err)
	}
	accepted, err = h.Service.Retry(h.Ctx, "tenant-north", root.ID, "good-retry", retry)
	if err != nil || !accepted.Reused || accepted.Run.ID != child.Run.ID || h.Capture.count() != count {
		t.Fatalf("accepted retry revalidated/captured: %v", err)
	}
	request := submitFixture(h, "root")
	accepted, err = h.Store.Admit(h.Ctx, agentrun.Admission{TenantID: "tenant-north", OperationKey: "submit-root",
		RequestHash: request.Hash(), Submit: request, RunID: uuid.NewString(), OperationID: uuid.NewString(), FirstStepID: uuid.NewString()})
	if err != nil || !accepted.Reused || accepted.Run.ID != root.ID {
		t.Fatalf("Store replay must precede even absent new snapshot/profile: %v", err)
	}
}

func TestRunSupportProfileRegistrationRecomputesHashes(t *testing.T) {
	h := setupSupportProfileHarness(t, "support-register")
	if err := h.Store.EnsureProfiles(h.Ctx); err != nil {
		t.Fatal(err)
	}
	h.Options.Profiles[0].Hash = agentrun.Fingerprint("tampered")
	store, err := runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProfiles(h.Ctx); err != agentrun.ErrProfileUnavailable {
		t.Fatalf("claimed hash was trusted: %v", err)
	}
	d, err := agentrun.DecodeSupportDefinition(h.Profile.Definition)
	if err != nil {
		t.Fatal(err)
	}
	d.Program.PromptSHA256 = strings.Repeat("b", 64)
	changed, err := agentrun.BuildSupportProfile(h.Profile.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	h.Options.Profiles = []agentrun.Profile{changed}
	store, err = runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProfiles(h.Ctx); err != agentrun.ErrConflict {
		t.Fatalf("same profile ID overwrote frozen definition: %v", err)
	}
}
