"""Cross-language supplement contract, actual sources and version isolation."""

from __future__ import annotations

import copy
import json

import httpx
import pytest
from jobforge_agent.dispatch import AuthorizedDispatcher, DispatchError, Endpoint
from jobforge_agent.policy_supplement import (
    is_policy_supplement,
    supplement_tool,
    validate_supplement,
)
from jobforge_agent.runtime_input import (
    ADJUDICATION_V3_EXECUTOR_VERSION,
    RuntimeInputError,
    parse_runtime_input,
    validate_step_result,
)
from jobforge_agent.step import execute_registered_step
from jobforge_agent.support_agent import (
    POLICY_TOPICS,
    next_tool_arguments,
    validate_agent_step,
)
from jobforge_agent.support_agent_v5 import (
    DEFINITION_ALIASES,
    FINAL_SYSTEM,
    SupportAgentV5Adapter,
)
from jobforge_agent.support_agent_v6 import SupportAgentV6Adapter
from jsonschema import Draft202012Validator
from test_deepseek import _envelope, _wire
from test_dispatch import Clock, Hooks, run_async
from test_run_tools import SyntheticHooks, _payload
from test_support_adapter import support_frame
from test_support_agent import FIXTURE, decision_step
from test_support_agent_v5 import body_of, final_checkpoint
from test_support_contract import REGISTRY, ROOT

SHARED = json.loads((ROOT / "api/support/agent-v2/fixtures.json").read_bytes())
SCHEMA = Draft202012Validator(
    json.loads((ROOT / "api/support/agent-v2/schema.json").read_bytes()),
    registry=REGISTRY,
)
INTENT = {
    "type": "policy_supplement",
    "topic": "missing_facts",
    "required_aliases": ["P03.2"],
}


def supplemented_checkpoint() -> dict:
    """Append a source-bound synthetic definition after the supplement intent."""
    checkpoint = final_checkpoint()
    read = copy.deepcopy(
        next(s for s in checkpoint["steps"] if s["step"]["kind"] == "search_policy")
    )
    match = copy.deepcopy(read["result_json"]["content"]["matches"][0])
    match.update(
        chunk_id="P03.2", source="P03.md", text="Synthetic clarification definition"
    )
    match["evidence_ref"] = match["evidence_ref"][:-5] + "P03.2"
    read["result_json"]["content"]["matches"] = [match]
    read["result_json"]["evidence_refs"] = [match["evidence_ref"]]
    checkpoint["steps"].extend(
        [decision_step(copy.deepcopy(INTENT), "model_proposal"), read]
    )
    return checkpoint


@pytest.mark.parametrize("case", SHARED["cases"], ids=lambda c: c["name"])
def test_shared_intent_shapes(case: dict) -> None:
    """Keep the Python and Go intent boundaries identical."""
    assert is_policy_supplement(case["value"]) is case["valid"]
    # JSON Schema covers shape; sorted order is the shared runtime constraint.
    assert SCHEMA.is_valid(case["value"]) is (
        case["valid"] or case["name"] == "unsorted"
    )


def test_shared_navigation_is_definition_only() -> None:
    """Supporting aliases cannot masquerade as requested definitions."""
    assert SHARED["topics"] == {
        key: {"aliases": list(aliases), "query": POLICY_TOPICS[key][1]}
        for key, aliases in DEFINITION_ALIASES.items()
    }


