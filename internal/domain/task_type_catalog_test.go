package domain

import (
	"slices"
	"strings"
	"testing"
)

func TestTaskTypeCatalogCanonicalizesAndCopies(t *testing.T) {
	input := []string{"pagewise.reindex", "demo.echo", "demo.fail"}
	catalog, err := NewTaskTypeCatalog(input)
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}

	want := []string{"demo.echo", "demo.fail", "pagewise.reindex"}
	if !slices.Equal(catalog.Types(), want) {
		t.Fatalf("types = %v, want %v", catalog.Types(), want)
	}
	if !catalog.Contains("demo.echo") || catalog.Contains("demo.unknown") {
		t.Fatal("catalog membership is incorrect")
	}
	if catalog.Size() != len(want) || len(catalog.Fingerprint()) != 64 {
		t.Fatalf("size/fingerprint = %d/%q", catalog.Size(), catalog.Fingerprint())
	}

	input[0] = "mutated"
	returned := catalog.Types()
	returned[0] = "mutated"
	if !slices.Equal(catalog.Types(), want) {
		t.Fatal("catalog must not expose mutable backing storage")
	}
}

func TestTaskTypeCatalogFingerprintIgnoresInputOrder(t *testing.T) {
	a, err := NewTaskTypeCatalog([]string{"demo.echo", "demo.fail"})
	if err != nil {
		t.Fatalf("new catalog a: %v", err)
	}
	b, err := NewTaskTypeCatalog([]string{"demo.fail", "demo.echo"})
	if err != nil {
		t.Fatalf("new catalog b: %v", err)
	}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("fingerprints differ: %s != %s", a.Fingerprint(), b.Fingerprint())
	}
}

func TestTaskTypeCatalogRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		types []string
	}{
		{name: "empty"},
		{name: "empty item", types: []string{"demo.echo", ""}},
		{name: "duplicate", types: []string{"demo.echo", "demo.echo"}},
		{name: "whitespace", types: []string{"demo echo"}},
		{name: "leading punctuation", types: []string{".demo"}},
		{name: "too long", types: []string{"d" + strings.Repeat("x", 128)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewTaskTypeCatalog(tt.types); err == nil {
				t.Fatal("expected invalid catalog")
			}
		})
	}
}

func TestDefaultTaskTypeCatalog(t *testing.T) {
	catalog, err := NewTaskTypeCatalog(DefaultTaskTypeNames())
	if err != nil {
		t.Fatalf("default catalog: %v", err)
	}
	if catalog.Size() != 6 {
		t.Fatalf("default catalog size = %d, want 6", catalog.Size())
	}
	for _, taskType := range []string{
		"demo.echo",
		"demo.sleep",
		"demo.fail",
		"demo.idempotent_effect",
		"demo.http",
		"pagewise.reindex",
	} {
		if !catalog.Contains(taskType) {
			t.Errorf("default catalog missing %q", taskType)
		}
	}
}
