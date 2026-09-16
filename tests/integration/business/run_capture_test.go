package business_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/run/businessclient"
)

func TestRunCaptureUsesRealBusinessHTTPAndPostgreSQL(t *testing.T) {
	ctx, _, loaderPool, runtimePool := isolatedPools(t)
	loader, runtime := business.NewStore(loaderPool), business.NewStore(runtimePool)
	dataset, upload := fixture()
	if err := loader.ImportDataset(ctx, dataset); err != nil {
		t.Fatal(err)
	}
	// Fixture vectors test persistence/transport identity, not model quality.
	if _, _, err := loader.PublishIndex(ctx, upload); err != nil {
		t.Fatal(err)
	}
	const operator = "contract-run-capture-operator"
	handler, err := business.NewHTTPHandler(runtime, map[string]business.Identity{operator: {TenantID: "tenant-north", Role: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := businessclient.New(server.URL, map[string]string{"tenant-north": operator})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Capture(ctx, "tenant-north", "ticket-1", "run-capture-stable")
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Capture(ctx, "tenant-north", "ticket-1", "run-capture-stable")
	if err != nil || first.ID != second.ID || first.ContentHash != second.ContentHash {
		t.Fatal("physical retry changed immutable capture identity")
	}
	saved, err := runtime.GetSnapshot(ctx, "tenant-north", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	vector, err := json.Marshal(saved.VersionVector())
	if err != nil || string(first.VersionVector) != string(vector) || first.IndexID != saved.Index.ID || first.IndexProfileHash != saved.Index.ProfileHash {
		t.Fatal("control binding does not match the persisted business version vector")
	}
}
