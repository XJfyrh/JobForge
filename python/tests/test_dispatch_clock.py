"""Exercise the production Linux clock with real TCP and synthetic business data."""

import asyncio
import json
import sys
import time
from pathlib import Path
from typing import Any

import pytest
from http_fault_server import HTTPFaultServer, ReceivedRequest, respond
from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    DispatchError,
    Endpoint,
    RunCallContext,
    boottime_ms,
    prepare_request,
)

Frame = dict[str, Any]


class Coordinator:
    """Issue synthetic permits; this is not a durable PostgreSQL reservation."""

    def __init__(self) -> None:
        """Keep the observations confirmed by this test coordinator."""
        self.observations: list[Frame] = []

    async def authorize(self, intent: Frame) -> Frame:
        """Grant one short permit for the synthetic business read."""
        return {
            **intent,
            "kind": "call_permit",
            "emitted_mono_ms": boottime_ms(),
            "physical_call_id": "00000000-0000-4000-8000-000000000099",
            "granted": True,
            "error_code": "",
            "dispatch_ms": 1000,
            "call_ms": 1000,
            "input_token_limit": 0,
            "output_token_limit": 0,
        }

    async def observe(self, observation: Frame) -> None:
        """Record the observation without simulating durable persistence."""
        self.observations.append(observation)

    async def settle(self, report: Frame) -> Frame:
        """Reject metering for this free business request."""
        raise AssertionError("free business reads cannot report paid usage")


def test_production_clock_and_default_dispatcher_over_real_tcp() -> None:
    """Linux must use its real BOOTTIME; other platforms explicitly refuse it."""
    if sys.platform != "linux":
        with pytest.raises(DispatchError, match="PROFILE_UNAVAILABLE"):
            boottime_ms()
        return

    clock_id = time.CLOCK_BOOTTIME
    before = time.clock_gettime_ns(clock_id) // 1_000_000
    observed = boottime_ms()
    after = time.clock_gettime_ns(clock_id) // 1_000_000
    assert before <= observed <= after

    async def run() -> None:
        async def handler(
            _reader: asyncio.StreamReader,
            writer: asyncio.StreamWriter,
            _request: ReceivedRequest,
        ) -> None:
            await respond(writer, body=b'{"synthetic":true}')

        source = Path(__file__).resolve().parents[2]
        fixture = source / "api/executor/v2/fixtures/frames.json"
        frame: Frame = json.loads(fixture.read_text(encoding="utf-8"))["valid_frames"][
            0
        ]
        frame["binding"]["step_kind"] = "get_order"
        frame["emitted_mono_ms"] = boottime_ms()
        frame["remaining_ms"] = 5000
        binding = frame["binding"]
        context = RunCallContext(
            binding["profile_hash"],
            binding["snapshot_hash"],
            "00000000-0000-4000-8000-000000000088",
        )
        hooks = Coordinator()
        async with HTTPFaultServer(handler) as server:
            dispatcher = AuthorizedDispatcher(
                frame,
                hooks=hooks,
                endpoints={"business": Endpoint(server.origin)},
            )
            try:
                request = prepare_request(
                    context=context,
                    endpoint="business",
                    subcall="get_order",
                    method="GET",
                    path=f"/business/v1/snapshots/{binding['snapshot_id']}/order",
                    max_response_bytes=8192,
                )
                result = await dispatcher.execute(
                    request,
                    context=context,
                    validate=lambda response: json.loads(response.body),
                )
                assert result == {"synthetic": True}
                assert len(server.requests) == len(hooks.observations) == 1
                assert hooks.observations[0]["business_outcome"] == "accepted"
                assert not dispatcher.closed
            finally:
                await dispatcher.aclose()

    asyncio.run(asyncio.wait_for(run(), timeout=10))
