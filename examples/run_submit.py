"""Submit once, retain stable keys, and explicitly poll the Run for a bounded time."""

from __future__ import annotations

import argparse
import os
import time

from jobforge.run_models import RunState
from run_common import client, emit


def main() -> None:
    """Submit one business intent and explicitly wait for its saved result."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ticket", required=True)
    parser.add_argument(
        "--request", required=True, help="retain this key after uncertainty"
    )
    parser.add_argument(
        "--wait-seconds", type=int, choices=range(1, 301), default=60, metavar="1..300"
    )
    args = parser.parse_args()
    with client() as api:
        accepted = api.submit(
            ticket_id=args.ticket,
            business_request_key=args.request,
            profile_id=os.environ["JOBFORGE_RUN_PROFILE"],
            budget_batch_id=os.environ["JOBFORGE_RUN_BUDGET_BATCH"],
            idempotency_key="submit-" + args.request,
        )
        run = accepted.run
        emit({"run_id": run.run_id, "reused": accepted.reused, "request": args.request})
        deadline = time.monotonic() + args.wait_seconds
        while not run.state.is_terminal and run.state != RunState.AWAITING_APPROVAL:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise SystemExit(
                    "Wait expired; retain the request key and query the printed Run ID."
                )
            time.sleep(min(0.5, remaining))
            run = api.get(run.run_id)
        emit({"run_id": run.run_id, "state": run.state, "error": run.error})
        if run.state in (RunState.FAILED, RunState.CANCELLED):
            raise SystemExit(1)


if __name__ == "__main__":
    main()
