package tasks

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/worker"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Service wires trusted task adapters to a business store and model backend.
type Service struct {
	store ArtifactStore
	model Model
}

// NewService injects business dependencies without changing queue behavior.
func NewService(store ArtifactStore, model Model) *Service {
	return &Service{store: store, model: model}
}

// Register is the only capability dispatch. No payload can register handlers.
func (s *Service) Register(registry *worker.Registry) {
	registry.Register("rag.index", worker.HandlerFunc(s.Execute))
	registry.Register("agent.extract", worker.HandlerFunc(s.Execute))
}

// Execute first resolves business identity, then computes and publishes once.
// Computation can repeat after a crash. Publishing and Complete are separate;
// returning an existing artifact is the recovery path, not checkpoint resume.
func (s *Service) Execute(ctx context.Context, job *worker.ClaimedJob) (_ string, err error) {
	ctx, span := observability.Tracer("jobforge.tasks").Start(ctx, "business."+job.Type)
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, "business execution failed")
		}
		span.End()
	}()
	span.SetAttributes(attribute.String("job_id", job.ID), attribute.String("tenant_id", job.TenantID), attribute.String("type", job.Type))
	if job.TenantID == "" {
		return "", permanent("BUSINESS_TENANT_REQUIRED")
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	p, err := parseInput(job.Type, job.Payload)
	if err != nil {
		return "", err
	}
	canonical, _ := json.Marshal(p)
	var sourceVersion string
	if job.Type == "rag.index" {
		_, sourceVersion, err = loadCorpus()
		if err != nil {
			return "", err
		}
		sourceVersion += indexVersion + EmbedDigest
	} else {
		document, readErr := fixtures.ReadFile("fixtures/purchase-order-v1.txt")
		if readErr != nil {
			return "", permanent("DOCUMENT_INVALID")
		}
		sourceVersion = digest(document) + digest(orderSchema) + ChatDigest + "extract-prompt-v1"
	}
	fingerprint := digest(append(canonical, []byte(sourceVersion)...))
	existing, err := s.store.Lookup(ctx, job.TenantID, job.Type, p.BusinessKey)
	if err == nil {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if existing.Fingerprint != fingerprint {
			return "", permanent("BUSINESS_KEY_CONFLICT")
		}
		span.SetAttributes(attribute.Bool("artifact.reused", true))
		return existing.ResultRef, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	var value any
	if job.Type == "rag.index" {
		value, err = buildIndex(ctx, s.model)
	} else {
		value, err = extractOrder(ctx, s.model)
	}
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > MaxArtifactBytes {
		return "", permanent("ARTIFACT_TOO_LARGE")
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	identity, _ := json.Marshal([]string{job.TenantID, job.Type, p.BusinessKey})
	a := &Artifact{TenantID: job.TenantID, TaskType: job.Type, BusinessKey: p.BusinessKey, ResultRef: "jobforge-artifact:" + digest(identity), Fingerprint: fingerprint, Body: body}
	ctx, publishSpan := observability.Tracer("jobforge.tasks").Start(ctx, "business.artifact.publish")
	stored, applied, err := s.store.Publish(ctx, a)
	publishSpan.SetAttributes(attribute.Bool("artifact.applied", applied))
	if err != nil {
		publishSpan.SetStatus(codes.Error, "artifact publication failed")
	}
	publishSpan.End()
	if err != nil {
		return "", err
	}
	span.SetAttributes(attribute.Bool("artifact.reused", !applied))
	return stored.ResultRef, nil
}
