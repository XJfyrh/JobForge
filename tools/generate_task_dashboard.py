"""Generate the version-controlled Grafana task dashboard (no live data)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def main() -> None:
    """Write an importable Grafana dashboard with bounded task dimensions."""
    datasource = {"type": "prometheus", "uid": "jobforge-prometheus"}
    select = 'queue=~"$queue",type=~"$type"'
    definitions = [
        (
            "Pending jobs",
            'sum(jobforge_queue_depth{queue=~"$queue"}) or vector(0)',
            "stat",
            "short",
        ),
        (
            "Live workers",
            "sum(jobforge_workers_active) or vector(0)",
            "stat",
            "short",
        ),
        (
            "Attempts · process total",
            f"sum(jobforge_job_attempts_total{{{select}}}) or vector(0)",
            "stat",
            "short",
        ),
        (
            "Dead · process total",
            f"sum(jobforge_dlq_total{{{select}}}) or vector(0)",
            "stat",
            "short",
        ),
        (
            "Queue backlog by state",
            'sum by (queue,state) (jobforge_queue_depth{queue=~"$queue"})',
            "timeseries",
            "short",
        ),
        (
            "Attempt outcomes by task",
            "sum by (type,outcome) "
            f"(rate(jobforge_job_attempts_total{{{select}}}[$__rate_interval]))",
            "timeseries",
            "ops",
        ),
        (
            "Worker execution duration p95",
            "histogram_quantile(0.95,sum by (le,type) "
            f"(rate(jobforge_job_latency_seconds_bucket{{{select}}}[$__rate_interval])))",
            "timeseries",
            "s",
        ),
        (
            "Automatic retries (5m)",
            "sum by (type,error_code) "
            f"((increase(jobforge_retries_total{{{select}}}[5m]) > 0) or "
            f"(jobforge_retries_total{{{select}}} unless jobforge_retries_total{{{select}}} offset 5m) or "
            f"(0 * jobforge_retries_total{{{select}}}))",
            "timeseries",
            "short",
        ),
        (
            "Lease recoveries (5m)",
            "sum by (type,resolution) "
            f"((increase(jobforge_lease_expired_total{{{select}}}[5m]) > 0) or "
            f"(jobforge_lease_expired_total{{{select}}} unless jobforge_lease_expired_total{{{select}}} offset 5m) or "
            f"(0 * jobforge_lease_expired_total{{{select}}}))",
            "timeseries",
            "short",
        ),
        (
            "Service health / alert state",
            'up{job=~"jobforge-.+"}',
            "timeseries",
            "short",
        ),
    ]
    panels: list[dict[str, Any]] = []
    for index, (title, expression, kind, unit) in enumerate(definitions):
        grid = (
            {"x": index * 6, "y": 0, "w": 6, "h": 4}
            if index < 4
            else {
                "x": ((index - 4) % 2) * 12,
                "y": 4 + ((index - 4) // 2) * 8,
                "w": 12,
                "h": 8,
            }
        )
        panels.append(
            {
                "id": index + 1,
                "title": title,
                "type": kind,
                "datasource": datasource,
                "gridPos": grid,
                "targets": [
                    {
                        "refId": "A",
                        "expr": expression,
                        "instant": kind == "stat",
                        "legendFormat": "{{type}} {{outcome}} {{state}} {{error_code}} {{resolution}} {{job}}",
                    }
                ],
                "fieldConfig": {"defaults": {"unit": unit}, "overrides": []},
                "options": {
                    "legend": {"displayMode": "list", "placement": "bottom"},
                    "reduceOptions": {"calcs": ["lastNotNull"], "values": False},
                },
                "description": "Process metrics can miss a commit during a crash. "
                "Use PostgreSQL attempt records for audit. Worker duration excludes "
                "queue waiting and lease recovery without a reported duration.",
            }
        )
    panels.append(
        {
            "id": 11,
            "title": "Active alerts",
            "type": "table",
            "datasource": datasource,
            "gridPos": {"x": 0, "y": 28, "w": 24, "h": 6},
            "targets": [
                {
                    "refId": "A",
                    "expr": 'ALERTS{alertstate="firing",alertname=~"JobForge.*"}',
                    "instant": True,
                    "format": "table",
                }
            ],
        }
    )
    dashboard = {
        "uid": "jobforge-tasks",
        "title": "JobForge · Agent / RAG execution",
        "schemaVersion": 39,
        "version": 1,
        "refresh": "5s",
        "time": {"from": "now-30m", "to": "now"},
        "tags": ["jobforge", "agent", "rag"],
        "panels": panels,
        "templating": {
            "list": [
                {
                    "name": "queue",
                    "label": "Queue",
                    "type": "query",
                    "datasource": datasource,
                    "query": "label_values(jobforge_jobs_submitted_total,queue)",
                    "includeAll": True,
                    "allValue": ".*",
                    "current": {"text": "All", "value": "$__all"},
                    "refresh": 1,
                },
                {
                    "name": "type",
                    "label": "Task",
                    "type": "custom",
                    "query": "rag.index,agent.extract",
                    "includeAll": True,
                    "allValue": "rag.index|agent.extract",
                    "current": {"text": "All", "value": "$__all"},
                },
                {
                    "name": "trace_id",
                    "label": "Trace ID",
                    "type": "textbox",
                    "current": {"text": "", "value": ""},
                },
            ]
        },
        "links": [
            {
                "title": "Open task trace",
                "type": "link",
                "url": "http://localhost:16686/trace/${trace_id}",
                "targetBlank": True,
            },
            {
                "title": "Prometheus alerts",
                "type": "link",
                "url": "http://localhost:9091/alerts",
                "targetBlank": True,
            },
        ],
    }
    target = (
        Path(__file__).resolve().parents[1]
        / "deploy/grafana/dashboards/jobforge-tasks.json"
    )
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(dashboard, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
