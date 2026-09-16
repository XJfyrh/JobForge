package run_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/run"
)

func TestFingerprintUsesDomainAndUTF8LengthPrefixes(t *testing.T) {
	if got := run.Fingerprint("fixture.domain.v1", "a\x00b", "é", ""); got != "55ee66df3f97474cee891e6693f3c23015648018abb17cecccc494ab476cdb86" {
		t.Fatalf("UTF-8 byte-length fingerprint changed: %s", got)
	}
	cases := []struct {
		name  string
		left  []string
		right []string
	}{
		{"split_boundary", []string{"ab", "c"}, []string{"a", "bc"}},
		{"delimiter_in_value", []string{"a:b", "c"}, []string{"a", "b:c"}},
		{"empty_final_field", []string{"a", ""}, []string{"a"}},
		{"empty_first_field", []string{"", "a"}, []string{"a", ""}},
		{"unicode_normalization", []string{"é"}, []string{"e\u0301"}},
		{"case_is_preserved", []string{"Ticket-A"}, []string{"ticket-a"}},
		{"space_is_preserved", []string{"ticket-a"}, []string{"ticket-a "}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if run.Fingerprint("fixture.v1", test.left...) == run.Fingerprint("fixture.v1", test.right...) {
				t.Fatal("distinct field boundaries/content collided")
			}
		})
	}
	if run.Fingerprint("submit.v1", "same") == run.Fingerprint("retry.v1", "same") {
		t.Fatal("different operation domains collided")
	}
}

func TestSharedAdmissionFixtureDefaultFingerprints(t *testing.T) {
	content, err := os.ReadFile("../../api/run/v2/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]json.RawMessage
	if err = json.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"submit", "submit_explicit_default"} {
		// Domain hashing requires the transport's frozen default to be filled.
		// This common fixture checks the normalized values, not HTTP strict decode.
		request := run.SubmitRequest{RunTimeoutSeconds: 3600}
		if err = json.Unmarshal(fixture[key], &request); err != nil || request.Validate() != nil {
			t.Fatalf("invalid shared submit fixture: %v", err)
		}
		if got := request.Hash(); got != "896ed10d085c33afafbc0bd66ee7b310b851bec7698cb0feba7add22bfc0a01e" {
			t.Fatalf("%s normalized fingerprint changed: %s", key, got)
		}
	}
	for _, key := range []string{"retry", "retry_explicit_default"} {
		request := run.RetryRequest{RunTimeoutSeconds: 3600}
		if err = json.Unmarshal(fixture[key], &request); err != nil || request.Validate() != nil {
			t.Fatalf("invalid shared retry fixture: %v", err)
		}
		if got := request.Hash("11111111-1111-4111-8111-111111111111"); got != "581b23774bae37543581d8c2948de8db710fef380de9a52926e53064cd699e9d" {
			t.Fatalf("%s normalized retry fingerprint changed: %s", key, got)
		}
	}
}

func TestIdentityKeysAndTimeoutsAreNotSilentlyNormalized(t *testing.T) {
	for _, valid := range []string{"a", "0", "a._:-Z09", strings.Repeat("a", 128)} {
		if !run.ValidIdentifier(valid) {
			t.Fatalf("valid boundary identifier rejected: %q", valid)
		}
	}
	for _, invalid := range []string{"", ".a", "_a", ":a", " a", "a ", "a\n", "a/b", "é", strings.Repeat("a", 129)} {
		if run.ValidIdentifier(invalid) {
			t.Fatalf("invalid identifier normalized/accepted: %q", invalid)
		}
	}
	request := run.SubmitRequest{SchemaVersion: 1, TicketID: "ticket-a", BusinessRequestKey: "intent-a",
		ProfileID: "profile-v1", BudgetBatchID: "batch-a", RunTimeoutSeconds: 3600}
	for _, timeout := range []int64{-1, 0, 1, 3600, 86400, 86401} {
		request.RunTimeoutSeconds = timeout
		valid := timeout >= 1 && timeout <= 86400
		if (request.Validate() == nil) != valid || ((run.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: timeout}).Validate() == nil) != valid {
			t.Fatalf("timeout %d was silently normalized or range changed", timeout)
		}
	}
	request.RunTimeoutSeconds = 3600
	request.SchemaVersion = 2
	if !errors.Is(request.Validate(), run.ErrInvalidArgument) {
		t.Fatal("unknown schema version accepted")
	}
}

func TestSubmitRetryAndSnapshotIdentityScope(t *testing.T) {
	request := run.SubmitRequest{SchemaVersion: 1, TicketID: "ticket-a", BusinessRequestKey: "intent-a",
		ProfileID: "profile-v1", BudgetBatchID: "batch-a", RunTimeoutSeconds: 3600}
	base := request.Hash()
	changes := []func(*run.SubmitRequest){
		func(r *run.SubmitRequest) { r.TicketID = "ticket-b" },
		func(r *run.SubmitRequest) { r.BusinessRequestKey = "intent-b" },
		func(r *run.SubmitRequest) { r.ProfileID = "profile-v2" },
		func(r *run.SubmitRequest) { r.BudgetBatchID = "batch-b" },
		func(r *run.SubmitRequest) { r.RunTimeoutSeconds++ },
	}
	for index, change := range changes {
		changed := request
		change(&changed)
		if changed.Hash() == base {
			t.Fatalf("submit semantic field %d omitted from fingerprint", index)
		}
	}
	retry := run.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 3600}
	if retry.Hash("source-a") == retry.Hash("source-b") || retry.Hash("source-a") == base {
		t.Fatal("retry source or operation omitted from fingerprint")
	}
	snapshot := run.SnapshotKey("tenant-a", "submit", "operation-a", "")
	for _, fields := range [][4]string{
		{"tenant-b", "submit", "operation-a", ""}, {"tenant-a", "retry", "operation-a", ""},
		{"tenant-a", "submit", "operation-b", ""}, {"tenant-a", "submit", "operation-a", "source-a"},
	} {
		if run.SnapshotKey(fields[0], fields[1], fields[2], fields[3]) == snapshot {
			t.Fatal("snapshot capture acquisition identity scope collided")
		}
	}
}
