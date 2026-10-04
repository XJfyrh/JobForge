"""S3 small-sample accounting keeps failed and original control exposure."""

from __future__ import annotations

from typing import Any

from tools.support_evaluation.score import ZERO_USAGE
from tools.support_recovery.report import group_calls, pair, sum_usage, timing_facts


def test_restart_comparison_includes_old_H0_and_never_zeros_unknown() -> None:
    """A low-cost H1 alone cannot create artificial savings or release a hold."""
    rows: list[dict[str, Any]] = []
    for arm, cost in (("C", 800), ("H0", 500), ("H1", 1000)):
        usage = dict(ZERO_USAGE, known_cost_microyuan=cost)
        rows.append(
            {
                "case_id": "DEV-002",
                "arm": arm,
                "ledger_usage": usage,
                "fault": {"phase": "commit_ack_lost"},
                "prefix_actions": [],
                "version_vector": {},
                "status": "finished",
                "score": {"correct": True, "safety": "passed"},
            }
        )
    comparison = pair(rows)
    assert comparison["restart_total_including_H0"]["known_cost_microyuan"] == 1500
    assert comparison["checkpoint_minus_restart_known_microyuan"] == -700
    assert comparison["exposure_settled"] and comparison["comparable"]
    rows[1]["ledger_usage"]["held_cost_microyuan"] = 2_105_344
    rows[1]["ledger_usage"]["unknown_chat_calls"] = 1
    assert not pair(rows)["exposure_settled"]
    totals = sum_usage(rows)
    assert totals is not None and totals["held_cost_microyuan"] == 2_105_344
    rows[1]["ledger_usage"] = None
    assert sum_usage(rows) is None and pair(rows)["restart_total_including_H0"] is None
    assert pair(rows)["checkpoint_minus_restart_known_microyuan"] is None


def test_changed_actual_path_or_missing_boundary_stays_in_report() -> None:
    """Different model choices cannot be stitched into a favorable pair."""
    rows: list[dict[str, Any]] = [
        {
            "case_id": "DEV-035",
            "arm": arm,
            "ledger_usage": dict(ZERO_USAGE),
            "fault": {"phase": "commit_ack_lost"},
            "prefix_actions": [],
            "version_vector": {},
            "status": "finished",
            "score": {"correct": True, "safety": "passed"},
        }
        for arm in ("C", "H0", "H1")
    ]
    rows[1]["prefix_actions"] = [{"kind": "different"}]
    assert not pair(rows)["comparable"]
    rows[1]["prefix_actions"] = []
    rows[0]["fault"] = None
    assert not pair(rows)["comparable"] and len(rows) == 3


def test_committed_search_includes_all_subcalls_only_from_actual_attempt() -> None:
    """Old/new tool attempts stay distinct, and H1 has no invented fault group."""
    calls: list[dict[str, Any]] = []
    for attempt, reserved in (
        (1, "2026-10-04T00:00:01Z"),
        (2, "2026-10-04T00:00:02Z"),
        (3, "2026-10-04T00:00:04Z"),
    ):
        for subcall in (
            "profile_version",
            "profile_tags",
            "query_embedding",
            "search_policy",
        ):
            calls.append(
                {
                    "physical_call_id": f"{attempt}-{subcall}",
                    "step_id": "same-logical-search",
                    "attempt_no": attempt,
                    "reserved_at": reserved,
                    "known_tokens": attempt,
                    "known_cost_microyuan": attempt * 10,
                }
            )
    actual = {
        "calls": {"items": calls},
        "steps": [
            {
                "record": {
                    "step_id": "same-logical-search",
                    "sequence": 5,
                    "output": {"physical_call_id": "2-search_policy"},
                }
            }
        ],
    }
    grouped = group_calls(
        actual,
        {
            "sequence": 5,
            "phase": "commit_ack_lost",
            "observed_at": "2026-10-04T00:00:03Z",
        },
    )
    expected = {
        "uncommitted_before_fault": 1,
        "committed_prefix": 2,
        "after_fault": 3,
    }
    for group, attempt in expected.items():
        assert grouped[group] == [
            call["physical_call_id"] for call in calls if call["attempt_no"] == attempt
        ]
    flattened = [identity for identities in grouped.values() for identity in identities]
    assert len(flattened) == len(set(flattened)) == len(calls) == 12
    selected = {identity for identity in flattened}
    assert (
        sum(
            call["known_tokens"]
            for call in calls
            if call["physical_call_id"] in selected
        )
        == 24
    )
    assert (
        sum(
            call["known_cost_microyuan"]
            for call in calls
            if call["physical_call_id"] in selected
        )
        == 240
    )
    uninterrupted = group_calls(actual, None)
    assert uninterrupted["no_fault_execution"] == [
        call["physical_call_id"] for call in calls
    ]
    assert all(not uninterrupted[group] for group in expected)


def test_timing_retains_natural_wait_and_does_not_use_cleanup_as_kill() -> None:
    """Persisted closure/claim and the emitted signal have separate meanings."""
    row = {
        "fault": {
            "attempt_no": 1,
            "kill_sent_at": "2026-10-04T00:00:01Z",
            "worker_wait_completed_at": "2026-10-04T00:00:02Z",
            "group_gone_confirmed_at": "2026-10-04T00:00:03Z",
            "child_group_at_boundary": "present",
        },
        "pre_fault_run": {
            "sampled_at": "2026-10-04T00:00:00Z",
            "lease_until": "2026-10-04T00:00:30Z",
        },
    }
    actual = {
        "run": {
            "state": "awaiting_approval",
            "created_at": "2026-10-04T00:00:00Z",
            "updated_at": "2026-10-04T00:00:40Z",
        }
    }
    control = {
        "attempts": [
            {"attempt_no": 1, "finished_at": "2026-10-04T00:00:31Z"},
            {"attempt_no": 2, "started_at": "2026-10-04T00:00:32Z"},
        ],
        "steps": [{"attempt_no": 2, "created_at": "2026-10-04T00:00:33Z"}],
    }
    facts = timing_facts(row, actual, control)
    assert facts["kill_to_close_seconds"] == 30
    assert facts["closed_to_claim_seconds"] == 1
    assert facts["kill_to_claim_seconds"] == 31
    assert facts["claim_to_first_step_seconds"] == 1
    assert facts["active_after_claim_seconds"] == 8
    assert facts["fault_to_terminal_seconds"] == 39
    assert facts["total_run_seconds"] == 40
    incomplete = timing_facts(row, actual, None)
    assert incomplete["kill_to_claim_seconds"] is None
    assert incomplete["total_run_seconds"] == 40
