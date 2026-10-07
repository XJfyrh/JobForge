"""Version selection and source-only navigation never manufacture evidence."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest
from jobforge_agent import runtime_adapters, runtime_registry
from jobforge_agent.dispatch import DispatchError
from jobforge_agent.runtime_input import APPROVAL_EXECUTOR_VERSION
from jobforge_agent.support_agent import SupportAgentAdapter
from jobforge_agent.support_agent_v2 import POLICY_ROLES, SupportAgentV2Adapter
from test_runtime_adapters import manifest
from test_support_agent import FIXTURE, agent_checkpoint, decision_step


def test_navigation_uses_committed_tools_and_actual_aliases_only() -> None:
    """Pending decisions grant neither completed requests nor citation evidence."""
    protected = agent_checkpoint()
    query = POLICY_ROLES["critical_exception"][1]
    protected["steps"][-2]["result_json"]["content"]["arguments"]["query"] = query
    pending = {
        "type": "tool",
        "name": "search_policy",
        "arguments": {"query": "new pending query"},
    }
    protected["steps"].append(decision_step(pending))
    before = copy.deepcopy(protected)
    old = json.loads(
        str(
            SupportAgentAdapter().proposal_messages(protected, correction=False)[1][
                "content"
            ]
        )
    )
    messages = SupportAgentV2Adapter().proposal_messages(protected, correction=False)
    body = json.loads(str(messages[1]["content"]))
    assert protected == before
    for field in (
        "T",
        "E1",
        "E2",
        "policies",
        "available_refs",
        "time_differences",
        "remaining_tools",
        "allowed_ticket_status_modes",
    ):
        assert body[field] == old[field]
    assert body["completed_requests"][-1]["arguments"]["query"] == query
    assert pending["arguments"] not in [
        r["arguments"] for r in body["completed_requests"]
    ]
    assert "suggested_query" not in body["policy_roles"]["critical_exception"]
    assert "suggested_query" in body["policy_roles"]["timing_definition"]
    assert not body["policy_roles"]["conflict_definition"]["retrieved"]
    for role in body["policy_roles"].values():
        assert set(role["retrieved"]) <= set(body["available_refs"])
        if role["retrieved"]:
            assert "suggested_query" not in role
    assert (
        not {"expected", "gold", "action", "conclusion", "required_claims"}
        & body.keys()
    )
    assert (
        sum(len(str(message["content"]).encode("utf-8")) for message in messages)
        <= 65536
    )


def test_candidate_keeps_the_nested_proposal_field_set_closed() -> None:
    """Complete contract wording still rejects rather than drops an extra field."""
    protected = agent_checkpoint()
    before = copy.deepcopy(protected)
    proposal = json.loads(FIXTURE["valid"][0]["model_json"])
    proposal["extra_comment"] = "synthetic unknown field"
    with pytest.raises(DispatchError, match="OUTPUT_INVALID"):
        SupportAgentV2Adapter().validate_proposal(
            {"type": "final", "proposal": proposal}, protected
        )
    assert proposal["extra_comment"] == "synthetic unknown field"
    assert protected == before


@pytest.mark.parametrize(
    "outer_type", ["proposal", "no_action", "record_conclusion", None, {}]
)
def test_candidate_rejects_other_outer_types_without_normalizing(
    outer_type: object,
) -> None:
    """Inner validity cannot turn another outer type into a final proposal."""
    protected = agent_checkpoint()
    proposal = json.loads(FIXTURE["valid"][0]["model_json"])
    before = copy.deepcopy(protected)
    adapter = SupportAgentV2Adapter()
    assert (
        adapter.validate_proposal({"type": "final", "proposal": proposal}, protected)[
            "proposal"
        ]
        == FIXTURE["valid"][0]["expected"]
    )
    with pytest.raises(DispatchError, match="OUTPUT_INVALID"):
        adapter.validate_proposal({"type": outer_type, "proposal": proposal}, protected)
    assert protected == before


@pytest.mark.parametrize(
    "schema,prompt,valid",
    [
        (1, None, True),
        (2, "support-agent-prompt-v2", True),
        (1, "support-agent-prompt-v2", False),
        (2, None, False),
        (2, "arbitrary-module", False),
        (2, "support-agent-prompt-v1", False),
    ],
)
def test_manifest_selects_only_the_explicit_installed_variant(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    schema: int,
    prompt: str | None,
    valid: bool,
) -> None:
    """Historical manifests cannot select the new or a dynamic implementation."""
    entry = {
        "profile_id": "candidate",
        "profile_hash": "a" * 64,
        "adapter_id": "support-agent-v1",
    }
    if prompt is not None:
        entry["prompt_version"] = prompt
    manifest(
        monkeypatch,
        tmp_path,
        {
            "schema_version": schema,
            "executor_version": APPROVAL_EXECUTOR_VERSION,
            "profiles": [entry],
        },
    )
    args = (
        entry["adapter_id"],
        entry["profile_id"],
        entry["profile_hash"],
        APPROVAL_EXECUTOR_VERSION,
    )
    if valid:
        selected = runtime_adapters.resolve_adapter(*args)
        assert type(selected) is (
            SupportAgentV2Adapter if schema == 2 else SupportAgentAdapter
        )
    else:
        with pytest.raises(DispatchError, match="PROFILE_UNAVAILABLE"):
            runtime_adapters.resolve_adapter(*args)
    assert set(runtime_registry.REGISTRY) == {"support-fixed-v1", "support-agent-v1"}
