"""Shared S4 source vectors, strict SDK views and independent Go hash encoding."""

from __future__ import annotations

import copy
import hashlib
import json
from dataclasses import fields
from pathlib import Path
from typing import Any

import httpx
import pytest
import yaml  # type: ignore[import-untyped]
from jsonschema import Draft202012Validator  # type: ignore[import-untyped]
from referencing import Registry, Resource

from jobforge import RunClient, run_actions
from jobforge.run_models import RunResult

ROOT = Path(__file__).resolve().parents[3]
PUBLIC = json.loads((ROOT / "api/run/v2/approval-fixtures.json").read_bytes())
ACTION = json.loads((ROOT / "api/business-action/v1/fixtures.json").read_bytes())
OPENAPI = yaml.safe_load((ROOT / "api/run/v2/openapi.yaml").read_bytes())
BUSINESS = json.loads((ROOT / "api/business-action/v1/schema.json").read_bytes())
SUPPORT = json.loads((ROOT / "api/support/v1/schema.json").read_bytes())
REGISTRY: Registry[Any] = Registry().with_resource(
    "https://jobforge.local/support/v1/schema.json", Resource.from_contents(SUPPORT)
)


def public_schema(name: str) -> dict[str, Any]:
    """Resolve local OpenAPI response references using its actual relative base."""
    value = json.loads(
        json.dumps(OPENAPI["components"]["schemas"]).replace(
            "#/components/schemas/", "#/$defs/"
        )
    )
    return {
        "$schema": "https://json-schema.org/draft/2020-12/schema",
        "$id": "https://jobforge.local/run/v2/schema.json",
        "$defs": value,
        "$ref": "#/$defs/" + name,
    }


@pytest.mark.parametrize(
    ("fixture", "name"),
    [
        ("approval", "ApprovalView"),
        ("approval_response", "ApprovalResponse"),
        ("effect_none", "EffectView"),
        ("effect_unknown", "EffectView"),
        ("effect_applied", "EffectView"),
        ("action_calls", "ActionCallsResponse"),
        ("receipt", "ActionReceipt"),
    ],
)
def test_shared_public_vectors(fixture: str, name: str) -> None:
    """SDK accepts actual closed source field names, all statuses and observations."""
    Draft202012Validator(public_schema(name), registry=REGISTRY).validate(
        PUBLIC[fixture]
    )
    model = getattr(run_actions, name)
    model.from_dict(PUBLIC[fixture])
    schema = OPENAPI["components"]["schemas"][name]
    assert (
        {item.name for item in fields(model)}
        == set(schema["properties"])
        == set(schema["required"])
    )
    for key in PUBLIC[fixture]:
        changed = copy.deepcopy(PUBLIC[fixture])
        del changed[key]
        with pytest.raises(ValueError):
            model.from_dict(changed)
    with pytest.raises(ValueError):
        model.from_dict({**PUBLIC[fixture], "unexpected": True})


def canonical(value: Any) -> str:
    """Independent Python encoding of Go map JSON, including default escaping."""
    raw = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    for literal, escaped in (
        ("&", r"\u0026"),
        ("<", r"\u003c"),
        (">", r"\u003e"),
        ("\u2028", r"\u2028"),
        ("\u2029", r"\u2029"),
    ):
        raw = raw.replace(literal, escaped)
    return raw


def fingerprint(domain: str, *parts: str) -> str:
    """Lengths count UTF-8 bytes, not Python characters."""
    digest = hashlib.sha256()
    for part in (domain, *parts):
        raw = part.encode("utf-8")
        digest.update(len(raw).to_bytes(8, "big"))
        digest.update(raw)
    return digest.hexdigest()


