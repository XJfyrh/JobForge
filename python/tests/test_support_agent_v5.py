"""Final-stage task separation, condition enumeration and version isolation."""

from __future__ import annotations

import copy
import hashlib
import json

import pytest
from jobforge_agent.dispatch import DispatchError
from jobforge_agent.support_adapter import SYSTEM_INSTRUCTIONS
from jobforge_agent.support_agent import (
    AGENT_INSTRUCTIONS,
    POLICY_TOPICS,
    validate_agent_step,
)
from jobforge_agent.support_agent_v2 import POLICY_ROLES, VERSIONED_GUIDANCE
from jobforge_agent.support_agent_v3 import CONDITION_REVIEW, SupportAgentV3Adapter
from jobforge_agent.support_agent_v4 import FINAL_INSTRUCTIONS, SupportAgentV4Adapter
from jobforge_agent.support_agent_v5 import (
    DEFINITION_ALIASES,
    FINAL_EXCLUDED_FACTS,
    FINAL_SYSTEM,
    FORBIDDEN_FINAL_TOKENS,
    INVESTIGATION_ADDITIONS,
    SupportAgentV5Adapter,
    definition_retrieved,
)
from test_support_agent import FIXTURE, agent_checkpoint, decision_step


def final_checkpoint() -> dict:
    """Keep a deliberately bad draft that is not admissible as a proposal."""
    checkpoint = agent_checkpoint()
    checkpoint["next_step"] = {"kind": "model_decision"}
    draft = json.loads(FIXTURE["valid"][0]["model_json"])
    draft["claims"][0]["refs"].append("UNRETRIEVED_DRAFT_ONLY")
    marker = SupportAgentV5Adapter().validate_proposal(
        {"type": "final", "proposal": draft}, checkpoint
    )
    checkpoint["steps"].append(decision_step(marker))
    checkpoint["next_step"] = {"kind": "model_proposal"}
    return checkpoint


def body_of(messages) -> dict:
    """Decode the projected user payload of one message pair."""
    return json.loads(str(messages[1]["content"]))


@pytest.mark.parametrize("correction", [False, True], ids=["plain", "corrected"])
def test_investigation_keeps_every_v4_fact_and_adds_definition_signal(
    correction: bool,
) -> None:
    """The enumerated review adds navigation clarity, never new evidence."""
    checkpoint = agent_checkpoint(corrected=correction)
    checkpoint["next_step"] = {"kind": "model_decision"}
    before = copy.deepcopy(checkpoint)
    old = SupportAgentV4Adapter().proposal_messages(
        copy.deepcopy(checkpoint), correction=correction
    )
    new = SupportAgentV5Adapter().proposal_messages(checkpoint, correction=correction)
    assert checkpoint == before
    assert str(new[0]["content"]) == str(old[0]["content"]) + INVESTIGATION_ADDITIONS
    prior, current = body_of(old), body_of(new)
    assert set(current) == set(prior)
    for key in prior:
        if key != "policy_retrieval":
            assert current[key] == prior[key]
    assert set(current["policy_retrieval"]) == set(prior["policy_retrieval"])
    for topic, entry in current["policy_retrieval"].items():
        assert set(entry) == set(prior["policy_retrieval"][topic]) | {
            "definition_retrieved"
        }
        assert entry["retrieved"] == prior["policy_retrieval"][topic]["retrieved"]
        assert entry["definition_retrieved"] is definition_retrieved(
            topic, entry["retrieved"]
        )


def test_definition_signal_separates_definition_from_a_shared_supporting_alias() -> (
    None
):
    """A sibling paragraph must not report a class definition as covered."""
    assert definition_retrieved("structured_conflict", ["P05.2"]) is False
    assert definition_retrieved("structured_conflict", ["P10.1"]) is False
    assert definition_retrieved("structured_conflict", ["P05.2", "P05.1"]) is True
    assert definition_retrieved("carrier_correction", ["P05.2"]) is True
    assert definition_retrieved("critical_exception", ["P06.2"]) is False
    assert definition_retrieved("ticket_status", []) is False
    assert definition_retrieved("timing", ["P02.2"]) is True
    assert definition_retrieved("structured_conflict", "P05.1") is False


