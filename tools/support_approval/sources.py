"""Prepare a fresh schema 4 source; historical profile templates stay immutable."""

from __future__ import annotations

import argparse
import hashlib
import re
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from tools.support_evaluation.export import json_bytes
from tools.support_recovery.sources import template as recovery_template


def template(
    repo: Path,
    observed_on: str,
    price_sha256: str,
    origin: str,
    key_id: str,
    public_key_sha256: str,
) -> dict[str, Any]:
    """Bind current sources and public action identity, leaving review receipts absent."""
    parts = urlsplit(origin)
    if (
        parts.scheme not in {"http", "https"}
        or not parts.hostname
        or parts.username
        or parts.password
        or parts.path
        or parts.query
        or parts.fragment
        or len(origin) > 256
        or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", key_id) is None
        or re.fullmatch(r"[0-9a-f]{64}", public_key_sha256) is None
    ):
        raise ValueError("invalid action public identity")
    source = recovery_template(repo, observed_on, price_sha256)
    definition = source["definition"]
    definition["schema_version"] = 4
    definition["program"]["approval_policy"] = "ticket_resolution_v1"
    definition["action"] = {
        "operation": "apply_ticket_resolution",
        "origin": origin,
        "key_id": key_id,
        "public_key_sha256": public_key_sha256,
    }
    return source


def main() -> None:
    """Use a verified price snapshot and deployment public identity; never private keys."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, required=True)
    parser.add_argument("--observed-on", required=True)
    parser.add_argument("--price-snapshot", type=Path, required=True)
    parser.add_argument("--action-origin", required=True)
    parser.add_argument("--action-key-id", required=True)
    parser.add_argument("--action-public-key-sha256", required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    value = template(
        args.repo,
        args.observed_on,
        hashlib.sha256(args.price_snapshot.read_bytes()).hexdigest(),
        args.action_origin,
        args.action_key_id,
        args.action_public_key_sha256,
    )
    with args.out.open("xb") as output:
        output.write(json_bytes(value))


if __name__ == "__main__":
    main()
