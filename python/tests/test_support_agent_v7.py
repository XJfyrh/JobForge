"""Status reads are prerequisites, never answers or extra model retries."""

from __future__ import annotations

import copy
import json

import httpx
import pytest
from jobforge_agent.dispatch import AuthorizedDispatcher, DispatchError, Endpoint
from jobforge_agent.policy_supplement import supplement_tool
from jobforge_agent.runtime_input import (
    ADJUDICATION_V4_EXECUTOR_VERSION,
    parse_runtime_input,
)
from jobforge_agent.status_policy import STATUS_POLICY_ALIASES, missing_status_policy
from jobforge_agent.step import execute_registered_step
from jobforge_agent.support_agent import (
    POLICY_TOPICS,
    next_tool_arguments,
    validate_agent_step,
)
from jobforge_agent.support_agent_v5 import FINAL_SYSTEM
from jobforge_agent.support_agent_v6 import SupportAgentV6Adapter
from jobforge_agent.support_agent_v7 import SupportAgentV7Adapter
from test_dispatch import Clock, run_async
from test_run_tools import SyntheticHooks, _payload
from test_support_adapter import support_frame
from test_support_agent import FIXTURE, decision_step
from test_support_agent_v5 import body_of, final_checkpoint
from test_support_agent_v6 import INTENT
from test_support_contract import ROOT

SHARED = json.loads(
    (ROOT / "api/support/s5-agent-v7/status-policy-fixtures.json").read_bytes()
)


def status_checkpoint(status: str = "informational_only") -> dict:
    """Project a status fact with a deliberately contradictory audit-only draft."""
    checkpoint = final_checkpoint()
    checkpoint["snapshot"]["ticket_binding_json"]["status"] = status
    checkpoint["steps"][0]["result_json"]["content"]["status"] = status
    return checkpoint


def policy_read(checkpoint: dict, alias: str) -> dict:
    """Create a source-bound synthetic paragraph for protocol tests only."""
    read = copy.deepcopy(
        next(s for s in checkpoint["steps"] if s["step"]["kind"] == "search_policy")
    )
    hit = read["result_json"]["content"]["matches"][0]
    hit.update(chunk_id=alias, source=alias[:3] + ".md", text="Synthetic status policy")
    hit["evidence_ref"] = hit["evidence_ref"][:-5] + alias
    read["result_json"]["content"]["matches"] = [hit]
    read["result_json"]["evidence_refs"] = [hit["evidence_ref"]]
    return read


def validate(checkpoint: dict, kind: str) -> None:
    """Exercise the complete accepted sequence, including both read edges."""
    validate_agent_step(
        checkpoint, kind, adjudication=True, supplement=True, status_policy=True
    )


@pytest.mark.parametrize(
    "status", ["open", "awaiting_information", "informational_only", "escalated"]
)
def test_shared_status_routes_and_old_version_isolation(status: str) -> None:
    """Only missing special-state paragraphs add a read; old prompts stay exact."""
    assert SHARED["status_aliases"] == STATUS_POLICY_ALIASES
    assert SHARED["query"] == POLICY_TOPICS["ticket_status"][1]
    checkpoint = status_checkpoint(status)
    prior = copy.deepcopy(checkpoint)
    old = SupportAgentV6Adapter().proposal_messages(checkpoint, correction=False)
    adapter = SupportAgentV7Adapter()
    if status not in STATUS_POLICY_ALIASES:
        validate(checkpoint, "model_proposal")
        assert adapter.proposal_messages(checkpoint, correction=False) == old
        assert missing_status_policy(checkpoint) is None
    else:
        validate(checkpoint, "search_policy")
        with pytest.raises(DispatchError):
            validate(checkpoint, "model_proposal")
        with pytest.raises(DispatchError):
            adapter.proposal_messages(checkpoint, correction=False)
        assert next_tool_arguments(
            checkpoint, "search_policy", supplement=True, status_policy=True
        ) == {"query": SHARED["query"]}
        with pytest.raises(DispatchError):
            next_tool_arguments(
                checkpoint, "get_order", supplement=True, status_policy=True
            )
        # Earlier retrieval needs no extra read or change to the final task.
        existing = copy.deepcopy(checkpoint)
        search = next(
            s for s in existing["steps"] if s["step"]["kind"] == "search_policy"
        )
        added = policy_read(checkpoint, STATUS_POLICY_ALIASES[status])["result_json"]
        search["result_json"]["content"]["matches"].extend(added["content"]["matches"])
        search["result_json"]["evidence_refs"].extend(added["evidence_refs"])
        validate(existing, "model_proposal")
        assert adapter.proposal_messages(
            existing, correction=False
        ) == SupportAgentV6Adapter().proposal_messages(existing, correction=False)
    assert checkpoint == prior


