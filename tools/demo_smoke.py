"""Verify a labelled free demo over public HTTP; never target a paid deployment."""

from __future__ import annotations

import argparse
import json
import time
from typing import Any
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import Request, urlopen
from uuid import uuid4


def request(
    base: str, path: str, token: str = "", body: Any = None, key: str = ""
) -> tuple[int, Any]:
    """Send one bounded exchange without mutation retries."""
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if key:
        headers["Idempotency-Key"] = key
    req = Request(
        base + path,
        headers=headers,
        data=None if body is None else json.dumps(body).encode(),
    )
    try:
        with urlopen(req, timeout=10) as response:
            return response.status, json.load(response)
    except HTTPError as error:
        return error.code, json.load(error)


def main() -> None:
    """Exercise identity, stable submission, approval binding and durable effects."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:8195")
    args = parser.parse_args()
    url = urlsplit(args.url)
    if (
        url.scheme != "http"
        or url.hostname not in ("127.0.0.1", "localhost")
        or url.path not in ("", "/")
    ):
        parser.error("the smoke target must be an explicit loopback demo")
    base = args.url.rstrip("/")
    code, metadata = request(base, "/demo.json")
    assert (
        code == 200
        and metadata["source_kind"] == "synthetic_mechanism_demo"
        and metadata["cloud_calls"] == 0
    )
    assert request(base, "/v2/identity")[0] == 401
    key = "smoke-" + uuid4().hex
    body = {
        "schema_version": 1,
        "ticket_id": metadata["ticket_id"],
        "business_request_key": key,
        "profile_id": metadata["profile_id"],
        "budget_batch_id": metadata["budget_batch_id"],
    }
    assert request(base, "/v2/runs", "demo-reader", body, key)[0] == 403
    start = time.monotonic()
    code, submitted = request(base, "/v2/runs", "demo-operator", body, key)
    assert code == 201 and not submitted["reused"], (code, submitted.get("error"))
    run_id = submitted["run"]["run_id"]
    code, replay = request(base, "/v2/runs", "demo-operator", body, key)
    assert code == 200 and replay["reused"] and replay["run"]["run_id"] == run_id
    path = "/v2/runs/" + run_id

    def wait_state(expected: str) -> None:
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            status, run = request(base, path, "demo-reader")
            assert status == 200
            if run["state"] == expected:
                return
            assert run["state"] not in ("failed", "cancelled"), run.get("error")
            time.sleep(0.25)
        raise AssertionError("demo did not reach " + expected)

    wait_state("awaiting_approval")
    proposal_seconds = time.monotonic() - start
    code, approval = request(base, path + "/approval", "demo-reader")
    assert code == 200 and approval["available"]
    decision = {
        "schema_version": 1,
        "decision": "approve",
        "proposal_hash": approval["proposal_hash"],
    }
    assert (
        request(
            base, path + "/approval", "demo-operator", decision, key + "-forbidden"
        )[0]
        == 403
    )
    wrong = dict(decision, proposal_hash="0" * 64)
    assert (
        request(base, path + "/approval", "demo-approver", wrong, key + "-wrong")[0]
        == 409
    )
    assert (
        request(base, path + "/approval", "demo-approver", decision, key + "-approval")[
            0
        ]
        == 200
    )
    assert (
        request(base, path + "/approval", "demo-approver", decision, key + "-approval")[
            0
        ]
        == 200
    )
    wait_state("succeeded")
    code, effect = request(base, path + "/effect", "demo-reader")
    assert code == 200 and effect["state"] == "applied"
    receipt = effect["receipt"]
    assert receipt["after_revision"] == receipt["before_revision"] + 1
    code, checked = request(
        base, path + "/reconcile", "demo-operator", {"schema_version": 1}
    )
    assert code == 200 and checked["receipt"] == receipt
    print(
        json.dumps(
            {
                "cloud_calls": 0,
                "model": "synthetic",
                "run_id": run_id,
                "submit_to_proposal_seconds": round(proposal_seconds, 3),
                "total_seconds": round(time.monotonic() - start, 3),
                "receipt_hash": receipt["receipt_hash"],
                "checks": [
                    "401",
                    "reader_403",
                    "submit_replay",
                    "operator_approval_403",
                    "wrong_hash_409",
                    "approval_replay",
                    "real_receipt",
                    "reconcile_same_receipt",
                ],
            }
        )
    )


if __name__ == "__main__":
    main()
