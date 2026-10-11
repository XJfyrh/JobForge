"""Read old S5 ledgers and official metadata; never call a chat endpoint."""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
from datetime import UTC, datetime
from html.parser import HTMLParser
from pathlib import Path
from typing import Any

import httpx

from tools.support_diagnosis.prepare import digest, read, save


class VisibleText(HTMLParser):
    """Collect text for a human-readable review of the frozen official tariff."""

    def __init__(self) -> None:
        """Initialize the visible text buffer."""
        super().__init__()
        self.parts: list[str] = []

    def handle_data(self, data: str) -> None:
        """Keep nonempty text without interpreting webpage instructions."""
        if data.strip():
            self.parts.append(data.strip())


def old_ledgers(root: Path) -> dict[str, Any]:
    """Refresh every original batch once, without reading secrets or resetting data."""
    old = read(root / "final-20261008/cumulative-stage-ledger.json")
    plans = [{"phase": p["phase"], "database": p["database"]} for p in old["phases"]]
    for path in sorted(root.glob("*-v3-20261008/identities.json")):
        plans.append(
            {"phase": path.parent.name, "database": read(path)["control_database"]}
        )
    if len({p["database"] for p in plans}) != len(plans) or len(plans) != 20:
        raise ValueError("unexpected prior database inventory")
    query = """
begin transaction isolation level repeatable read read only;
select json_build_object('database',current_database(),'sampled_at',clock_timestamp(),
 'runs',(select count(*) from runs),
 'chat_calls',(select count(*) from physical_calls where kind='chat'),
 'unknown_chat_calls',(select count(*) from physical_calls where kind='chat' and status='unknown'),
 'inflight',(select count(*) from physical_calls where status='reserved'),
 'unfinished_attempts',(select count(*) from run_attempts where finished_at is null),
 'known',(select coalesce(sum(known_cost_microyuan),0) from budget_accounts where scope='batch'),
 'held',(select coalesce(sum(held_cost_microyuan),0) from budget_accounts where scope='batch'));
commit;
"""
    phases = []
    for row in plans:
        clean = row["database"] == "jobforge_agent"
        args = [
            "docker",
            "exec",
            "-i",
            "jobforge-s5-clean-demo-control-postgres-1"
            if clean
            else "deploy-postgres-1",
            "psql",
            "-X",
            "-q",
            "-At",
            "-v",
            "ON_ERROR_STOP=1",
            "-U",
            "jobforge_agent" if clean else "jobforge",
            "-d",
            row["database"],
        ]
        result = subprocess.run(
            args,
            input=query,
            text=True,
            encoding="utf-8",
            capture_output=True,
            check=True,
            timeout=30,
        )
        phases.append({**row, **json.loads(result.stdout)})
    keys = (
        "runs",
        "chat_calls",
        "unknown_chat_calls",
        "inflight",
        "unfinished_attempts",
        "known",
        "held",
    )
    return {
        "recorded_at": datetime.now(UTC).isoformat(),
        "phases": phases,
        "totals": {k: sum(p[k] for p in phases) for k in keys},
    }


def collect(root: Path, output: Path) -> dict[str, Any]:
    """Save current source bytes before interpreting their pricing or capabilities."""
    output.mkdir(exist_ok=False)
    ledger = old_ledgers(root)
    save(output / "prior-ledger.json", ledger)
    containers = subprocess.run(
        ["docker", "ps", "--format", "{{.Names}}"],
        text=True,
        capture_output=True,
        check=True,
        timeout=30,
    ).stdout.splitlines()
    active = [
        name
        for name in containers
        if name.startswith("jobforge-s5-")
        and ("worker" in name or name.endswith(("-control", "-paid")))
    ]
    if active:
        raise ValueError("old paid process still running")
    totals = ledger["totals"]
    if any(
        totals[k]
        for k in ("inflight", "unknown_chat_calls", "unfinished_attempts", "held")
    ):
        raise ValueError("old ledger has unresolved exposure")
    credentials = read(root / "dev-agent-narrow/secrets/worker.json")
    key = credentials["tenants"]["tenant-north"]["deepseek_api_key"]
    if key != credentials["tenants"]["tenant-south"]["deepseek_api_key"]:
        raise ValueError("provider identity mismatch")
    artifacts = {}
    with httpx.Client(timeout=60, follow_redirects=False) as client:
        for name, url, authenticated in (
            (
                "pricing.html",
                "https://api-docs.deepseek.com/zh-cn/quick_start/pricing/",
                False,
            ),
            ("models.json", "https://api.deepseek.com/models", True),
            ("balance.json", "https://api.deepseek.com/user/balance", True),
        ):
            response = client.get(
                url, headers={"Authorization": "Bearer " + key} if authenticated else {}
            )
            response.raise_for_status()
            with (output / name).open("xb") as stream:
                stream.write(response.content)
            artifacts[name] = digest(response.content)
    parser = VisibleText()
    parser.feed((output / "pricing.html").read_text(encoding="utf-8"))
    with (output / "pricing-visible.txt").open("x", encoding="utf-8") as stream:
        stream.write("\n".join(parser.parts))
    model = next(
        m for m in read(output / "models.json")["data"] if m["id"] == "deepseek-flash"
    )
    available = read(output / "balance.json")["is_available"]
    save(
        output / "metadata.json",
        {
            "recorded_at": datetime.now(UTC).isoformat(),
            "model": model,
            "account_available": available,
            "artifacts": artifacts,
            "old_paid_containers": active,
            "c_free_bytes": shutil.disk_usage("C:/").free,
            "paid_chat_calls": 0,
        },
    )
    return {
        "model": model,
        "account_available": available,
        "prior_totals": totals,
        "pricing_sha256": artifacts["pricing.html"],
    }


def main() -> None:
    """Collect metadata, or record the independently reviewed official tariff."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--confirm-tariff", action="store_true")
    args = parser.parse_args()
    if args.confirm_tariff:
        meta = read(args.output / "metadata.json")
        if not meta["account_available"] or meta["old_paid_containers"]:
            raise ValueError("provider or sender not ready")
        save(
            args.output / "receipt.json",
            {
                "ready": True,
                "pricing_sha256": meta["artifacts"]["pricing.html"],
                "reviewed_tariff": {
                    "miss": 2000000,
                    "hit": 40000,
                    "output": 8000000,
                    "denominator": 1000000,
                },
                "recorded_at": datetime.now(UTC).isoformat(),
                "paid_chat_calls": 0,
            },
        )
        print('{"ready":true,"paid_chat_calls":0}')
    else:
        print(json.dumps(collect(args.root, args.output)))


if __name__ == "__main__":
    main()
