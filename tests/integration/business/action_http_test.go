package business_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xjfyrh/jobforge/internal/business"
)

func actionExchange(t *testing.T, server *httptest.Server, method, path, key string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func actionServer(t *testing.T, writer, reader *business.Store, keys map[string]business.TrustedActionKey) *httptest.Server {
	t.Helper()
	handler, err := business.NewActionHTTPHandler(writer, reader, map[string]business.Identity{
		"synthetic-action-writer-key": {TenantID: "tenant-north", Role: "action_writer"},
		"synthetic-action-reader-key": {TenantID: "tenant-north", Role: "action_reader"},
		"synthetic-south-writer-key":  {TenantID: "tenant-south", Role: "action_writer"},
		"synthetic-south-reader-key":  {TenantID: "tenant-south", Role: "action_reader"},
	}, keys)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestBusinessActionHTTPFirstReceiptAndStrictFailures(t *testing.T) {
	ctx, admin, _, writer, reader, action, keys := actionFixture(t)
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	a := action.Authorization
	now := time.Now().UTC()
	a.DecidedAt, a.AuthorizedAt = now.Add(-time.Second).UnixMicro(), now.UnixMicro()
	a.PermissionExpiresAt, a.AuthorizationExpiresAt = now.Add(3*time.Second).UnixMicro(), now.Add(3*time.Second).UnixMicro()
	action, err := business.SignAction(a, action.Parameters, private)
	if err != nil {
		t.Fatal(err)
	}
	server := actionServer(t, writer, reader, keys)
	raw, _ := json.Marshal(action)
	path := "/business/v1/actions/apply_ticket_resolution"
	checks := []struct {
		name, key string
		body      []byte
		status    int
	}{
		{"no writer", "synthetic-business-reader-key", raw, 401},
		{"reader", "synthetic-action-reader-key", raw, 403},
		{"foreign writer", "synthetic-south-writer-key", raw, 404},
		{"duplicate", "synthetic-action-writer-key", append([]byte(`{"signature":"duplicate",`), raw[1:]...), 400},
		{"unknown", "synthetic-action-writer-key", append([]byte(`{"unknown":1,`), raw[1:]...), 400},
		{"null", "synthetic-action-writer-key", bytes.Replace(raw, []byte(`"schema_version":1`), []byte(`"schema_version":null`), 1), 400},
	}
	for _, check := range checks {
		status, _ := actionExchange(t, server, http.MethodPost, path, check.key, check.body)
		if status != check.status {
			t.Fatalf("%s status=%d", check.name, status)
		}
	}
	wrong := action
	wrong.Signature = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	wrongRaw, _ := json.Marshal(wrong)
	if status, _ := actionExchange(t, server, http.MethodPost, path, "synthetic-action-writer-key", wrongRaw); status != 403 {
		t.Fatal("invalid first signature accepted", status)
	}
	status, firstRaw := actionExchange(t, server, http.MethodPost, path, "synthetic-action-writer-key", raw)
	var first business.ActionReceipt
	if status != 200 || json.Unmarshal(firstRaw, &first) != nil || first.Validate(action) != nil {
		t.Fatal("real HTTP transaction", status)
	}
	if _, err := admin.Exec(ctx, `update business.policy_versions set revision=revision+1,
		body=jsonb_set(body,'{revision}',to_jsonb(revision+1)) where tenant_id='tenant-north'`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(time.UnixMicro(a.AuthorizationExpiresAt)) + 30*time.Millisecond)
	// Same immutable content returns the first receipt before current versions,
	// expiry or signature/key availability. No new effect is applied.
	keys[a.KeyID] = business.TrustedActionKey{PublicKey: make(ed25519.PublicKey, 32), Tenants: map[string]bool{"tenant-north": true}}
	changedKeys := actionServer(t, writer, reader, keys)
	status, repeated := actionExchange(t, changedKeys, http.MethodPost, path, "synthetic-action-writer-key", wrongRaw)
	if status != 200 || !bytes.Equal(firstRaw, repeated) {
		t.Fatal("repeat did not return first receipt", status)
	}
	changed := action
	changed.Parameters.Summary = "Changed content"
	changedRaw, _ := json.Marshal(changed)
	if status, _ := actionExchange(t, server, http.MethodPost, path, "synthetic-action-writer-key", changedRaw); status != 409 {
		t.Fatal("same operation changed parameters", status)
	}
	receiptPath := "/business/v1/actions/" + a.OperationID + "/receipt"
	for key, want := range map[string]int{"synthetic-action-reader-key": 200, "synthetic-south-reader-key": 404, "synthetic-action-writer-key": 403} {
		status, body := actionExchange(t, server, http.MethodGet, receiptPath, key, nil)
		if status != want || want == 200 && !bytes.Equal(body, firstRaw) {
			t.Fatal("receipt tenant/role/identity", status, want)
		}
	}
	if status, _ := actionExchange(t, server, http.MethodGet, "/business/v1/actions/"+uuid.NewString()+"/receipt", "synthetic-action-reader-key", nil); status != 404 {
		t.Fatal("missing receipt", status)
	}
}

func TestBusinessActionFirstWriteExpiryAndRollback(t *testing.T) {
	ctx, admin, _, writer, _, action, keys := actionFixture(t)
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	expired := action.Authorization
	now := time.Now().UTC()
	expired.DecidedAt = now.Add(-9 * time.Second).UnixMicro()
	expired.AuthorizedAt = now.Add(-8 * time.Second).UnixMicro()
	expired.PermissionExpiresAt = now.Add(-7 * time.Second).UnixMicro()
	expired.AuthorizationExpiresAt = expired.PermissionExpiresAt
	old, err := business.SignAction(expired, action.Parameters, private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ApplyResolution(ctx, old.Authorization.TenantID, old, keys); err != business.ErrAuthorizationExpired {
		t.Fatal("expired authorization first effect", err)
	}
	if _, err := admin.Exec(ctx, `create function business.fail_resolution_fixture() returns trigger
		language plpgsql as $$begin raise exception 'synthetic insert failure'; end$$;
		create trigger fail_resolution_fixture before insert on business.ticket_resolutions
		for each statement execute function business.fail_resolution_fixture()`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ApplyResolution(ctx, action.Authorization.TenantID, action, keys); err == nil {
		t.Fatal("injected receipt insert failure accepted")
	}
	var revision, count int
	if err := admin.QueryRow(ctx, `select revision,(select count(*) from business.ticket_resolutions)
		from business.tickets where tenant_id='tenant-north' and ticket_id='ticket-1'`).Scan(&revision, &count); err != nil || revision != 1 || count != 0 {
		t.Fatal("rollback left ticket or receipt effect", err)
	}
	if _, err := admin.Exec(ctx, `drop trigger fail_resolution_fixture on business.ticket_resolutions;
		drop function business.fail_resolution_fixture()`); err != nil {
		t.Fatal(err)
	}
	first, err := writer.ApplyResolution(ctx, action.Authorization.TenantID, action, keys)
	if err != nil || first.AfterRevision != 2 {
		t.Fatal("failed transaction consumed operation identity", err)
	}
	again, err := writer.ApplyResolution(context.Background(), action.Authorization.TenantID, action, keys)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal("post-rollback first receipt changed", err)
	}
}
