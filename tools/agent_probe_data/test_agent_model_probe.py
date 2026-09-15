"""Deterministic checks for the S0 probe guardrails, not model acceptance."""

import argparse
import asyncio
import importlib.util
import json
import time
from pathlib import Path

import httpx
import pytest

SPEC = importlib.util.spec_from_file_location(
    "agent_model_probe", Path(__file__).resolve().parents[1] / "agent_model_probe.py"
)
assert SPEC is not None and SPEC.loader is not None
PROBE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROBE)
CASE = json.loads(PROBE.DATA_PATH.read_text(encoding="utf-8"))["cases"][0]


def call(name: str, arguments: object) -> dict:
    """Build a deliberately synthetic model message for guardrail tests."""
    return {"tool_calls": [{"function": {"name": name, "arguments": arguments}}]}


@pytest.mark.parametrize(
    ("message", "error"),
    [
        (call("write_ticket", {}), "UNKNOWN_TOOL"),
        (call("get_order", {"order_id": 42}), "INVALID_ARGUMENTS"),
        (
            call("get_order", {"order_id": CASE["order_id"], "tenant": "x"}),
            "INVALID_ARGUMENTS",
        ),
        (
            call("get_order", {"order_id": "other-tenant-order"}),
            "OBJECT_NOT_AUTHORIZED",
        ),
        ({"tool_calls": [None]}, "INVALID_TOOL_ENVELOPE"),
        (
            {"tool_calls": [call("get_order", {}), call("get_delivery", {})]},
            "EXPECTED_SINGLE_TOOL",
        ),
    ],
)
def test_rejects_entire_invalid_call(message: dict, error: str) -> None:
    """Reject unknown tools, wrong shapes and unauthorized objects before dispatch."""
    with pytest.raises(PROBE.ProbeError, match=error):
        PROBE.validate_call(message, CASE)


@pytest.mark.parametrize(
    "value",
    [
        "https://api.example.com",
        "http://localhost:11435",
        "http://127.0.0.1:11435/api",
        "http://secret@127.0.0.1:11435",
    ],
)
def test_rejects_nonlocal_or_credential_origins(value: str) -> None:
    """The probe never accepts a remote paid service or embedded credentials."""
    with pytest.raises(argparse.ArgumentTypeError):
        PROBE.local_origin(value)


def test_final_requires_actual_evidence_and_accurate_values() -> None:
    """Schema validity does not silently convert incorrect decisions into success."""
    refs = {
        PROBE.fixture_result(name, CASE)["evidence_ref"]
        for name in PROBE.TOOL_ARGUMENTS
    }
    final = {
        "decision": CASE["expected_decision"],
        "order_id": CASE["order_id"],
        "delivery_status": CASE["delivery_status"],
        "evidence_refs": sorted(refs),
    }
    assert PROBE.validate_final(json.dumps(final), CASE, refs)
    final["decision"] = "record_only"
    assert not PROBE.validate_final(json.dumps(final), CASE, refs)
    with pytest.raises(PROBE.ProbeError, match="EVIDENCE"):
        PROBE.validate_final(json.dumps(final), CASE, set())


def test_response_bound_and_wall_deadline() -> None:
    """Bound actual async reads independently of a transport's per-read timeout."""

    async def oversized(_: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=b"x" * (PROBE.MAX_RESPONSE + 1))

    async def delayed(_: httpx.Request) -> httpx.Response:
        await asyncio.sleep(1)
        return httpx.Response(200, json={})

    async def check() -> None:
        async with httpx.AsyncClient(
            transport=httpx.MockTransport(oversized)
        ) as client:
            with pytest.raises(PROBE.ProbeError, match="RESPONSE_TOO_LARGE"):
                await PROBE.request_json(client, "GET", "http://127.0.0.1:1")
        async with httpx.AsyncClient(transport=httpx.MockTransport(delayed)) as client:
            with pytest.raises(TimeoutError):
                await PROBE.request_json(
                    client, "GET", "http://127.0.0.1:1", timeout=0.01
                )

    asyncio.run(check())


@pytest.mark.parametrize(
    "failure",
    [
        TimeoutError(),
        httpx.ReadTimeout("read"),
        httpx.WriteError("write"),
        httpx.ConnectError("connect"),
    ],
)
def test_chat_keeps_uncertain_request_reserved(failure: Exception) -> None:
    """Every failed transport conservatively retains one unknown physical call."""
    sent = 0

    async def transport(_: httpx.Request) -> httpx.Response:
        nonlocal sent
        sent += 1
        raise failure

    async def check() -> None:
        async with httpx.AsyncClient(
            base_url="http://127.0.0.1:1", transport=httpx.MockTransport(transport)
        ) as client:
            requests = []
            with pytest.raises(type(failure)):
                await PROBE.chat(client, "probe", [], requests, time.monotonic() + 1)
            assert len(requests) == 1
            assert requests[0]["completion_unknown"] is True
            assert requests[0]["usage_known"] is False

    asyncio.run(check())
    assert sent == 1


def test_unfinished_model_response_is_not_treated_as_completed() -> None:
    """A syntactically valid response without done=true cannot release the stop guard."""

    async def transport(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200, json={"message": {"role": "assistant", "content": "{}"}}
        )

    async def check() -> None:
        async with httpx.AsyncClient(
            base_url="http://127.0.0.1:1", transport=httpx.MockTransport(transport)
        ) as client:
            requests = []
            with pytest.raises(PROBE.ProbeError, match="INCOMPLETE_MODEL_RESPONSE"):
                await PROBE.chat(client, "probe", [], requests, time.monotonic() + 1)
            assert requests[0]["completion_unknown"] is True

    asyncio.run(check())