def test_action_hash_encoding_and_receipt() -> None:
    """Cross-language field order/canonicalization does not depend on Go helpers."""
    action = ACTION["action"]
    a = action["authorization"]
    Draft202012Validator(BUSINESS, registry=REGISTRY).validate(action)
    assert (
        fingerprint(
            "jobforge.business.parameters.v1", "1", canonical(action["parameters"])
        )
        == ACTION["parameters_hash"]
    )
    vector_hash = fingerprint(
        "jobforge.business.version-vector.v1", "1", canonical(a["version_vector"])
    )
    names = (
        "schema_version",
        "key_id",
        "operation",
        "tenant_id",
        "business_request_id",
        "business_request_created_at",
        "operation_id",
        "run_id",
        "approval_id",
        "actor_id",
        "decided_at",
        "proposal_hash",
        "parameters_hash",
        "snapshot_id",
        "snapshot_hash",
        "version_vector",
        "authorized_at",
        "permission_expires_at",
        "authorization_expires_at",
        "run_deadline",
    )
    parts = [
        vector_hash if name == "version_vector" else str(a[name]) for name in names
    ]
    assert (
        fingerprint("jobforge.business.authorization.v1", *parts)
        == ACTION["authorization_hash"]
    )
    receipt = {k: v for k, v in ACTION["receipt"].items() if k != "receipt_hash"}
    assert (
        fingerprint("jobforge.business.receipt.v1", "1", canonical(receipt))
        == ACTION["receipt"]["receipt_hash"]
    )


@pytest.mark.parametrize(
    "change",
    [
        "no_action",
        "empty_action",
        "ticket_zero",
        "ticket_overflow",
        "policy_zero",
        "order_zero",
        "delivery_zero",
    ],
)
def test_action_source_rejects_invalid_shared_vectors(change: str) -> None:
    """The schema rejects these before deployment, as the Go parser also does."""
    value = copy.deepcopy(ACTION["action"])
    vector = value["authorization"]["version_vector"]
    if change == "no_action":
        value["parameters"]["decision"] = "no_action"
    elif change == "empty_action":
        value["parameters"]["action"] = ""
    elif change == "ticket_zero":
        vector["ticket"]["revision"] = 0
    elif change == "ticket_overflow":
        vector["ticket"]["revision"] = (1 << 53) - 1
    elif change == "policy_zero":
        vector["policy"]["revision"] = 0
    elif change == "order_zero":
        vector["order"] = {"id": "order-1", "exists": True, "revision": 0}
    else:
        vector["delivery"] = {
            "id": "delivery-1",
            "exists": True,
            "aggregate_revision": 0,
        }
    assert not Draft202012Validator(BUSINESS, registry=REGISTRY).is_valid(value)


def test_result_dispositions_and_absence() -> None:
    """Unknown/approved are facts different from no result or explicit no_action."""
    for item in PUBLIC["results"]:
        Draft202012Validator(public_schema("RunResult"), registry=REGISTRY).validate(
            item
        )
        RunResult.from_dict(item)
    with pytest.raises(ValueError):
        RunResult.from_dict({**PUBLIC["results"][0], "disposition": "no_action"})


def test_new_methods_one_exchange_and_closed_decision() -> None:
    """No new method polls, retries, follows redirects or sends an actor."""
    requests: list[httpx.Request] = []
    responses = iter(
        [
            PUBLIC["approval"],
            PUBLIC["approval_response"],
            PUBLIC["effect_applied"],
            PUBLIC["effect_applied"],
            PUBLIC["action_calls"],
        ]
    )

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json=next(responses))

    uid = PUBLIC["approval"]["run_id"]
    with RunClient(
        "https://unit.invalid", "fixture", transport=httpx.MockTransport(handler)
    ) as client:
        client.approval(uid)
        client.decide_approval(
            uid,
            "approve",
            PUBLIC["decision"]["proposal_hash"],
            idempotency_key="decision-1",
        )
        client.effect(uid)
        client.reconcile(uid)
        client.action_calls(uid)
    assert len(requests) == 5
    assert json.loads(requests[1].content) == PUBLIC["decision"]
    assert "Idempotency-Key" not in requests[3].headers