def test_first_adjudication_is_independent_and_supplement_is_not_proposal() -> None:
    """Missing policy is navigation, not a source or approval artifact."""
    checkpoint = final_checkpoint()
    before = copy.deepcopy(checkpoint)
    adapter = SupportAgentV6Adapter()
    messages = adapter.proposal_messages(checkpoint, correction=False)
    facts = body_of(messages)
    assert "UNRETRIEVED_DRAFT_ONLY" not in json.dumps(messages)
    assert "investigation_complete" not in json.dumps(messages)
    assert "no additional search is possible" not in str(messages[0]["content"])
    assert facts["policy_supplement"]["missing_facts"]["missing_aliases"] == [
        "P03.1",
        "P03.2",
    ]
    assert "P03.2" not in facts["available_refs"]
    assert adapter.validate_proposal(INTENT, checkpoint) == INTENT
    with pytest.raises(DispatchError):
        SupportAgentV5Adapter().validate_proposal(INTENT, checkpoint)
    assert checkpoint == before
    checkpoint["steps"].append(decision_step(INTENT, "model_proposal"))
    validate_agent_step(checkpoint, "search_policy", adjudication=True, supplement=True)
    assert (
        next_tool_arguments(checkpoint, "search_policy", supplement=True)
        == supplement_tool(INTENT)["arguments"]
    )
    for kind in ("get_order", "get_delivery", "submit_proposal"):
        with pytest.raises(DispatchError):
            validate_agent_step(checkpoint, kind, adjudication=True, supplement=True)
    with pytest.raises((DispatchError, RuntimeInputError)):
        validate_agent_step(checkpoint, "search_policy", adjudication=True)
    result = checkpoint["steps"][-1]["result_json"]
    with pytest.raises(RuntimeInputError):
        validate_step_result(result, "model_proposal")


def test_supplemented_final_excludes_intent_and_draft_and_cannot_supplement_again() -> (
    None
):
    """A successful read yields independent final-only adjudication."""
    checkpoint = supplemented_checkpoint()
    adapter = SupportAgentV6Adapter()
    before = copy.deepcopy(checkpoint)
    validate_agent_step(
        checkpoint, "model_proposal", adjudication=True, supplement=True
    )
    messages = adapter.proposal_messages(checkpoint, correction=False)
    assert messages[0]["content"] == FINAL_SYSTEM
    assert "P03.2" in body_of(messages)["available_refs"]
    assert "policy_supplement" not in json.dumps(messages)
    assert "UNRETRIEVED_DRAFT_ONLY" not in json.dumps(messages)
    with pytest.raises(DispatchError):
        adapter.validate_proposal(INTENT, checkpoint)
    proposal = json.loads(FIXTURE["valid"][0]["model_json"])
    assert (
        adapter.validate_proposal({"type": "final", "proposal": proposal}, checkpoint)[
            "type"
        ]
        == "final"
    )
    assert checkpoint == before


@pytest.mark.parametrize(
    "mutation", ["empty", "unrelated", "cross_snapshot", "cross_policy", "one_of_two"]
)
def test_incomplete_or_unbound_search_cannot_reach_final(mutation: str) -> None:
    """An executed search is insufficient without all source-bound aliases."""
    checkpoint = supplemented_checkpoint()
    read = checkpoint["steps"][-1]["result_json"]
    if mutation == "empty":
        read["content"]["matches"], read["evidence_refs"] = [], []
    elif mutation == "unrelated":
        hit = read["content"]["matches"][0]
        hit["chunk_id"] = "P04.1"
        hit["evidence_ref"] = hit["evidence_ref"][:-5] + "P04.1"
        read["evidence_refs"] = [hit["evidence_ref"]]
    elif mutation == "cross_snapshot":
        read["content"]["snapshot_id"] = "11111111-1111-4111-8111-111111111111"
    elif mutation == "cross_policy":
        read["content"]["matches"][0]["policy_version"] = "another-policy"
    else:
        checkpoint["steps"][-2]["result_json"]["content"]["required_aliases"] = [
            "P03.1",
            "P03.2",
        ]
    with pytest.raises(DispatchError):
        validate_agent_step(
            checkpoint, "model_proposal", adjudication=True, supplement=True
        )


