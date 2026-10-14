"""Complete-bundle prerequisite, final isolation and single-call accounting."""

from __future__ import annotations

import asyncio
import copy
import json
from pathlib import Path

import pytest
from http_fault_server import HTTPFaultServer, ReceivedRequest, respond
from jobforge_agent import outbound_audit
from jobforge_agent.business import POLICY_BUNDLE_ALIASES
from jobforge_agent.dispatch import AuthorizedDispatcher, DispatchError, Endpoint
from jobforge_agent.runtime_input import (
    ADJUDICATION_V5_EXECUTOR_VERSION,
    FIXED_BUNDLE_EXECUTOR_VERSION,
    RuntimeInputError,
    parse_runtime_input,
)
from jobforge_agent.step import execute_registered_step
from jobforge_agent.support_agent import validate_agent_step
from jobforge_agent.support_agent_v8 import (
    FINAL_SYSTEM,
    SupportAgentV8Adapter,
    SupportFixedV2Adapter,
    validate_fixed_bundle_step,
)
from jobforge_agent.support_contract import support_sources
from test_dispatch import Clock, run_async
from test_run_tools import SyntheticHooks
from test_support_adapter import support_frame
from test_support_agent import FIXTURE
from test_support_agent_v5 import body_of, final_checkpoint
from test_support_agent_v6 import INTENT


def bundle_checkpoint(*, fixed: bool = False) -> dict:
    """Insert a complete synthetic bundle, preserving the original policy text."""
    checkpoint = final_checkpoint()
    search = next(
        s for s in checkpoint["steps"] if s["step"]["kind"] == "search_policy"
    )
    bundle = copy.deepcopy(search)
    bundle["step"]["kind"] = "read_policy_bundle"
    original = {
        hit["chunk_id"]: hit for hit in search["result_json"]["content"]["matches"]
    }
    template = next(iter(original.values()))
    hits = []
    for alias in POLICY_BUNDLE_ALIASES:
        hit = copy.deepcopy(original.get(alias, template))
        hit.pop("distance", None)
        hit["chunk_id"] = alias
        hit["evidence_ref"] = (
            f"business-policy:{checkpoint['snapshot']['index_id']}:{alias}"
        )
        hits.append(hit)
    bundle["result_json"]["content"]["matches"] = hits
    bundle["result_json"]["evidence_refs"] = [hit["evidence_ref"] for hit in hits]
    checkpoint["steps"] = [
        item
        for item in checkpoint["steps"]
        if item["step"]["kind"] != "search_policy"
        and (item["result_json"]["content"] or {}).get("name") != "search_policy"
        and (
            not fixed
            or item["step"]["kind"] not in {"model_decision", "protocol_correction"}
        )
    ]
    checkpoint["steps"].insert(1, bundle)
    return checkpoint


@pytest.mark.parametrize("fixed", [False, True])
def test_complete_bundle_final_projection_and_closed_envelope(fixed: bool) -> None:
    """Both strategies see all paragraphs and exactly one final envelope."""
    checkpoint = bundle_checkpoint(fixed=fixed)
    before = copy.deepcopy(checkpoint)
    if fixed:
        validate_fixed_bundle_step(checkpoint, "model_proposal")
        adapter = SupportFixedV2Adapter()
    else:
        validate_agent_step(
            checkpoint, "model_proposal", adjudication=True, policy_bundle=True
        )
        adapter = SupportAgentV8Adapter()
    messages = adapter.proposal_messages(checkpoint, correction=False)
    facts = body_of(messages)
    assert messages[0]["content"] == FINAL_SYSTEM
    assert [hit["chunk_id"] for hit in facts["policies"]] == list(POLICY_BUNDLE_ALIASES)
    assert all(alias in facts["available_refs"] for alias in POLICY_BUNDLE_ALIASES)
    for forbidden in (
        "UNRETRIEVED_DRAFT_ONLY",
        "investigation_complete",
        "previous_tools",
        "remaining_tools",
        "policy_retrieval",
        "policy_supplement",
        "1024 output tokens",
    ):
        assert forbidden not in json.dumps(messages)
    assert checkpoint == before
    proposal = json.loads(FIXTURE["valid"][0]["model_json"])
    adapter.validate_proposal({"type": "final", "proposal": proposal}, checkpoint)
    for value in (
        proposal,
        INTENT,
        {"type": "tool", "name": "read_policy_bundle", "arguments": {}},
    ):
        with pytest.raises(DispatchError):
            adapter.validate_proposal(value, checkpoint)