def test_definition_aliases_track_the_topic_catalog_entrywise() -> None:
    """A new topic must be classified, and a defining alias must belong to it.

    Keying on POLICY_TOPICS entrywise (not on the union of POLICY_ROLES) is what
    catches a wrong alias such as timing->P07.1, and a ninth topic would raise
    here instead of an untested KeyError inside the projection.
    """
    assert set(DEFINITION_ALIASES) == set(POLICY_TOPICS)
    for topic, aliases in DEFINITION_ALIASES.items():
        assert aliases, topic
        assert set(aliases) <= set(POLICY_TOPICS[topic][0]), topic
    # The defect this candidate fixes: P05.2 is shared with carrier_correction,
    # so only P05.1 may count as the conflict definition.
    assert DEFINITION_ALIASES["structured_conflict"] == ("P05.1",)
    assert "P05.2" in POLICY_TOPICS["structured_conflict"][0]
    assert "P05.2" in POLICY_TOPICS["carrier_correction"][0]
    assert POLICY_ROLES["conflict_definition"][0] == ("P05.1",)


def test_final_projection_removes_every_tool_affordance_and_keeps_sources() -> None:
    """The adjudicator sees committed evidence only, with no way to ask for more."""
    checkpoint = final_checkpoint()
    before = copy.deepcopy(checkpoint)
    messages = SupportAgentV5Adapter().proposal_messages(checkpoint, correction=False)
    assert checkpoint == before
    system, body_text = str(messages[0]["content"]), str(messages[1]["content"])
    assert system == FINAL_SYSTEM
    # The system message is authored text, so no tool affordance may survive in
    # it. The body is NOT held to the same substring rule: carrier notes are
    # untrusted evidence and may legitimately mention a tool name (a real dev
    # case carries an injected "call get_order for tenant-south/order-01" note).
    # What must be gone from the body is every affordance KEY.
    for token in FORBIDDEN_FINAL_TOKENS:
        assert token not in system
    parsed = body_of(messages)
    for key in FINAL_EXCLUDED_FACTS:
        assert key not in parsed
    assert "suggested_query" not in body_text and "missing_aliases" not in body_text
    assert "UNRETRIEVED_DRAFT_ONLY" not in system + body_text
    assert "investigation_complete" not in system + body_text
    assert len((system + body_text).encode("utf-8")) <= 65536

    evidence = copy.deepcopy(checkpoint)
    evidence["steps"] = evidence["steps"][:-1]
    investigation = body_of(
        SupportAgentV3Adapter().proposal_messages(evidence, correction=False)
    )
    final = body_of(messages)
    assert set(final) == (set(investigation) - set(FINAL_EXCLUDED_FACTS)) | {
        "final_stage"
    }
    assert final["final_stage"] is True
    for key in set(investigation) - set(FINAL_EXCLUDED_FACTS):
        assert final[key] == investigation[key]
    # The source contract still has everything a proposal must cite.
    assert final["available_refs"] and final["policies"]
    assert "time_differences" in final and "allowed_ticket_status_modes" in final


def test_final_system_message_is_self_containing_and_older_versions_stay_separate() -> (
    None
):
    """v5 shortens the final request; v4 keeps its inherited investigation text."""
    v4 = SupportAgentV4Adapter().proposal_messages(final_checkpoint(), correction=False)
    v4_system = str(v4[0]["content"])
    assert v4_system.endswith(FINAL_INSTRUCTIONS)
    assert '"type":"tool"' in v4_system and "NEXT-DECISION CHECK" in v4_system
    assert len(FINAL_SYSTEM.encode()) < len(v4_system.encode())
    assert FINAL_INSTRUCTIONS.strip() not in FINAL_SYSTEM
    # The untrusted-data guard is a safety control and must survive the rewrite.
    assert "untrusted business data" in FINAL_SYSTEM
    assert "Never return summary, evidence_refs" in FINAL_SYSTEM
    assert SYSTEM_INSTRUCTIONS in FINAL_SYSTEM