def test_specific_missing_alias_and_duplicate_query() -> None:
    """An existing sibling does not block the missing alias, but repeats fail."""
    checkpoint = final_checkpoint()
    search = next(
        s for s in checkpoint["steps"] if s["step"]["kind"] == "search_policy"
    )
    hit = copy.deepcopy(search["result_json"]["content"]["matches"][0])
    hit.update(chunk_id="P03.1", source="P03.md", text="Synthetic P03.1")
    hit["evidence_ref"] = hit["evidence_ref"][:-5] + "P03.1"
    search["result_json"]["content"]["matches"].append(hit)
    search["result_json"]["evidence_refs"].append(hit["evidence_ref"])
    assert validate_supplement(INTENT, checkpoint) == INTENT
    adapter = SupportAgentV6Adapter()
    assert body_of(adapter.proposal_messages(checkpoint, correction=False))[
        "policy_supplement"
    ]["missing_facts"]["missing_aliases"] == ["P03.2"]
    with pytest.raises(DispatchError):
        validate_supplement(dict(INTENT, required_aliases=["P03.1"]), checkpoint)
    prior = next(
        s
        for s in checkpoint["steps"]
        if s["result_json"]["content"].get("name") == "search_policy"
    )
    prior["result_json"]["content"] = supplement_tool(INTENT)
    with pytest.raises(DispatchError):
        validate_supplement(INTENT, checkpoint)
    assert (
        "missing_facts"
        not in body_of(adapter.proposal_messages(checkpoint, correction=False))[
            "policy_supplement"
        ]
    )


@run_async
async def test_registered_supplement_search_dispatches_without_old_query_entry() -> (
    None
):
    """Exercise the real step entry, four dispatches and acknowledged result."""
    checkpoint = supplemented_checkpoint()
    expected = checkpoint["steps"].pop()["result_json"]["content"]
    expected["matches"][0]["distance"] = 0.0
    frame = support_frame(checkpoint, "search_policy")
    frame["input"].update(
        adapter_id="support-agent-v1",
        executor_version=ADJUDICATION_V3_EXECUTOR_VERSION,
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
            assert json.loads(request.content)["input"] == [
                POLICY_TOPICS["missing_facts"][1]
            ]
        return httpx.Response(
            200,
            json=expected if path.endswith("/policies/search") else _payload(path),
        )

    for endpoint in ("business", "ollama"):
        dispatcher._clients[endpoint] = httpx.AsyncClient(
            transport=httpx.MockTransport(respond)
        )
    try:
        result = await execute_registered_step(
            frame, parse_runtime_input(frame), SupportAgentV6Adapter(), dispatcher
        )
        assert result["result"]["content"] == expected
        assert result["result"]["proposal"] is None
        assert len(hooks.intents) == len(hooks.observations) == 4
        assert len(hooks.reports) == 1
    finally:
        await dispatcher.aclose()


@run_async
async def test_registered_first_thinking_returns_committable_supplement() -> None:
    """The accepted model response must survive the real result finalizer."""
    frame = support_frame(final_checkpoint(), "model_proposal")
    frame["input"].update(
        adapter_id="support-agent-v1",
        executor_version=ADJUDICATION_V3_EXECUTOR_VERSION,
        provider_audit_policy="deepseek-audit-v2",
    )
    clock = Clock()
    frame["emitted_mono_ms"] = clock.now
    hooks = Hooks(clock)
    hooks.patch = {"input_token_limit": 1048576, "output_token_limit": 16384}
    dispatcher = AuthorizedDispatcher(
        frame,
        hooks=hooks,
        endpoints={"deepseek": Endpoint("https://api.deepseek.com", "fixture")},
        clock=clock,
        provider_audit_policy="deepseek-audit-v2",
    )
    envelope = _envelope()
    envelope["choices"][0]["message"].update(
        content=json.dumps(INTENT), reasoning_content="Synthetic private reasoning"
    )
    envelope["usage"]["completion_tokens_details"] = {"reasoning_tokens": 2}
    dispatcher._clients["deepseek"] = httpx.AsyncClient(
        transport=httpx.MockTransport(
            lambda request: httpx.Response(200, content=_wire(envelope))
        )
    )
    try:
        result = await execute_registered_step(
            frame, parse_runtime_input(frame), SupportAgentV6Adapter(), dispatcher
        )
        assert result["outcome"] == "success"
        assert result["result"]["content"] == INTENT
        assert result["result"]["proposal"] is None
        assert len(hooks.intents) == len(hooks.observations) == len(hooks.reports) == 1
    finally:
        await dispatcher.aclose()