@pytest.mark.parametrize("status", ["informational_only", "escalated"])
def test_prerequisite_and_optional_supplement_have_separate_boundaries(
    status: str,
) -> None:
    """No pseudo model intent is inserted; first thinking may still supplement."""
    checkpoint = status_checkpoint(status)
    checkpoint["steps"].append(policy_read(checkpoint, STATUS_POLICY_ALIASES[status]))
    before = copy.deepcopy(checkpoint)
    validate(checkpoint, "model_proposal")
    adapter = SupportAgentV7Adapter()
    messages = adapter.proposal_messages(checkpoint, correction=False)
    facts = body_of(messages)
    assert STATUS_POLICY_ALIASES[status] in facts["available_refs"]
    assert "ticket_status" not in facts["policy_supplement"]
    assert "missing_facts" in facts["policy_supplement"]
    for forbidden in (
        "UNRETRIEVED_DRAFT_ONLY",
        "investigation_complete",
        "previous_tools",
        "completed_requests",
        "status_prerequisite",
    ):
        assert forbidden not in json.dumps(messages)
    assert checkpoint == before
    with pytest.raises(DispatchError):
        adapter.validate_proposal(
            {
                "type": "policy_supplement",
                "topic": "ticket_status",
                "required_aliases": ["P09.1"],
            },
            checkpoint,
        )
    assert adapter.validate_proposal(INTENT, checkpoint) == INTENT
    checkpoint["steps"].append(decision_step(INTENT, "model_proposal"))
    validate(checkpoint, "search_policy")
    checkpoint["steps"].append(policy_read(checkpoint, "P03.2"))
    validate(checkpoint, "model_proposal")
    final_messages = adapter.proposal_messages(checkpoint, correction=False)
    assert final_messages[0]["content"] == FINAL_SYSTEM
    assert "policy_supplement" not in json.dumps(final_messages)
    assert "P03.2" in body_of(final_messages)["available_refs"]
    with pytest.raises(DispatchError):
        adapter.validate_proposal(INTENT, checkpoint)
    # Source-valid proposals remain model assertions; status cannot select an action.
    proposal = json.loads(FIXTURE["valid"][0]["model_json"])
    assert (
        adapter.validate_proposal({"type": "final", "proposal": proposal}, checkpoint)[
            "type"
        ]
        == "final"
    )


@pytest.mark.parametrize(
    "mutation", ["empty", "unrelated", "cross_snapshot", "cross_policy"]
)
def test_incomplete_prerequisite_cannot_start_thinking(mutation: str) -> None:
    """Execution of a query does not prove the required paragraph was read."""
    checkpoint = status_checkpoint()
    checkpoint["steps"].append(policy_read(checkpoint, "P08.1"))
    result = checkpoint["steps"][-1]["result_json"]
    if mutation == "empty":
        result["content"]["matches"], result["evidence_refs"] = [], []
    elif mutation == "unrelated":
        checkpoint["steps"][-1] = policy_read(checkpoint, "P08.2")
    elif mutation == "cross_snapshot":
        result["content"]["snapshot_id"] = "11111111-1111-4111-8111-111111111111"
    else:
        result["content"]["matches"][0]["policy_version"] = "another-policy"
    with pytest.raises(DispatchError):
        validate(checkpoint, "model_proposal")
    with pytest.raises(DispatchError):
        SupportAgentV7Adapter().proposal_messages(checkpoint, correction=False)


def test_duplicate_failed_status_query_is_not_repeated() -> None:
    """A marker cannot silently retry an already executed query."""
    checkpoint = status_checkpoint()
    query = next(
        s
        for s in checkpoint["steps"]
        if s["result_json"]["content"].get("name") == "search_policy"
    )
    query["result_json"]["content"] = supplement_tool(missing_status_policy(checkpoint))
    with pytest.raises(DispatchError):
        validate(checkpoint, "search_policy")


@run_async
async def test_registered_prerequisite_search_has_no_model_intent() -> None:
    """The installed step entry derives exactly the Go-selected fixed query."""
    checkpoint = status_checkpoint()
    expected = policy_read(checkpoint, "P08.1")["result_json"]["content"]
    expected["matches"][0]["distance"] = 0.0
    frame = support_frame(checkpoint, "search_policy")
    frame["input"].update(
        adapter_id="support-agent-v1",
        executor_version=ADJUDICATION_V4_EXECUTOR_VERSION,
        provider_audit_policy="deepseek-audit-v2",
        tool_invocation_id="00000000-0000-4000-8000-000000000099",
    )
    clock = Clock()
    frame["emitted_mono_ms"] = clock.now
    hooks = SyntheticHooks(frame)
    dispatcher = AuthorizedDispatcher(
        frame,
        hooks=hooks,
        endpoints={
            "business": Endpoint("http://business.test", "fixture"),
            "ollama": Endpoint("http://ollama:11434"),
        },
        clock=clock,
        provider_audit_policy="deepseek-audit-v2",
    )

    def respond(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if path == "/api/embed":
            assert json.loads(request.content)["input"] == [SHARED["query"]]
        return httpx.Response(
            200, json=expected if path.endswith("/policies/search") else _payload(path)
        )

    for endpoint in ("business", "ollama"):
        dispatcher._clients[endpoint] = httpx.AsyncClient(
            transport=httpx.MockTransport(respond)
        )
    try:
        result = await execute_registered_step(
            frame, parse_runtime_input(frame), SupportAgentV7Adapter(), dispatcher
        )
        assert result["result"]["content"] == expected
        assert result["result"]["proposal"] is None
        assert len(hooks.intents) == len(hooks.observations) == 4
        assert len(hooks.reports) == 1
    finally:
        await dispatcher.aclose()
