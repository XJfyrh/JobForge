"""The reviewed candidate changes instructions, never source truth or authority."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest
from jobforge_agent import runtime_adapters, runtime_registry
from jobforge_agent.dispatch import DispatchError
from jobforge_agent.runtime_input import APPROVAL_EXECUTOR_VERSION
from jobforge_agent.support_agent_v2 import SupportAgentV2Adapter
from jobforge_agent.support_agent_v3 import CONDITION_REVIEW, SupportAgentV3Adapter
from test_runtime_adapters import manifest
from test_support_agent import FIXTURE, agent_checkpoint


@pytest.mark.parametrize("correction", [False, True])
def test_condition_review_preserves_every_source_and_the_checkpoint(
    correction: bool,
) -> None:
    """Additional instructions grant no missing policy, event or decision."""
    checkpoint = agent_checkpoint()
    before = copy.deepcopy(checkpoint)
    original = SupportAgentV2Adapter().proposal_messages(
        checkpoint, correction=correction
    )
    actual = SupportAgentV3Adapter().proposal_messages(
        checkpoint, correction=correction
    )
    assert checkpoint == before
    assert actual[1] == original[1]
    assert actual[0]["content"] == str(original[0]["content"]) + CONDITION_REVIEW
    facts = json.loads(str(actual[1]["content"]))
    assert "P05.1" not in facts["available_refs"]
    assert facts["policy_navigation"]["conflict_definition"]["retrieved"] == []
    assert not {"expected", "required_claims", "active_ids", "gold"} & facts.keys()


def test_candidate_keeps_source_rejection_and_exact_output_contract() -> None:
    """The review cannot turn a missing paragraph into an allowed reference."""
    checkpoint = agent_checkpoint()
    proposal = json.loads(FIXTURE["valid"][0]["model_json"])
    adapter = SupportAgentV3Adapter()
    value = {"type": "final", "proposal": proposal}
    assert adapter.validate_proposal(
        value, checkpoint
    ) == SupportAgentV2Adapter().validate_proposal(value, checkpoint)
    proposal["claims"][0]["refs"].append("P05.1")
    with pytest.raises(DispatchError, match="OUTPUT_INVALID"):
        adapter.validate_proposal(value, checkpoint)
    proposal = json.loads(FIXTURE["valid"][0]["model_json"])
    proposal["condition_review"] = []
    with pytest.raises(DispatchError, match="OUTPUT_INVALID"):
        adapter.validate_proposal({"type": "final", "proposal": proposal}, checkpoint)


def test_condition_review_overflow_fails_without_truncating(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The appended instruction also counts toward the original message cap."""
    original = [
        {"role": "system", "content": "x" * (65536 - 2)},
        {"role": "user", "content": "{}"},
    ]
    monkeypatch.setattr(
        SupportAgentV2Adapter, "proposal_messages", lambda *a, **kw: original
    )
    with pytest.raises(DispatchError, match="OUTPUT_INVALID") as failure:
        SupportAgentV3Adapter().proposal_messages(agent_checkpoint(), correction=False)
    assert failure.value.stop and failure.value.fact == "size_limit"
    assert len(original[0]["content"]) == 65534


@pytest.mark.parametrize(
    "schema,prompt,valid",
    [
        (2, "support-agent-prompt-v3", True),
        (1, "support-agent-prompt-v3", False),
        (2, "support-agent-prompt-v4", False),
    ],
)
def test_manifest_selects_only_registered_v3(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    schema: int,
    prompt: str,
    valid: bool,
) -> None:
    """A selected implementation needs the explicit versioned manifest."""
    manifest(
        monkeypatch,
        tmp_path,
        {
            "schema_version": schema,
            "executor_version": APPROVAL_EXECUTOR_VERSION,
            "profiles": [
                {
                    "profile_id": "candidate",
                    "profile_hash": "a" * 64,
                    "adapter_id": "support-agent-v1",
                    "prompt_version": prompt,
                }
            ],
        },
    )
    args = ("support-agent-v1", "candidate", "a" * 64, APPROVAL_EXECUTOR_VERSION)
    if valid:
        assert type(runtime_adapters.resolve_adapter(*args)) is SupportAgentV3Adapter
    else:
        with pytest.raises(DispatchError, match="PROFILE_UNAVAILABLE"):
            runtime_adapters.resolve_adapter(*args)
    assert set(runtime_registry.REGISTRY) == {"support-fixed-v1", "support-agent-v1"}