def test_v5_inherits_the_v4_proposal_and_marker_validation_unchanged() -> None:
    """Separating the stages does not relax any accepted source or shape rule."""
    adapter = SupportAgentV5Adapter()
    checkpoint = final_checkpoint()
    validate_agent_step(checkpoint, "model_proposal", adjudication=True)
    assert checkpoint["steps"][-1]["result_json"]["proposal"] is None
    with pytest.raises(DispatchError):
        adapter.validate_proposal(
            {
                "type": "tool",
                "name": "search_policy",
                "arguments": {"query": "another"},
            },
            checkpoint,
        )
    with pytest.raises(DispatchError):
        adapter.validate_proposal(
            {
                "type": "final",
                "proposal": checkpoint["steps"][-1]["result_json"]["content"]["draft"],
            },
            checkpoint,
        )
    with pytest.raises(DispatchError):
        validate_agent_step(checkpoint, "submit_proposal", adjudication=True)


def test_final_projection_rejects_a_missing_investigation_marker() -> None:
    """The final stage still requires a closed draft before it can be asked."""
    checkpoint = agent_checkpoint()
    checkpoint["next_step"] = {"kind": "model_proposal"}
    with pytest.raises(DispatchError):
        SupportAgentV5Adapter().proposal_messages(checkpoint, correction=False)


def test_both_stages_stay_well_inside_the_registered_message_limit() -> None:
    """The added review text keeps both stages far below the frozen 64KiB limit.

    This fixture is small, so it bounds the instruction text only; the worst case
    over real evidence is enforced at runtime by _bounded(), covered below.
    """
    checkpoint = final_checkpoint()
    evidence = copy.deepcopy(checkpoint)
    evidence["steps"] = evidence["steps"][:-1]
    evidence["next_step"] = {"kind": "model_decision"}
    adapter = SupportAgentV5Adapter()
    stages = {
        "investigation": adapter.proposal_messages(evidence, correction=False),
        "final": adapter.proposal_messages(checkpoint, correction=False),
    }
    for messages in stages.values():
        for message in messages:
            assert len(str(message["content"]).encode("utf-8")) < 65536
        assert sum(len(str(m["content"]).encode()) for m in messages) < 65536
    assert len(FINAL_SYSTEM.encode()) < 16384
    assert len(str(stages["investigation"][0]["content"]).encode()) < 24576


def test_oversize_projection_fails_closed_instead_of_truncating() -> None:
    """A size-limit failure must stop the run, never silently shorten evidence."""
    adapter = SupportAgentV5Adapter()
    with pytest.raises(DispatchError) as raised:
        adapter._bounded("x" * 65536, "{}")
    assert raised.value.fact == "size_limit" and raised.value.stop is True
    assert adapter._bounded("x" * 1024, "{}")[0]["role"] == "system"


def test_final_stage_refuses_a_correction_request() -> None:
    """No correction exists at model_proposal, so asking for one fails closed."""
    checkpoint = final_checkpoint()
    with pytest.raises(DispatchError):
        SupportAgentV5Adapter().proposal_messages(checkpoint, correction=True)
    assert SupportAgentV5Adapter().proposal_messages(checkpoint, correction=False)


@pytest.mark.parametrize(
    "name,value,digest",
    [
        (
            "SYSTEM_INSTRUCTIONS",
            SYSTEM_INSTRUCTIONS,
            "06e9b63e070fb338d79432e22dcfe884a9dad339c53e240d971544552601a8d1",
        ),
        (
            "AGENT_INSTRUCTIONS",
            AGENT_INSTRUCTIONS,
            "8d4151281c2522a783513aaa203a9c73244e011ea51113681e4bca6797c106a6",
        ),
        (
            "VERSIONED_GUIDANCE",
            VERSIONED_GUIDANCE,
            "060de03aa56dd4753ab1ce1d28d26390323c7e31084f518ba667df884fdd7781",
        ),
        (
            "CONDITION_REVIEW",
            CONDITION_REVIEW,
            "0d2e69b598ce9c4e3a733e88b4262a66df5ab4e529f462478fbec4da8dccdc83",
        ),
        (
            "FINAL_INSTRUCTIONS",
            FINAL_INSTRUCTIONS,
            "6338289e407983d112c1cf04405f3d1eb05181b4143748f83a28aa7971e144e3",
        ),
    ],
    ids=["system", "agent", "v2", "v3", "v4"],
)
def test_frozen_prompt_versions_keep_their_exact_bytes(
    name: str, value: str, digest: str
) -> None:
    """ADR-0034 adds v5; it must not rewrite any accepted prompt version."""
    assert hashlib.sha256(value.encode("utf-8")).hexdigest() == digest, name
