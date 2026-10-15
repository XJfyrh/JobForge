"""Exact supported prompt, unchanged investigation and inherited safety guards."""

from __future__ import annotations

import copy
import hashlib

import pytest
from jobforge_agent.dispatch import DispatchError
from jobforge_agent.support_agent_v8 import SupportAgentV8Adapter
from jobforge_agent.support_agent_v9 import FINAL_SYSTEM, SupportAgentV9Adapter
from test_support_agent_v8 import bundle_checkpoint


def test_final_changes_only_the_supported_system_without_mutating_checkpoint() -> None:
    """The deployed request keeps the exact diagnostic text and captured facts."""
    checkpoint = bundle_checkpoint()
    before = copy.deepcopy(checkpoint)
    old = SupportAgentV8Adapter().proposal_messages(checkpoint, correction=False)
    new = SupportAgentV9Adapter().proposal_messages(checkpoint, correction=False)
    assert new[1] == old[1]
    assert new[0] == {"role": "system", "content": FINAL_SYSTEM}
    assert checkpoint == before
    assert len(FINAL_SYSTEM.encode()) == 5751
    assert hashlib.sha256(FINAL_SYSTEM.encode()).hexdigest() == (
        "9d97b58df578dc47ebba208e56c2c8b4eb906141bdf00124513c2fd78aa4a88b"
    )


@pytest.mark.parametrize("correction", [False, True])
def test_investigation_and_correction_are_byte_identical_to_v8(
    correction: bool,
) -> None:
    """A final prompt change cannot enter investigation requests."""
    checkpoint = bundle_checkpoint()
    checkpoint["next_step"]["kind"] = "model_decision"
    assert SupportAgentV9Adapter().proposal_messages(
        checkpoint, correction=correction
    ) == SupportAgentV8Adapter().proposal_messages(checkpoint, correction=correction)


@pytest.mark.parametrize("mutation", ["missing_bundle", "missing_marker", "correction"])
def test_final_retains_bundle_marker_and_no_correction_guards(mutation: str) -> None:
    """The new selector cannot bypass v8's prerequisite validation."""
    checkpoint = bundle_checkpoint()
    if mutation == "missing_bundle":
        checkpoint["steps"].pop(1)
    elif mutation == "missing_marker":
        checkpoint["steps"].pop()
    with pytest.raises(DispatchError):
        SupportAgentV9Adapter().proposal_messages(
            checkpoint, correction=mutation == "correction"
        )
