"""Exercise real Compose failures and query Jaeger, Prometheus and Grafana.

Run only against a disposable local Compose project. The script stops model,
collector and Worker containers, restores them in finally, and leaves reports
on stdout. No fixture model or direct job-state UPDATE is used.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import time
from collections.abc import Callable
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any
from uuid import uuid4

import httpx
from jobforge import JobForgeClient
from jobforge.models import Job, JobState
from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor


def eventually(check: Callable[[], Any], description: str, seconds: int = 90) -> Any:
    """Bound every observation wait; failures retain the relevant description."""
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        value = check()
        if value:
            return value
        time.sleep(0.5)
    raise AssertionError(f"timed out: {description}")


def main() -> None:
    """Validate real backend data after faults, using installed SDK calls."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", default="jobforge-agent-rag")
    parser.add_argument("--api-url", default="http://localhost:8080")
    parser.add_argument("--artifact-url", default="http://localhost:8081")
    parser.add_argument("--prometheus-url", default="http://localhost:9091")
    parser.add_argument("--jaeger-url", default="http://localhost:16686")
    parser.add_argument("--grafana-url", default="http://localhost:3000")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    provider = TracerProvider(
        resource=Resource.create({"service.name": "jobforge-sdk"})
    )
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(timeout=2)))
    trace.set_tracer_provider(provider)
    tracer = trace.get_tracer("jobforge.observability.acceptance")
    key = os.environ.get("JOBFORGE_API_KEY", "dev-api-key")
    reports: list[dict[str, Any]] = []

    def compose(*command: str) -> None:
        subprocess.run(
            [
                "docker",
                "compose",
                "-p",
                args.project,
                "-f",
                "deploy/compose.yaml",
                "--profile",
                "models",
                "--profile",
                "obs",
                *command,
            ],
            cwd=root,
            check=True,
            timeout=45,
            capture_output=True,
        )

    with JobForgeClient(args.api_url, key) as client, httpx.Client(timeout=10) as http:

        def query(expression: str) -> list[Any]:
            response = http.get(
                args.prometheus_url + "/api/v1/query", params={"query": expression}
            )
            response.raise_for_status()
            return list(response.json()["data"]["result"])

        def metric(expression: str) -> float:
            values = query(expression)
            return sum(float(v["value"][1]) for v in values)

        def alerts() -> set[str]:
            response = http.get(args.prometheus_url + "/api/v1/alerts")
            response.raise_for_status()
            return {
                a["labels"]["alertname"]
                for a in response.json()["data"]["alerts"]
                if a["state"] == "firing"
            }

        def submit(task_type: str, timeout: int = 150, attempts: int = 3) -> str:
            versions = (
                {"corpus_version": "handbook-v1"}
                if task_type == "rag.index"
                else {
                    "document_version": "purchase-order-v1",
                    "schema_version": "purchase-order-v1",
                }
            )
            return client.submit(
                "default",
                task_type,
                {"version": 1, "business_key": uuid4().hex, **versions},
                timeout_seconds=timeout,
                max_attempts=attempts,
                run_at=datetime.now(timezone.utc) - timedelta(days=1),
            ).job_id

        def state(job_id: str, expected: JobState) -> Job | None:
            job = client.get(job_id)
            if job.state.is_terminal and job.state != expected:
                raise AssertionError(
                    f"{job.id}: expected {expected}, got {job.state}: {job.attempts}"
                )
            return job if job.state == expected else None

        def wait(job_id: str, expected: JobState) -> Job:
            return eventually(
                lambda: state(job_id, expected), f"{job_id} {expected}", 180
            )

        def inspect(job: Job, scenario: str) -> None:
            assert job.result_ref and job.state == JobState.SUCCEEDED
            response = http.get(
                args.artifact_url + "/v1/artifacts/" + job.result_ref.split(":", 1)[1],
                headers={"Authorization": f"Bearer {key}"},
            )
            response.raise_for_status()
            artifact = response.json()["body"]
            if job.type == "rag.index":
                assert artifact["dimensions"] == 384 and len(artifact["chunks"]) == 6
                assert all(
                    c["actual_source"] == c["expected_source"]
                    for c in artifact["checks"]
                )
            else:
                assert artifact["order"]["order_id"] == "PO-2026-0042"
                assert artifact["order"]["total_amount"] == 300
            report = {
                "scenario": scenario,
                "type": job.type,
                "job_id": job.id,
                "trace_id": job.trace_id,
                "result_ref": job.result_ref,
                "attempts": [a.outcome for a in job.attempts],
            }
            reports.append(report)
            print(json.dumps(report), flush=True)

        def check_trace(job: Job, *required: str) -> None:
            provider.force_flush(timeout_millis=5000)

            def present() -> bool:
                response = http.get(args.jaeger_url + "/api/traces/" + job.trace_id)
                if response.status_code == 404:
                    return False
                response.raise_for_status()
                data = response.json()["data"]
                names = {s["operationName"] for d in data for s in d["spans"]}
                for secret in ("Northwind Components", "PO-2026-0042", key):
                    assert secret not in response.text, "sensitive content in trace"
                return set(required).issubset(names)

            eventually(present, f"persisted trace {job.trace_id}: {required}", 30)

        try:
            eventually(
                lambda: metric('sum(up{job=~"jobforge-.+"})') == 7,
                "seven healthy scrape targets",
            )
            compose("kill", "-s", "SIGHUP", "prometheus")
            print(
                "FAULT collector unavailable: both real tasks must still succeed",
                flush=True,
            )
            compose("stop", "otel-collector")
            outage: list[Job] = []
            for task_type in ("rag.index", "agent.extract"):
                with tracer.start_as_current_span("acceptance.collector_outage"):
                    job = wait(submit(task_type), JobState.SUCCEEDED)
                    inspect(job, "collector_outage")
                    outage.append(job)
            compose("start", "otel-collector")
            for previous in outage:
                current = client.get(previous.id)
                assert (
                    current.result_ref == previous.result_ref
                    and current.state == previous.state
                )

            print(
                "FAULT workers stopped: backlog and alerts must become visible",
                flush=True,
            )
            compose("stop", "-t", "5", "worker-1", "worker-2")
            with tracer.start_as_current_span("acceptance.backlog"):
                pending = [submit(t) for t in ("rag.index", "agent.extract")]
                eventually(
                    lambda: metric('sum(jobforge_queue_depth{state="ready"})') >= 2,
                    "ready backlog",
                )
                eventually(
                    lambda: {
                        "JobForgeServiceDown",
                        "JobForgeBacklogWithoutWorkers",
                    }.issubset(alerts()),
                    "service-down and no-worker alerts",
                    160,
                )
                print(
                    json.dumps({"alerts_during_outage": sorted(alerts())}), flush=True
                )
                compose("start", "worker-1")
                for job_id in pending:
                    job = wait(job_id, JobState.SUCCEEDED)
                    inspect(job, "backlog_recovery")
            eventually(
                lambda: metric('sum(jobforge_queue_depth{state="ready"})') == 0,
                "backlog returns to zero",
            )

            for task_type in ("rag.index", "agent.extract"):
                print(
                    f"FAULT {task_type}: backend unavailable then automatic retry",
                    flush=True,
                )
                with tracer.start_as_current_span("acceptance.automatic_retry"):
                    compose("stop", "ollama")
                    job_id = submit(task_type)
                    wait(job_id, JobState.RETRY_WAIT)
                    compose("start", "ollama")
                    job = wait(job_id, JobState.SUCCEEDED)
                    assert (
                        job.attempt >= 2 and job.attempts[0].outcome == "failed_retry"
                    )
                    inspect(job, "automatic_retry")
                check_trace(
                    job,
                    "sdk.submit",
                    "http.submit_job",
                    "gateway.fail_job",
                    "gateway.complete_job",
                    "business." + task_type,
                )

                print(f"FAULT {task_type}: timeout then manual retry", flush=True)
                with tracer.start_as_current_span("acceptance.timeout_manual_retry"):
                    compose("pause", "ollama")
                    job_id = submit(task_type, timeout=5, attempts=1)
                    dead = wait(job_id, JobState.DEAD)
                    assert (
                        dead.attempts[0].error_code == "TIMEOUT" and not dead.result_ref
                    )
                    compose("unpause", "ollama")
                    clone = client.retry(job_id)
                    job = wait(clone.job_id, JobState.SUCCEEDED)
                    assert job.retry_of_job_id == dead.id
                    inspect(job, "timeout_manual_retry")
                check_trace(
                    job, "http.retry_job", "gateway.fail_job", "gateway.complete_job"
                )

                print(
                    f"FAULT {task_type}: SIGKILL Worker, natural lease expiry, re-execution",
                    flush=True,
                )
                with tracer.start_as_current_span("acceptance.worker_crash"):
                    compose("pause", "ollama")
                    job_id = submit(task_type)
                    wait(job_id, JobState.RUNNING)
                    compose("kill", "-s", "SIGKILL", "worker-1")
                    compose("unpause", "ollama")
                    compose("start", "worker-1")
                    job = wait(job_id, JobState.SUCCEEDED)
                    assert (
                        job.attempt == 2 and job.attempts[0].outcome == "lease_expired"
                    )
                    inspect(job, "worker_crash")
                check_trace(
                    job,
                    "scheduler.recover_lease",
                    "gateway.complete_job",
                    "business." + task_type,
                )

            compose("start", "worker-2")
            eventually(
                lambda: (
                    "JobForgeServiceDown" not in alerts()
                    and "JobForgeBacklogWithoutWorkers" not in alerts()
                ),
                "service and backlog alerts resolve",
            )
            eventually(
                lambda: {"JobForgeDeadLetter", "JobForgeLeaseRecovery"}.issubset(
                    alerts()
                ),
                "DLQ and recovery alerts",
            )
            evidence: dict[str, float] = {}
            for task_type in ("rag.index", "agent.extract"):
                for outcome in (
                    "succeeded",
                    "failed_retry",
                    "failed_dead",
                    "lease_expired",
                ):
                    expression = f'sum(jobforge_job_attempts_total{{type="{task_type}",queue="default",outcome="{outcome}"}})'
                    evidence[f"{task_type}/{outcome}"] = eventually(
                        lambda e=expression: metric(e), expression
                    )
            dashboard = http.get(
                args.grafana_url + "/api/dashboards/uid/jobforge-tasks",
                auth=("admin", os.environ.get("JOBFORGE_GRAFANA_PASSWORD", "jobforge")),
            )
            dashboard.raise_for_status()
            assert len(dashboard.json()["dashboard"]["panels"]) == 11
            print(
                json.dumps(
                    {
                        "metrics": evidence,
                        "alerts": sorted(alerts()),
                        "grafana_panels": 11,
                        "status": "PASS",
                    }
                ),
                flush=True,
            )
        finally:
            # Restore only the explicitly selected disposable project.
            for command in (
                ("unpause", "ollama"),
                ("start", "ollama", "worker-1", "worker-2", "otel-collector"),
            ):
                try:
                    compose(*command)
                except subprocess.CalledProcessError:
                    pass
            provider.shutdown()


if __name__ == "__main__":
    main()
