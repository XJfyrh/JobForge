"""Keep experimental changes explicit and diagnostic inputs separate from gold."""

import copy
import json
from typing import Any

from tools.support_diagnosis.prepare import GROUPS, group_messages, payload, schedule


def original() -> list[dict[str, Any]]:
    """Return a minimal source projection, without expected answers."""
    return [
        {"role": "system", "content": "original instructions"},
        {
            "role": "user",
            "content": json.dumps(
                {
                    "T": {"status": "open"},
                    "available_refs": ["P02.1"],
                    "policy_navigation": {"unchanged": True},
                    "policies": [
                        {
                            "chunk_id": "P02.1",
                            "text": "timing",
                            "evidence_ref": "actual-policy",
                        }
                    ],
                }
            ),
        },
    ]


def test_groups_change_one_factor_and_preserve_original() -> None:
    """Preserve old fields and distinguish corpus context from retrieval."""
    source = original()
    untouched = copy.deepcopy(source)
    variants = {
        g: group_messages(
            source, g, {"P02.1": "timing", "P06.1": "exception"}, "a" * 64
        )
        for g in GROUPS
    }
    assert source == untouched
    assert variants["A"] == variants["B"]
    assert variants["A"][1] == variants["D"][1]
    assert variants["A"][0] == variants["C"][0]
    added = json.loads(variants["C"][1]["content"])
    assert added["policy_navigation"] == {"unchanged": True}
    assert added["policies"][0]["evidence_ref"] == "actual-policy"
    assert added["policies"][1]["provenance"] == "diagnostic-policy"
    assert added["policies"][1]["evidence_ref"].startswith("diagnostic-policy:")
    assert set(added["available_refs"]) == {"P02.1", "P06.1"}


def test_same_capacity_and_only_thinking_parameters_differ() -> None:
    """Keep the model and capacity identical across mode treatments."""
    a, b = payload(original(), "A"), payload(original(), "B")
    assert a["max_tokens"] == b["max_tokens"] == 16384
    assert {k for k in a.keys() | b.keys() if a.get(k) != b.get(k)} == {
        "thinking",
        "reasoning_effort",
    }


def test_schedule_contains_every_trial_once_with_rotating_order() -> None:
    """Include each matched observation without output-dependent sampling."""
    rows = schedule([f"S5V3-{i:03}" for i in range(1, 21)])
    assert len(rows) == len({r["trial_id"] for r in rows}) == 240
    assert {r["group"] for r in rows[:4]} == set(GROUPS)
    for group in GROUPS:
        assert sum(r["group"] == group for r in rows) == 60
    assert [rows[i]["group"] for i in (0, 80, 160)] == ["A", "B", "C"]
