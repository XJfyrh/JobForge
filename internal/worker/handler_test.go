package worker

import (
	"context"
	"slices"
	"testing"
)

func TestRegistryTypesAreSorted(t *testing.T) {
	registry := NewRegistry()
	handler := HandlerFunc(func(context.Context, *ClaimedJob) (string, error) { return "", nil })
	registry.Register("pagewise.reindex", handler)
	registry.Register("demo.fail", handler)
	registry.Register("demo.echo", handler)

	want := []string{"demo.echo", "demo.fail", "pagewise.reindex"}
	if !slices.Equal(registry.Types(), want) {
		t.Fatalf("types = %v, want %v", registry.Types(), want)
	}
	if registry.Lookup("demo.echo") == nil || registry.Lookup("demo.unknown") != nil {
		t.Fatal("registry lookup is incorrect")
	}
}

func TestRegistryRejectsInvalidRegistrations(t *testing.T) {
	validHandler := HandlerFunc(func(context.Context, *ClaimedJob) (string, error) { return "", nil })
	tests := []struct {
		name     string
		taskType string
		handler  Handler
		prepare  func(*Registry)
	}{
		{name: "empty type", handler: validHandler},
		{name: "invalid type", taskType: "demo type", handler: validHandler},
		{name: "nil handler", taskType: "demo.echo"},
		{name: "typed nil handler", taskType: "demo.echo", handler: HandlerFunc(nil)},
		{
			name:     "duplicate",
			taskType: "demo.echo",
			handler:  validHandler,
			prepare: func(registry *Registry) {
				registry.Register("demo.echo", validHandler)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewRegistry()
			if tt.prepare != nil {
				tt.prepare(registry)
			}
			defer func() {
				if recover() == nil {
					t.Fatal("expected registration to panic")
				}
			}()
			registry.Register(tt.taskType, tt.handler)
		})
	}
}
