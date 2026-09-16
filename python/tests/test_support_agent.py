"""Shared dynamic decisions, accumulated provenance and actual dispatcher ACKs."""

from __future__ import annotations

import copy
import json

import httpx
import pytest
from jobforge_agent.dispatch import AuthorizedDispatcher, DispatchError, Endpoint
from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json
from jobforge_agent.runtime_input import AGENT_EXECUTOR_VERSION, parse_runtime_input
from jobforge_agent.step import execute_registered_step
from jobforge_agent.support_agent import (
    SupportAgentAdapter,
    next_tool_arguments,
    tool_decision,
    validate_agent_step,
)
from jobforge_agent.support_contract import support_sources
from jsonschema import Draft202012Validator
from referencing import Resource
from test_deepseek import _envelope, _wire
from test_dispatch import Clock, Hooks, run_async
from test_support_adapter import support_checkpoint, support_frame
from test_support_contract import FIXTURE, REGISTRY, ROOT

DECISIONS = json.loads(
    (ROOT / "api/support/agent-v1/fixtures.json").read_text(encoding="utf-8")
)
SCHEMA = json.loads(
    (ROOT / "api/support/agent-v1/schema.json").read_text(encoding="utf-8")
)
VALIDATOR = Draft202012Validator(
    SCHEMA,
    registry=REGISTRY.with_resource(SCHEMA["$id"], Resource.from_contents(SCHEMA)),
)


def decision_step(value: dict | None, kind: str = "model_decision") -> dict:
    """Build a protected decision; missing value is the one correction marker."""
    return {
        "step": {"kind": kind},
        "result_json": {
            "schema_version": 1,
            "tool_invocation_id": "",
            "physical_call_id": "44444444-4444-4444-8444-444444444444",
            "evidence_refs": [],
            "content": value,
            "proposal": None,
            "correction_required": value is None,
        },
    }


def agent_checkpoint(*, corrected: bool = False) -> dict:
    """Convert synthetic accepted sources into actual model/tool alternation."""
    protected = support_checkpoint()
    original = protected["steps"]
    protected["steps"] = original[:1]
    if corrected:
        protected["steps"].append(decision_step(None))
    for index, read in enumerate(original[1:]):
        name = read["step"]["kind"]
        args = (
            {"query": "delivery timing"}
            if name == "search_policy"
            else {"order_id": "order-synthetic"}
        )
        protected["steps"].append(
            decision_step(
                {"type": "tool", "name": name, "arguments": args},
                "protocol_correction" if corrected and index == 0 else "model_decision",
            )
        )
        protected["steps"].append(read)
    return protected


@pytest.mark.parametrize("case", DECISIONS["cases"], ids=lambda value: value["name"])
def test_shared_tool_decisions(case: dict) -> None:
    """Match the Go object boundary, normalization, duplicate and size rejection."""
    protected = support_checkpoint(missing=case["missing_order"])
    if case["valid"]:
        value = strict_json(case["model_json"].encode("utf-8"))
        VALIDATOR.validate(value)
        assert tool_decision(value, protected) == case["expected"]
    else:
        with pytest.raises((DispatchError, ToolError)):
            tool_decision(strict_json(case["model_json"].encode("utf-8")), protected)


def test_accumulated_policies_keep_prior_citations_and_reject_changed_text() -> None:
    """A new distance is allowed; immutable source content cannot change."""
    protected = agent_checkpoint()
    search = copy.deepcopy(protected["steps"][-1])
    search["result_json"]["content"]["matches"][0]["distance"] = 0.5
    protected["steps"].append(search)
    assert (
        len(
            support_sources(protected, repeated_search=True).contents["search_policy"][
                "matches"
            ]
        )
        == 1
    )
    search["result_json"]["content"]["matches"][0]["text"] = "changed"
    with pytest.raises(DispatchError):
        support_sources(protected, repeated_search=True)


def test_dynamic_history_and_tool_arguments_are_bound() -> None:
    """The adapter reads committed arguments and does not choose the next cursor."""
    protected = agent_checkpoint()
    validate_agent_step(protected, "model_decision")
    protected["steps"].pop()
    assert next_tool_arguments(protected, "search_policy") == {
        "query": "delivery timing"
    }
    with pytest.raises(DispatchError):
        next_tool_arguments(protected, "get_order")
    with pytest.raises(DispatchError):
        validate_agent_step(protected, "model_decision")


def test_prompt_preserves_actual_evidence_and_final_schema() -> None:
    """No gold is needed to construct the complete decision context."""
    protected = agent_checkpoint()
    model = {"type": "final", "proposal": json.loads(FIXTURE["valid"][0]["model_json"])}
    VALIDATOR.validate(model)
    actual = SupportAgentAdapter().validate_proposal(model, protected)
    assert actual["proposal"] == FIXTURE["valid"][0]["expected"]
    messages = SupportAgentAdapter().proposal_messages(protected, correction=False)
    facts = json.loads(str(messages[1]["content"]))
    assert facts["E2"] == protected["steps"][-3]["result_json"]["content"]
    assert len(facts["previous_tools"]) == 3
    assert "P01.1" in facts["available_refs"]


@run_async
@pytest.mark.parametrize(
    "mode", ["tool", "final", "first_invalid", "correction_invalid", "later_invalid"]
)
async def test_agent_output_uses_real_report_observation_and_one_correction(
    mode: str,
) -> None:
    """Synthetic HTTP exercises protocol and ACKs, not cloud quality."""
    protected = agent_checkpoint(corrected=mode == "later_invalid")
    kind = "model_decision"
    if mode == "correction_invalid":
        protected["steps"].append(decision_step(None))
        kind = "protocol_correction"
    frame = support_frame(protected, kind)
    frame["input"].update(
        adapter_id="support-agent-v1", executor_version=AGENT_EXECUTOR_VERSION
    )
    clock = Clock()
    frame["emitted_mono_ms"] = clock.now
    hooks = Hooks(clock)
    envelope = _envelope()
    value = {
        "type": "tool",
        "name": "search_policy",
        "arguments": {"query": "new focused policy"},
    }
    if mode == "final":
        value = {
            "type": "final",
            "proposal": json.loads(FIXTURE["valid"][0]["model_json"]),
        }
    if "invalid" in mode:
        value = {}
    envelope["choices"][0]["message"]["content"] = json.dumps(value)
    dispatcher = AuthorizedDispatcher(
        frame,
        hooks=hooks,
        endpoints={"deepseek": Endpoint("https://api.deepseek.com", "fixture")},
        clock=clock,
    )
    dispatcher._clients["deepseek"] = httpx.AsyncClient(
        transport=httpx.MockTransport(
            lambda request: httpx.Response(200, content=_wire(envelope))
        )
    )
    try:
        operation = execute_registered_step(
            frame, parse_runtime_input(frame), SupportAgentAdapter(), dispatcher
        )
        if mode in {"correction_invalid", "later_invalid"}:
            with pytest.raises(DispatchError, match="OUTPUT_INVALID"):
                await operation
        else:
            result = (await operation)["result"]
            assert result["correction_required"] == (mode == "first_invalid")
            if mode == "tool":
                assert result["content"] == value and result["proposal"] is None
            if mode == "final":
                assert result["proposal"] == FIXTURE["valid"][0]["expected"]
        assert len(hooks.intents) == len(hooks.reports) == len(hooks.observations) == 1
        assert dispatcher.last_confirmed_observation() is not None
    finally:
        await dispatcher.aclose()
