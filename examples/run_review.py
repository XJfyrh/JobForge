"""Read the original approval view; decide only with an explicitly reviewed hash."""

from __future__ import annotations

import argparse

from run_common import client, emit


def main() -> None:
    """Read a proposal or apply the caller's explicit original-hash decision."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_id")
    parser.add_argument("--decision", choices=("approve", "reject"))
    parser.add_argument("--proposal-hash")
    parser.add_argument("--request", help="stable approval idempotency key")
    args = parser.parse_args()
    if args.decision and not (args.proposal_hash and args.request):
        parser.error(
            "a decision requires the reviewed --proposal-hash and stable --request"
        )
    with client() as api:
        if args.decision:
            emit(
                api.decide_approval(
                    args.run_id,
                    args.decision,
                    args.proposal_hash,
                    idempotency_key=args.request,
                )
            )
        else:
            emit(api.approval(args.run_id))


if __name__ == "__main__":
    main()
