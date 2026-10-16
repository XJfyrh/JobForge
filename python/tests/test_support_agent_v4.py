"""Independent final requests, version isolation and bounded reasoning metering."""

from __future__ import annotations

import copy
import json

import pytest
from jobforge_agent.deepseek import prepare_chat_request, proposal_object
from jobforge_agent.dispatch import DispatchError, RunCallContext
from jobforge_agent.errors import ToolError
from jobforge_agent.provider_audit import capture_chat_report
from jobforge_agent.support_agent import validate_agent_step
from jobforge_agent.support_agent_v3 import SupportAgentV3Adapter
from jobforge_agent.support_agent_v4 import FINAL_INSTRUCTIONS, SupportAgentV4Adapter
from test_deepseek import CALL_ID, _envelope, _wire
from test_support_agent import FIXTURE, agent_checkpoint, decision_step


def final_checkpoint() -> dict:
    """Keep a deliberately bad draft that is not admissible as a proposal."""
    checkpoint = agent_checkpoint()
    checkpoint["next_step"] = {"kind": "model_decision"}
    draft = json.loads(FIXTURE["valid"][0]["model_json"])
    draft["claims"][0]["refs"].append("UNRETRIEVED_DRAFT_ONLY")
    marker = SupportAgentV4Adapter().validate_proposal(
        {"type": "final", "proposal": draft}, checkpoint
    )
    checkpoint["steps"].append(decision_step(marker))
    checkpoint["next_step"] = {"kind": "model_proposal"}
    return checkpoint


def test_v4_investigation_is_exactly_v3_and_draft_cannot_approve() -> None:
    """The new first stage does not silently rewrite the investigated task."""
    checkpoint = agent_checkpoint()
    checkpoint["next_step"] = {"kind": "model_decision"}
    for correction in (False, True):
        assert SupportAgentV4Adapter().proposal_messages(
            checkpoint, correction=correction
        ) == SupportAgentV3Adapter().proposal_messages(
            checkpoint, correction=correction
        )
    final = final_checkpoint()
    validate_agent_step(final, "model_proposal", adjudication=True)
    assert final["steps"][-1]["result_json"]["proposal"] is None
    with pytest.raises(DispatchError):
        validate_agent_step(final, "submit_proposal", adjudication=True)
    with pytest.raises(DispatchError):
        validate_agent_step(final, "model_proposal")


def test_final_projection_never_contains_draft_and_keeps_actual_sources() -> None:
    """Exclusion happens before the original projection can copy model history."""
    checkpoint = final_checkpoint()
    before = copy.deepcopy(checkpoint)
    messages = SupportAgentV4Adapter().proposal_messages(checkpoint, correction=False)
    assert checkpoint == before
    assert "UNRETRIEVED_DRAFT_ONLY" not in json.dumps(messages)
    assert "investigation_complete" not in json.dumps(messages)
    assert str(messages[0]["content"]).endswith(FINAL_INSTRUCTIONS)
    evidence = copy.deepcopy(checkpoint)
    evidence["steps"].pop()
    assert (
        messages[1]
        == SupportAgentV3Adapter().proposal_messages(evidence, correction=False)[1]
    )
    with pytest.raises(DispatchError):
        SupportAgentV4Adapter().validate_proposal(
            {
                "type": "tool",
                "name": "search_policy",
                "arguments": {"query": "another"},
            },
            checkpoint,
        )
    with pytest.raises(DispatchError):
        SupportAgentV4Adapter().validate_proposal(
            {
                "type": "final",
                "proposal": checkpoint["steps"][-1]["result_json"]["content"]["draft"],
            },
            checkpoint,
        )


def test_final_model_capacity_is_versioned_and_equal_for_agent_and_fixed() -> None:
    """Reasoning shares completion capacity; temperature is not represented as zero."""
    messages = [{"role": "user", "content": "JSON"}]
    requests = [
        prepare_chat_request(
            messages,
            context=RunCallContext(
                "a" * 64,
                "b" * 64,
                "",
                agent=agent,
                wide_context=not agent,
                final_adjudication=True,
            ),
        )
        for agent in (True, False)
    ]
    assert requests[0].body == requests[1].body
    final = json.loads(requests[0].body)
    assert final["max_tokens"] == 16384 and final["thinking"] == {"type": "enabled"}
    assert final["reasoning_effort"] == "high" and "temperature" not in final
    assert requests[0].max_response_bytes == 1048576
    old = prepare_chat_request(messages, context=RunCallContext("a" * 64, "b" * 64, ""))
    assert (
        json.loads(old.body)["max_tokens"] == 1024 and old.max_response_bytes == 65536
    )


@pytest.mark.parametrize(
    "reasoning,count,mode",
    [
        ("", 0, "nonthinking"),
        (None, 0, "nonthinking"),
        ("r" * 70000, 4, "thinking"),
        ("r", 0, "invalid"),
    ],
    ids=["empty", "null", "large", "contradictory"],
)
def test_final_audit_observes_empty_reasoning_and_large_complete_usage(
    reasoning, count, mode
) -> None:
    """A full reasoning body is hashed, never included in the audit or final JSON."""
    envelope = _envelope()
    envelope["choices"][0]["message"]["reasoning_content"] = reasoning
    envelope["usage"]["completion_tokens_details"] = {"reasoning_tokens": count}
    raw = _wire(envelope)
    report = capture_chat_report(
        raw,
        http_status=200,
        physical_call_id=CALL_ID,
        expected_response_model="deepseek-flash",
        thinking=True,
    )
    assert report.provider_audit.schema_version == 2
    assert report.provider_audit.mode_state == mode
    assert len(json.dumps(report.to_dict())) < 2048
    if mode == "invalid":
        assert (
            report.usage is None and report.provider_audit.reasoning_state == "invalid"
        )
        with pytest.raises(ToolError, match="PROTOCOL_ERROR"):
            proposal_object(raw, thinking=True)
        return
    assert report.usage.input_tokens == 10 and report.usage.output_tokens == 5
    assert proposal_object(raw, thinking=True) == {"answer": "synthetic"}
    if reasoning and count:
        old = capture_chat_report(
            raw,
            http_status=200,
            physical_call_id=CALL_ID,
            expected_response_model="deepseek-flash",
        )
        assert old.usage is None and not old.provider_audit.response_complete


def test_truncated_final_is_failure_with_complete_usage() -> None:
    """Length never becomes a successful empty proposal or a metering zero."""
    envelope = _envelope()
    envelope["choices"][0]["finish_reason"] = "length"
    envelope["choices"][0]["message"].update(content="", reasoning_content="synthetic")
    envelope["usage"].update(
        completion_tokens=16384,
        total_tokens=16394,
        completion_tokens_details={"reasoning_tokens": 16384},
    )
    raw = _wire(envelope)
    report = capture_chat_report(
        raw,
        http_status=200,
        physical_call_id=CALL_ID,
        expected_response_model="deepseek-flash",
        thinking=True,
    )
    assert (
        report.usage.output_tokens == 16384
        and report.provider_audit.mode_state == "thinking"
    )
    with pytest.raises(ToolError, match="OUTPUT_INVALID"):
        proposal_object(raw, thinking=True)
