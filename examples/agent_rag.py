"""Run both real tasks and inspect their protected business artifacts."""

from __future__ import annotations

import argparse
import json
import os
import time
from datetime import datetime, timedelta, timezone
from uuid import uuid4

import httpx
from jobforge import JobForgeClient, NotFoundError
from jobforge.models import Job, JobState
from opentelemetry import trace
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator


def wait_terminal(client: JobForgeClient, job_id: str) -> Job:
    """Poll within a total budget; submission itself is never repeated."""
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        job = client.get(job_id)
        if job.state.is_terminal:
            return job
        time.sleep(0.2)
    raise TimeoutError(f"job {job_id} did not finish within 180 seconds")


def main() -> None:
    """Submit real tasks and verify model outputs, references, and isolation."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-url", default="http://localhost:8080")
    parser.add_argument("--artifact-url", default="http://localhost:8081")
    parser.add_argument("--queue", default="default")
    args = parser.parse_args()
    provider = TracerProvider(
        resource=Resource.create({"service.name": "jobforge-sdk"})
    )
    if os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT"):
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import (
            OTLPSpanExporter,
        )
        from opentelemetry.sdk.trace.export import BatchSpanProcessor

        provider.add_span_processor(
            BatchSpanProcessor(
                OTLPSpanExporter(timeout=2),
                max_queue_size=2048,
                schedule_delay_millis=1000,
            )
        )
    trace.set_tracer_provider(provider)
    key = os.environ.get("JOBFORGE_API_KEY", "dev-api-key")
    foreign_key = os.environ.get("JOBFORGE_FOREIGN_API_KEY", "other-api-key")
    run_id = uuid4().hex
    with (
        JobForgeClient(args.api_url, key) as client,
        JobForgeClient(args.api_url, foreign_key) as foreign,
        httpx.Client(
            base_url=args.artifact_url,
            headers={"Authorization": f"Bearer {key}"},
            timeout=65,
        ) as artifacts,
    ):
        for task_type, versions in (
            ("rag.index", {"corpus_version": "handbook-v1"}),
            (
                "agent.extract",
                {
                    "document_version": "purchase-order-v1",
                    "schema_version": "purchase-order-v1",
                },
            ),
        ):
            payload = {
                "version": 1,
                "business_key": f"{task_type}-{run_id}",
                **versions,
            }
            past = datetime.now(timezone.utc) - timedelta(days=1)
            with trace.get_tracer("jobforge.demo").start_as_current_span(
                f"demo.{task_type}"
            ):
                submitted = client.submit(
                    args.queue,
                    task_type,
                    payload,
                    run_at=past,
                    timeout_seconds=150,
                    idempotency_key=f"{task_type}-{run_id}",
                )
                repeated = client.submit(
                    args.queue,
                    task_type,
                    payload,
                    run_at=past,
                    timeout_seconds=150,
                    idempotency_key=f"{task_type}-{run_id}",
                )
                assert repeated.job_id == submitted.job_id and repeated.deduplicated
                job = wait_terminal(client, submitted.job_id)
                assert job.state == JobState.SUCCEEDED, (job.state, job.attempts)
                assert job.result_ref and job.result_ref.startswith(
                    "jobforge-artifact:"
                )
                assert job.attempts[-1].outcome == "succeeded"
                artifact_id = job.result_ref.split(":", 1)[1]
                path = f"/v1/artifacts/{artifact_id}"
                headers: dict[str, str] = {}
                TraceContextTextMapPropagator().inject(headers)
                response = artifacts.get(path, headers=headers)
                response.raise_for_status()
                stored = response.json()
                body = stored["body"]
                assert stored["result_ref"] == job.result_ref
                denied = artifacts.get(
                    path, headers={**headers, "Authorization": f"Bearer {foreign_key}"}
                )
                assert denied.status_code == 404, denied.status_code
                try:
                    foreign.get(job.id)
                except NotFoundError:
                    pass
                else:
                    raise AssertionError("foreign tenant read job")
                if task_type == "rag.index":
                    assert body["dimensions"] == 384 and len(body["chunks"]) == 6
                    assert all(len(c["vector"]) == 384 for c in body["chunks"])
                    for check in body["checks"]:
                        assert check["actual_source"] == check["expected_source"]
                        search = artifacts.post(
                            path + "/search",
                            headers=headers,
                            json={"query": check["query"], "k": 1},
                        )
                        search.raise_for_status()
                        assert (
                            search.json()["hits"][0]["source"]
                            == check["expected_source"]
                        )
                    summary = {"chunks": len(body["chunks"]), "retrieval_checks": 3}
                else:
                    order = body["order"]
                    assert order["order_id"] == "PO-2026-0042"
                    assert order["supplier"] == "Northwind Components"
                    assert order["quantity"] == 12 and order["total_amount"] == 300
                    assert order["currency"] == "USD"
                    assert order["delivery_date"] == "2026-10-01"
                    assert 1 <= body["model_calls"] <= 2
                    summary = {"schema_valid": True, "model_calls": body["model_calls"]}
                print(
                    json.dumps(
                        {
                            "task": task_type,
                            "job_id": job.id,
                            "trace_id": job.trace_id,
                            "state": job.state.value,
                            "result_ref": job.result_ref,
                            "artifact_url": args.artifact_url + path,
                            **summary,
                        }
                    ),
                    flush=True,
                )
    provider.shutdown()


if __name__ == "__main__":
    main()
