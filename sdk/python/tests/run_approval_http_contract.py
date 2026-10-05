"""Installed SDK against real control HTTP/PG; provider/receipts are synthetic."""

from __future__ import annotations

import sys
from collections.abc import Callable
from typing import Any

import httpx

from jobforge import (
    ApprovalConflictError,
    ConflictError,
    ForbiddenError,
    NotFoundError,
    RunClient,
    RunState,
)


def fails(kind: type[Exception], operation: Callable[[], Any]) -> None:
    """Require the actual stable server code through the installed SDK."""
    try:
        operation()
    except kind:
        return
    raise AssertionError("expected stable HTTP error")


def decisions(url: str, approved: str, rejected: str) -> None:
    """Role/tenant matrix, stable actor rotation and immutable first decision."""
    with (
        RunClient(url, "approval-reader") as reader,
        RunClient(url, "approval-operator") as operator,
        RunClient(url, "approval-approver") as approver,
        RunClient(url, "approval-rotated") as rotated,
        RunClient(url, "approval-other") as other,
        RunClient(url, "approval-foreign") as foreign,
        RunClient(url, "approval-foreign-operator") as foreign_operator,
    ):
        original = reader.approval(approved)
        assert original.status == "pending" and original.available
        assert original.proposal.decision == "proposal"
        assert reader.result(approved).disposition == "proposal"
        assert reader.effect(approved).state == "none"
        assert reader.action_calls(approved).items == []
        for client in (reader, operator):
            fails(
                ForbiddenError,
                lambda: client.decide_approval(
                    approved,
                    "approve",
                    original.proposal_hash,
                    idempotency_key="unauthorized",
                ),
            )
        for client in (foreign, foreign_operator):
            for operation in (
                client.approval,
                client.effect,
                client.action_calls,
                client.reconcile,
            ):
                fails(NotFoundError, lambda: operation(approved))
            fails(
                NotFoundError,
                lambda: client.decide_approval(
                    approved,
                    "approve",
                    original.proposal_hash,
                    idempotency_key="foreign",
                ),
            )
        decision = approver.decide_approval(
            approved, "approve", original.proposal_hash, idempotency_key="sdk-approve"
        )
        assert decision.run.state == RunState.READY and decision.run.recovery_count == 0
        assert decision.approval.actor_id == "stable-approver"
        replay = rotated.decide_approval(
            approved, "approve", original.proposal_hash, idempotency_key="sdk-approve"
        )
        alias = rotated.decide_approval(
            approved, "approve", original.proposal_hash, idempotency_key="sdk-alias"
        )
        assert replay.reused and alias.reused
        assert (
            replay.approval.approval_id
            == alias.approval.approval_id
            == decision.approval.approval_id
        )
        assert alias.approval.decided_at == decision.approval.decided_at
        assert reader.result(approved).disposition == "approved"
        fails(
            ConflictError,
            lambda: approver.decide_approval(
                approved,
                "reject",
                original.proposal_hash,
                idempotency_key="sdk-approve",
            ),
        )
        fails(
            ApprovalConflictError,
            lambda: other.decide_approval(
                approved,
                "approve",
                original.proposal_hash,
                idempotency_key="different-actor",
            ),
        )
        second = reader.approval(rejected)
        rejected_view = approver.decide_approval(
            rejected, "reject", second.proposal_hash, idempotency_key="sdk-reject"
        )
        assert (
            rejected_view.run.state == RunState.SUCCEEDED
            and rejected_view.run.outcome == "rejected"
        )
        assert reader.result(rejected).disposition == "rejected"
        assert reader.action_calls(rejected).items == []
        assert operator.reconcile(rejected).state == "none"
        for client in (reader, approver):
            fails(ForbiddenError, lambda: client.reconcile(rejected))
        # Raw malformed approval bodies exercise the real strict transport.
        path = url + "/v2/runs/" + approved + "/approval"
        for body in (
            b'{"schema_version":1,"decision":"approve","proposal_hash":null}',
            b'{"schema_version":1,"decision":"approve","proposal_hash":"'
            + original.proposal_hash.encode()
            + b'","actor_id":"spoof"}',
            b'{"schema_version":1,"decision":"approve","decision":"reject","proposal_hash":"'
            + original.proposal_hash.encode()
            + b'"}',
        ):
            response = httpx.post(
                path,
                content=body,
                headers={
                    "Authorization": "Bearer approval-approver",
                    "Idempotency-Key": "malformed",
                    "Content-Type": "application/json",
                },
                follow_redirects=False,
            )
            assert (
                response.status_code == 400
                and response.json()["error"]["code"] == "INVALID_ARGUMENT"
            )


def applied(url: str, uid: str) -> None:
    """Read persisted control facts; business acceptance is a separate layer."""
    with RunClient(url, "approval-reader") as reader:
        assert reader.result(uid).disposition == "applied"
        effect = reader.effect(uid)
        assert effect.state == "applied" and effect.receipt is not None
        calls = reader.action_calls(uid).items
        assert {item.kind for item in calls} == {"action_write", "receipt_query"}
        assert all(item.provider_metering == "not_applicable" for item in calls)


def main() -> None:
    """No fixture gold, secrets, model requests or background polling."""
    url, phase, *ids = sys.argv[1:]
    if phase == "decisions":
        decisions(url, *ids)
    elif phase == "applied":
        applied(url, *ids)
    else:
        raise ValueError("unknown contract phase")
    print("PASS real HTTP approval SDK: " + phase)


if __name__ == "__main__":
    main()
