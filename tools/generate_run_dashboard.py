"""Generate the separate v3 Run dashboard without inventing missing values."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def main() -> None:
    """Write deterministic provisioned panels using bounded Run metrics."""
    datasource = {"type": "prometheus", "uid": "jobforge-run-prometheus"}
    definitions = [
        ("PG snapshot available", "jobforge_run_snapshot_success", "stat", "short"),
        ("Live Workers", "jobforge_run_workers_live", "stat", "short"),
        (
            "Run state",
            "sum by (state) (jobforge_run_current)",
            "timeseries",
            "short",
        ),
        (
            "Pending age (seconds)",
            "max by (state) (jobforge_run_oldest_wait_seconds)",
            "timeseries",
            "s",
        ),
        (
            "Batch known / held (observed usage)",
            'sum by (kind) (jobforge_run_budget_cost_microyuan{scope="batch",kind=~"known|held"}) / 1000000',
            "timeseries",
            "currencyCNY",
        ),
        (
            "Maximum account exposure / limit",
            "max by (scope) (jobforge_run_budget_max_ratio)",
            "timeseries",
            "percentunit",
        ),
        (
            "Frozen accounts",
            "sum by (scope) (jobforge_run_budget_frozen)",
            "timeseries",
            "short",
        ),
        (
            "Physical calls · durable status",
            "sum by (kind,status) (jobforge_run_calls)",
            "timeseries",
            "short",
        ),
        (
            "Step duration p95 · includes cleanup/commit",
            "histogram_quantile(0.95,sum by (le,kind) "
            "(rate(jobforge_run_step_duration_seconds_bucket[$__rate_interval])))",
            "timeseries",
            "s",
        ),
        (
            "Checkpoint commit RPC p95 · includes ACK",
            'histogram_quantile(0.95,sum by (le) (rate(jobforge_run_control_rpc_duration_seconds_bucket{method="CommitStep"}[$__rate_interval])))',
            "timeseries",
            "s",
        ),
        (
            "Public refusals / errors",
            'sum by (result) (rate(jobforge_run_http_requests_total{result!="ok"}[$__rate_interval]))',
            "timeseries",
            "ops",
        ),
        (
            "Persisted recoveries",
            "jobforge_run_recoveries",
            "timeseries",
            "short",
        ),
        (
            "Independent action physical calls",
            "sum by (kind,status) (jobforge_run_action_calls)",
            "timeseries",
            "short",
        ),
        (
            "Active Run alerts",
            'ALERTS{alertstate="firing",alertname=~"JobForgeRun.*"}',
            "table",
            "short",
        ),
    ]
    panels: list[dict[str, Any]] = []
    for index, (title, expression, kind, unit) in enumerate(definitions):
        panels.append(
            {
                "id": index + 1,
                "title": title,
                "type": kind,
                "datasource": datasource,
                "gridPos": {
                    "x": index % 2 * 12,
                    "y": index // 2 * 7,
                    "w": 12,
                    "h": 7,
                },
                "targets": [
                    {
                        "refId": "A",
                        "expr": expression,
                        "legendFormat": "{{state}}{{scope}}{{kind}}{{status}}{{result}}",
                    }
                ],
                "fieldConfig": {"defaults": {"unit": unit}, "overrides": []},
                "options": {
                    "legend": {"displayMode": "list", "placement": "bottom"},
                    "reduceOptions": {"calcs": ["lastNotNull"], "values": False},
                },
                "description": "Missing samples are unknown. PG snapshots are durable; process histograms may miss a crash. Known usage is observed, not an invoice. Held exposure is not zero. Account scopes overlap.",
            }
        )
    dashboard = {
        "uid": "jobforge-runs",
        "title": "JobForge · v3 Runs",
        "schemaVersion": 39,
        "version": 1,
        "refresh": "5s",
        "time": {"from": "now-30m", "to": "now"},
        "tags": ["jobforge", "runs", "agent-v3"],
        "panels": panels,
        "links": [
            {
                "title": "Run traces",
                "type": "link",
                "url": "http://localhost:16687",
                "targetBlank": True,
            },
            {
                "title": "Run alerts",
                "type": "link",
                "url": "http://localhost:9094/alerts",
                "targetBlank": True,
            },
        ],
    }
    path = (
        Path(__file__).resolve().parents[1]
        / "deploy/grafana/run-dashboards/jobforge-runs.json"
    )
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(dashboard, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )


if __name__ == "__main__":
    main()