@pytest.mark.parametrize(
    ("body", "error", "uncertain"),
    [
        (b"not json", "INVALID_RESPONSE_JSON", True),
        (b"[]", "INVALID_RESPONSE_ENVELOPE", True),
        (b"x" * (PROBE.MAX_RESPONSE + 1), "RESPONSE_TOO_LARGE", True),
        (b'{"done":true,"message":[]}', "INVALID_ASSISTANT_ENVELOPE", False),
    ],
    ids=[
        "invalid-json",
        "invalid-envelope",
        "oversized-body",
        "finished-invalid-message",
    ],
)
def test_response_completion_is_separate_from_protocol_validity(
    body: bytes, error: str, uncertain: bool
) -> None:
    """Parsing and size failures remain unknown; a finished bad message does not."""

    async def transport(_: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=body)

    async def check() -> None:
        async with httpx.AsyncClient(
            base_url="http://127.0.0.1:1", transport=httpx.MockTransport(transport)
        ) as client:
            requests = []
            with pytest.raises(PROBE.ProbeError, match=error):
                await PROBE.chat(client, "probe", [], requests, time.monotonic() + 1)
            assert requests[0]["completion_unknown"] is uncertain

    asyncio.run(check())


def install_stage_fixtures(
    monkeypatch: pytest.MonkeyPatch,
    output: Path,
    failure_stage: str | None,
    status_fails: bool,
    protocol_fails: bool = False,
) -> tuple[argparse.Namespace, list[str], list[str]]:
    """Install deterministic stage results while observing orchestration and disk order."""
    stages: list[str] = []
    samples: list[str] = []

    def result(stage: str) -> dict:
        stages.append(stage)
        uncertain = stage == failure_stage
        row = {
            "passed": not uncertain and not protocol_fails,
            "requests": [
                {
                    "number": 1,
                    "usage_known": not uncertain,
                    "completion_unknown": uncertain,
                }
            ],
        }
        if uncertain:
            row["error"] = "TimeoutError"
        elif protocol_fails:
            row["error"] = "INVALID_FINAL_JSON"
        return row

    async def metadata(_client: object, _method: str, path: str, **_: object) -> dict:
        if path == "/api/version":
            return {"version": "test-fixture"}
        if path == "/api/tags":
            return {"models": [{"name": "probe", "digest": "fixture-digest"}]}
        assert path == "/api/ps"
        # This read must already contain the just-finished case, even when ps fails.
        saved = json.loads(output.read_text(encoding="utf-8"))
        assert len(saved["cases"]) == len(stages)
        assert saved["cases"][-1]["requests"][0]["number"] == 1
        samples.append(stages[-1])
        if status_fails:
            raise httpx.ReadError("synthetic status failure")
        return {"models": []}

    async def case(_client: object, _model: str, case: dict) -> dict:
        return result(f"case:{case['id']}")

    async def structured(_client: object, _model: str, _case: dict) -> dict:
        return result("structured_output")

    async def correction(
        _client: object, _model: str, _case: dict, malformed: bool
    ) -> dict:
        return result(
            "correction:malformed" if malformed else "correction:unknown_tool"
        )

    monkeypatch.setattr(PROBE, "request_json", metadata)
    monkeypatch.setattr(PROBE, "run_case", case)
    monkeypatch.setattr(PROBE, "structured_probe", structured)
    monkeypatch.setattr(PROBE, "correction_probe", correction)
    return (
        argparse.Namespace(
            origin="http://127.0.0.1:1",
            model="probe",
            digest="fixture-digest",
            output=output,
        ),
        stages,
        samples,
    )


@pytest.mark.parametrize(
    "failure_stage",
    [
        "case:delayed",
        "structured_output",
        "correction:unknown_tool",
        "correction:malformed",
    ],
)
def test_every_uncertain_stage_is_saved_and_stops_next_inference(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, failure_stage: str
) -> None:
    """Persist timeout evidence from any phase without sending a later model request."""
    output = tmp_path / "evidence.json"
    args, stages, samples = install_stage_fixtures(
        monkeypatch, output, failure_stage, True
    )
    assert asyncio.run(PROBE.run(args)) == 1
    order = [
        "case:delayed",
        "case:delivered",
        "case:injected_tool_note",
        "structured_output",
        "correction:unknown_tool",
        "correction:malformed",
    ]
    assert stages == order[: order.index(failure_stage) + 1]
    saved = json.loads(output.read_text(encoding="utf-8"))
    assert saved["stopped_after_uncertain_backend_completion"] is True
    assert saved["stopped_at"] == failure_stage
    assert saved["all_passed"] is False
    rows = saved["cases"] + saved.get("corrections", [])
    if "structured_output" in saved:
        rows.append(saved["structured_output"])
    assert sum(row["requests"][0]["completion_unknown"] for row in rows) == 1
    if failure_stage == "case:delayed":
        assert samples == []


@pytest.mark.parametrize("protocol_fails", [False, True])
def test_status_failure_preserves_results_and_does_not_change_inference_outcome(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, protocol_fails: bool
) -> None:
    """Optional ps failures cannot erase either success or known protocol failure."""
    output = tmp_path / "evidence.json"
    args, stages, samples = install_stage_fixtures(
        monkeypatch, output, None, True, protocol_fails
    )
    assert asyncio.run(PROBE.run(args)) == int(protocol_fails)
    saved = json.loads(output.read_text(encoding="utf-8"))
    assert len(stages) == 6
    assert len(samples) == 3
    assert saved["all_passed"] is not protocol_fails
    assert len(saved["cases"]) == 3
    assert len(saved["corrections"]) == 2
    assert [row["error"] for row in saved["observation_errors"]] == ["ReadError"] * 3
    assert "stopped_after_uncertain_backend_completion" not in saved
