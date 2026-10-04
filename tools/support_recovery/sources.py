"""Generate a new schema 3 source template without rewriting historical profiles."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
from typing import Any

from tools.support_evaluation.evidence import fingerprint
from tools.support_evaluation.export import json_bytes


def template(repo: Path, observed_on: str, price_sha256: str) -> dict[str, Any]:
    """Bind the current installed-source inputs; receipts remain explicitly absent."""
    source = json.loads(
        (repo / "deploy/support-agent.source.example.json").read_bytes()
    )
    definition = source["definition"]
    definition["schema_version"] = 3
    definition["program"]["recovery_policy"] = "confirmed_uncommitted_v1"
    definition["model"]["observed_on"] = definition["price"]["observed_on"] = (
        observed_on
    )
    definition["price"]["source_sha256"] = price_sha256
    source["price_snapshot"]["sha256"] = price_sha256
    parts: list[str] = []
    for path in sorted((repo / "python/jobforge_agent").glob("*.py")):
        parts.extend(
            (
                "python/jobforge_agent/" + path.name,
                hashlib.sha256(path.read_bytes()).hexdigest(),
            )
        )
    definition["program"]["adapter_source_sha256"] = fingerprint(
        "jobforge.support.adapter-source.v1", *parts
    )
    for key, relative in (
        ("prompt_sha256", "python/jobforge_agent/support_agent.py"),
        ("decision_schema_sha256", "api/support/agent-v1/schema.json"),
        ("proposal_schema_sha256", "api/support/v1/schema.json"),
    ):
        definition["program"][key] = hashlib.sha256(
            (repo / relative).read_bytes()
        ).hexdigest()
    return source


def main() -> None:
    """Use an actual reviewed price snapshot, but never claim build/data/scoring review."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, required=True)
    parser.add_argument("--observed-on", required=True)
    parser.add_argument("--price-snapshot", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    value = template(
        args.repo,
        args.observed_on,
        hashlib.sha256(args.price_snapshot.read_bytes()).hexdigest(),
    )
    with args.out.open("xb") as output:
        output.write(json_bytes(value))


if __name__ == "__main__":
    main()