@pytest.mark.parametrize(
    "mutation",
    [
        "missing",
        "extra",
        "duplicate",
        "reorder",
        "alias",
        "snapshot",
        "index",
        "version",
        "ref",
        "empty",
        "oversize",
        "distance",
        "source",
    ],
)
def test_invalid_bundle_fails_before_model(mutation: str) -> None:
    """Incomplete or cross-bound sources cannot reach model projection."""
    checkpoint = bundle_checkpoint()
    result = checkpoint["steps"][1]["result_json"]
    content = result["content"]
    hits = content["matches"]
    if mutation == "missing":
        hits.pop()
    elif mutation == "extra":
        hits.append(copy.deepcopy(hits[0]))
    elif mutation == "duplicate":
        hits[1] = copy.deepcopy(hits[0])
    elif mutation == "reorder":
        hits.reverse()
    elif mutation == "alias":
        hits[0]["chunk_id"] = "P11.1"
    elif mutation == "snapshot":
        content["snapshot_id"] = "11111111-1111-4111-8111-111111111111"
    else:
        field, value = {
            "index": ("index_id", "99999999-1111-4111-8111-111111111111"),
            "version": ("policy_version", "wrong-policy"),
            "ref": ("evidence_ref", "wrong-ref"),
            "empty": ("text", " "),
            "oversize": ("text", "a" * 769),
            "distance": ("distance", None),
            "source": ("source", ""),
        }[mutation]
        hits[0][field] = value
    result["evidence_refs"] = [hit["evidence_ref"] for hit in hits]
    with pytest.raises(DispatchError):
        validate_agent_step(
            checkpoint, "model_proposal", adjudication=True, policy_bundle=True
        )
    with pytest.raises(DispatchError):
        SupportAgentV8Adapter().proposal_messages(checkpoint, correction=False)


def test_order_version_isolation_and_source_conflicts() -> None:
    """Old runtimes reject bundle evidence and search cannot replace it."""
    checkpoint = bundle_checkpoint()
    with pytest.raises(DispatchError):
        validate_agent_step(checkpoint, "model_proposal", adjudication=True)
    frame = support_frame(copy.deepcopy(checkpoint), "model_proposal")
    with pytest.raises(RuntimeInputError):
        parse_runtime_input(frame)
    missing = copy.deepcopy(checkpoint)
    missing["steps"].pop(1)
    with pytest.raises(DispatchError):
        validate_agent_step(
            missing, "model_proposal", adjudication=True, policy_bundle=True
        )
    search = copy.deepcopy(checkpoint["steps"][1])
    search["step"]["kind"] = "search_policy"
    search["result_json"]["content"]["matches"] = search["result_json"]["content"][
        "matches"
    ][:1]
    search["result_json"]["evidence_refs"] = search["result_json"]["evidence_refs"][:1]
    search["result_json"]["content"]["matches"][0]["distance"] = 0.1
    checkpoint["steps"].insert(-1, search)
    support_sources(checkpoint, repeated_search=True)
    for field in ("text", "source"):
        changed = copy.deepcopy(checkpoint)
        changed["steps"][-2]["result_json"]["content"]["matches"][0][field] += (
            " changed"
        )
        with pytest.raises(DispatchError):
            support_sources(changed, repeated_search=True)


@pytest.mark.parametrize("fixed", [False, True])
@run_async
async def test_registered_bundle_is_one_business_call(
    fixed: bool, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The installed step emits index-bound audit evidence for one real HTTP read."""
    monkeypatch.setattr(outbound_audit, "_DIRECTORY", tmp_path)
    checkpoint = bundle_checkpoint(fixed=fixed)
    expected = copy.deepcopy(checkpoint["steps"][1]["result_json"]["content"])
    # A real complete envelope is allowed past the old 8 KiB tool boundary.
    for hit in expected["matches"]:
        hit["text"] = "a" * 700
    checkpoint["steps"] = checkpoint["steps"][:1]
    frame = support_frame(checkpoint, "read_policy_bundle")
    frame["input"].update(
        adapter_id="support-fixed-v1" if fixed else "support-agent-v1",
        executor_version=FIXED_BUNDLE_EXECUTOR_VERSION
        if fixed
        else ADJUDICATION_V5_EXECUTOR_VERSION,
        provider_audit_policy="deepseek-audit-v2",
        tool_invocation_id="00000000-0000-4000-8000-000000000099",
    )
    clock = Clock()
    frame["emitted_mono_ms"] = clock.now
    hooks = SyntheticHooks(frame)

    async def handle(
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
        request: ReceivedRequest,
    ) -> None:
        assert request.method == "GET"
        assert request.path.endswith("/policies/bundle") and "?" not in request.path
        assert request.body == b""
        await respond(writer, body=json.dumps(expected).encode())

    async with HTTPFaultServer(handle) as server:
        dispatcher = AuthorizedDispatcher(
            frame,
            hooks=hooks,
            endpoints={"business": Endpoint(server.origin, "fixture")},
            clock=clock,
            provider_audit_policy="deepseek-audit-v2",
        )
        try:
            result = await execute_registered_step(
                frame,
                parse_runtime_input(frame),
                SupportFixedV2Adapter() if fixed else SupportAgentV8Adapter(),
                dispatcher,
            )
        finally:
            await dispatcher.aclose()
        assert len(server.requests) == 1
    assert result["result"]["content"] == expected
    assert len(result["result"]["evidence_refs"]) == 20
    assert [f["subcall"] for f in hooks.intents] == ["read_policy_bundle"]
    paths = list(tmp_path.glob("*.jsonl"))
    assert len(paths) == 1
    records = [json.loads(line) for line in paths[0].read_bytes().splitlines()]
    assert [record["event"] for record in records] == [
        "dispatch_attempt",
        "http_response",
        "finish",
    ]
    assert all(
        record["resource_id"] == checkpoint["snapshot"]["index_id"]
        for record in records
    )
    assert all(
        record["snapshot_id"] == frame["binding"]["snapshot_id"] for record in records
    )
    assert records[-1]["response_complete"]
