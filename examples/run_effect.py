"""Inspect the independent effect; reconciliation is a separate explicit operator action."""

from __future__ import annotations

import argparse

from run_common import client, emit


def main() -> None:
    """Read the effect, optionally requesting one operator reconciliation."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_id")
    parser.add_argument(
        "--reconcile",
        action="store_true",
        help="operator: query the existing receipt once",
    )
    args = parser.parse_args()
    with client() as api:
        if args.reconcile:
            emit(api.reconcile(args.run_id))
        else:
            emit(api.effect(args.run_id))


if __name__ == "__main__":
    main()
