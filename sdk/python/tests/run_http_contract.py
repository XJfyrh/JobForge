"""Installed Run SDK against a production HTTP router and real PostgreSQL.

The Go integration harness owns service/database setup; this runner never mocks
HTTP or starts a model. Invoke with URL, registered test profile, batch and ticket.
"""

from __future__ import annotations

import json
import os
import sys
from concurrent.futures import ThreadPoolExecutor
from uuid import uuid4

import httpx
import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider

from jobforge import (
    AlreadyTerminalError,
    ConflictError,
    ForbiddenError,
    InvalidArgumentError,
    InvalidTransitionError,
    NotFoundError,
    ProfileUnavailableError,
    RunClient,
    RunState,
    UnauthorizedError,
)


class CountingTransport(httpx.HTTPTransport):
    """Count real HTTP sends and inspect propagation without replacing replies."""

    def __init__(self) -> None:
        super().__init__(retries=0)
        self.sent = 0

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        """Delegate a real exchange after checking its stable trace identity."""
        self.sent += 1
        assert "11111111111111111111111111111111" in request.headers["traceparent"]
        return super().handle_request(request)


def main() -> None:
    """Exercise admission, tenant/role boundaries and stable public operations."""
    url, profile, batch, ticket = sys.argv[1:]
    trace.set_tracer_provider(TracerProvider())
    key_a = os.environ.get("RUN_CONTRACT_OPERATOR_KEY", "contract-a")
    key_b = os.environ.get("RUN_CONTRACT_FOREIGN_KEY", "contract-b")
    key_reader = os.environ.get("RUN_CONTRACT_READER_KEY", "contract-reader")
    suffix = uuid4().hex
    transport = CountingTransport()
    parent = "00-11111111111111111111111111111111-2222222222222222-01"
    with (
        RunClient(url, key_a, traceparent=parent, transport=transport) as client,
        RunClient(url, key_b) as foreign,
        RunClient(url, key_reader) as reader,
        RunClient(url, "invalid-key") as unauthorized,
    ):
        with pytest.raises(UnauthorizedError):
            unauthorized.get(str(uuid4()))
        with pytest.raises(NotFoundError):
            client.get(str(uuid4()))
        with pytest.raises(ProfileUnavailableError):
            client.submit(
                ticket,
                "missing-profile-" + suffix,
                "unregistered-profile",
                batch,
                idempotency_key="missing-profile-" + suffix,
            )

        submitted = client.submit(
            ticket,
            "intent-" + suffix,
            profile,
            batch,
            idempotency_key="submit-" + suffix,
        )
        run_id = submitted.run.run_id
        assert not submitted.reused and submitted.run.state == RunState.READY
        before = transport.sent
        repeated = client.submit(
            ticket,
            "intent-" + suffix,
            profile,
            batch,
            idempotency_key="submit-" + suffix,
        )
        assert transport.sent == before + 1
        assert repeated.reused and repeated.run.run_id == run_id
        rebound = client.submit(
            ticket,
            "intent-" + suffix,
            profile,
            batch,
            idempotency_key="new-key-" + suffix,
        )
        assert rebound.reused and rebound.run.run_id == run_id
        assert rebound.run.run_deadline == submitted.run.run_deadline
        for operation_key in ("submit-" + suffix, "changed-" + suffix):
            with pytest.raises(ConflictError):
                client.submit(
                    ticket,
                    "intent-" + suffix,
                    profile,
                    batch,
                    idempotency_key=operation_key,
                    run_timeout_seconds=3599,
                )

        assert reader.get(run_id).run_id == run_id
        with pytest.raises(ForbiddenError):
            reader.submit(
                ticket,
                "reader-" + suffix,
                profile,
                batch,
                idempotency_key="reader-" + suffix,
            )
        with pytest.raises(ForbiddenError):
            reader.cancel(run_id, idempotency_key="reader-cancel")
        with pytest.raises(ForbiddenError):
            reader.retry(run_id, idempotency_key="reader-retry")
        for operation in (foreign.get, foreign.steps, foreign.events, foreign.result):
            with pytest.raises(NotFoundError):
                operation(run_id)
        for write_operation in (foreign.cancel, foreign.retry):
            with pytest.raises(NotFoundError):
                write_operation(run_id, idempotency_key="foreign-operation")

        current = client.get(run_id)
        assert current.budget.family.used.physical_http == 0
        assert current.budget.run_usage.physical_http == 0
        assert current.version_vector.ticket.id == ticket
        assert client.steps(run_id).items == []
        events = client.events(run_id, limit=1)
        assert len(events.items) == 1 and events.items[0].sequence >= 1
        result = client.result(run_id)
        assert not result.available and result.kind is None and result.ref is None
        with pytest.raises(InvalidTransitionError):
            client.retry(run_id, idempotency_key="early-retry")

        accepted = client.cancel(run_id, idempotency_key="cancel-" + suffix)
        assert accepted.run.state == RunState.CANCELLED and not accepted.reused
        repeated_cancel = client.cancel(run_id, idempotency_key="cancel-" + suffix)
        assert repeated_cancel.reused
        assert repeated_cancel.operation_id == accepted.operation_id
        with pytest.raises(AlreadyTerminalError):
            client.cancel(run_id, idempotency_key="late-cancel-" + suffix)

        def retry(index: int) -> str:
            with RunClient(url, key_a) as other:
                return other.retry(
                    run_id, idempotency_key=f"concurrent-retry-{suffix}-{index}"
                ).run.run_id

        with ThreadPoolExecutor(max_workers=2) as pool:
            successors = list(pool.map(retry, range(2)))
        assert len(set(successors)) == 1 and successors[0] != run_id
        successor = client.get(successors[0])
        assert successor.state == RunState.READY and successor.cursor_version == 0
        assert successor.retry_of_run_id == run_id
        assert successor.business_request_id == current.business_request_id
        assert successor.budget.family.id == current.budget.family.id
        assert successor.snapshot_id != current.snapshot_id
        assert client.get(run_id).state == RunState.CANCELLED
        retried_again = client.retry(run_id, idempotency_key="another-retry-" + suffix)
        assert retried_again.reused and retried_again.run.run_id == successor.run_id
        with pytest.raises(ConflictError):
            client.retry(
                run_id,
                idempotency_key="different-retry-" + suffix,
                run_timeout_seconds=17,
            )

        page = client.list(limit=1)
        assert len(page.items) == 1 and page.next_cursor
        next_page = client.list(limit=1, cursor=page.next_cursor)
        assert len(next_page.items) == 1
        assert next_page.items[0].run_id != page.items[0].run_id
        with pytest.raises(InvalidArgumentError):
            foreign.list(cursor=page.next_cursor)
        with pytest.raises(InvalidArgumentError):
            client.list(state=RunState.FAILED, cursor=page.next_cursor)
        cancelled = client.list(state=RunState.CANCELLED)
        assert any(run.run_id == run_id for run in cancelled.items)
        assert all(run.state == RunState.CANCELLED for run in cancelled.items)

    # Raw requests exercise server decoding rather than only SDK validation.
    body = json.dumps(
        {
            "schema_version": 1,
            "ticket_id": ticket,
            "business_request_key": "strict-" + suffix,
            "profile_id": profile,
            "budget_batch_id": batch,
        },
        separators=(",", ":"),
    ).encode()
    malformed = [
        body[:-1] + b',"schema_version":1}',
        body[:-1] + b',"run_timeout_seconds":null}',
        body[:-1] + b',"run_timeout_seconds":1.0}',
        body[:-1] + b',"run_timeout_seconds":true}',
        body[:-1] + b',"Run_Timeout_Seconds":1}',
        body[:-1] + b',"tenant_id":"other"}',
        body + b"{}",
        body[:-1] + b',"extra":"\xff"}',
        b"{" + b" " * 4096 + b"}",
    ]
    with httpx.Client(
        base_url=url, headers={"Authorization": f"Bearer {key_a}"}
    ) as raw:
        for index, invalid in enumerate(malformed):
            response = raw.post(
                "/v2/runs",
                content=invalid,
                headers={
                    "Content-Type": "application/json",
                    "Idempotency-Key": f"invalid-{suffix}-{index}",
                },
            )
            assert response.status_code == 400
            assert response.json()["error"]["code"] == "INVALID_ARGUMENT"
    print(
        "PASS real HTTP Run SDK: identity, shared budgets, concurrent single retry, "
        "strict JSON, tenant/roles, metadata pages, result availability, trace, "
        "single HTTP exchange"
    )


if __name__ == "__main__":
    main()
