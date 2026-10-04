"""Installed SDK inspects recovered real PostgreSQL facts via actual HTTP."""

from __future__ import annotations

import sys

from jobforge import RunClient


def main() -> None:
    """Use existing public views; internal recovery proofs remain SQL evidence."""
    url, run_id, old_call, new_call, step_id = sys.argv[1:]
    with RunClient(url, "recovery-reader") as reader:
        run = reader.get(run_id)
        steps = reader.steps(run_id, limit=32)
        events = reader.events(run_id, limit=100)
        calls = reader.calls(run_id)
        assert run.attempt_no == 2 and run.recovery_count == 1
        assert run.cursor_version >= 4 and run.state == "running"
        assert len(steps.items) == run.cursor_version
        recovered = [item for item in steps.items if item.step_id == step_id]
        assert len(recovered) == 1
        assert recovered[0].output["physical_call_id"] == new_call
        chats = {
            item.physical_call_id: item
            for item in calls.items
            if item.subcall == "chat"
        }
        assert old_call != new_call and old_call in chats and new_call in chats
        assert chats[old_call].attempt_no == 1 and chats[new_call].attempt_no == 2
        assert all(
            item.usage_known and item.held_cost_microyuan == 0
            for item in chats.values()
        )
        assert not calls.batch_frozen and calls.batch_stop_code is None
        assert run.budget.family.known_cost_microyuan == sum(
            item.known_cost_microyuan for item in chats.values()
        )
        assert any(
            item.event_type == "attempt_closed" and item.attempt_no == 1
            for item in events.items
        )
    print("PASS real HTTP recovery SDK")


if __name__ == "__main__":
    main()
