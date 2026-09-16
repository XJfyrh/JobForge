"""Authorized tool boundaries use synthetic responses, never model acceptance."""

import asyncio
import copy
import hashlib
import json
from dataclasses import replace
from pathlib import Path
from typing import Any, cast
from uuid import uuid4

import pytest
from http_fault_server import HTTPFaultServer, ReceivedRequest, respond
from jobforge_agent import dispatch as agent_dispatch
from jobforge_agent import run_tools as agent_run_tools
from jobforge_agent.business import SnapshotBinding
from jobforge_agent.dispatch import (
    AuthorizedDispatcher,
    DispatchError,
    Endpoint,
    RunCallContext,
)
from jobforge_agent.embedding import MODEL, MODEL_DIGEST, embedding_body, vector
from jobforge_agent.errors import ToolError
from jobforge_agent.protocol_v2 import Conversation, observation_hash, usage_hash
from jobforge_agent.run_tools import RunBusinessTools

SNAPSHOT = "535cf2ed-cdba-4d8c-a576-a41c865535da"
INDEX = "f9313cc0-4ed7-4ce6-8da8-206a75dd65d0"
BINDING = SnapshotBinding(SNAPSHOT, "order-1", "d" * 64, INDEX, "policy-v1")
CONTEXT = RunCallContext("a" * 64, "b" * 64, "535cf2ed-cdba-4d8c-a576-a41c865535db")
SYNTHETIC_VECTOR = [1.0] + [0.0] * 383
Frame = dict[str, Any]


def _execute_step(name: str) -> Frame:
    path = Path(__file__).resolve().parents[2] / "api/executor/v2/fixtures/frames.json"
    frame: Frame = json.loads(path.read_text(encoding="utf-8"))["valid_frames"][0]
    frame["binding"].update(step_kind=name, snapshot_id=SNAPSHOT)
    frame["emitted_mono_ms"] = 1000
    return frame


class SyntheticHooks:
    """Exercise the v2 peer state with fake permission, never a durable budget."""

    def __init__(self, start: Frame, *, settlement: str = "settled") -> None:
        """Track every synthetic permission and accepted/rejected observation."""
        self.intents: list[Frame] = []
        self.observations: list[Frame] = []
        self.reports: list[Frame] = []
        self.settlement = settlement
        self.conversation = Conversation()
        self.conversation.accept(start, 1000)

    async def authorize(self, intent: Frame) -> Frame:
        """Return one new permit after checking its exact v2 sequence."""
        self.conversation.accept(intent, 1000)
        self.intents.append(copy.deepcopy(intent))
        permit = copy.deepcopy(intent)
        permit.update(
            kind="call_permit",
            physical_call_id=str(uuid4()),
            granted=True,
            error_code="",
            dispatch_ms=1000,
            call_ms=10000,
            input_token_limit=1000 if intent["subcall"] == "query_embedding" else 0,
            output_token_limit=0,
        )
        self.conversation.accept(permit, 1000)
        # The fake peer treats dispatch as possibly sent, then independently
        # compares this expectation to the real TCP server's captured requests.
        self.conversation.can_dispatch(permit["physical_call_id"], 1000)
        return permit

    async def observe(self, observation: Frame) -> Frame:
        """Confirm only a standard decoded observation from the same sequence."""
        self.conversation.accept(observation, 1000)
        self.observations.append(copy.deepcopy(observation))
        ack = {
            "version": 2,
            "kind": "call_observation_ack",
            "request_id": observation["request_id"],
            "binding": copy.deepcopy(observation["binding"]),
            "emitted_mono_ms": 1000,
            "call_sequence": observation["call_sequence"],
            "physical_call_id": observation["physical_call_id"],
            "observation_hash": observation_hash(observation),
        }
        self.conversation.accept(ack, 1000)
        return ack

    async def settle(self, report: Frame) -> Frame:
        """Return a synthetic same-call ACK after the real usage join checks."""
        self.conversation.accept_metering(report, 1000)
        self.reports.append(copy.deepcopy(report))
        ack = {
            key: copy.deepcopy(report[key])
            for key in (
                "version",
                "request_id",
                "binding",
                "emitted_mono_ms",
                "call_sequence",
                "physical_call_id",
            )
        }
        ack.update(
            kind="metering_ack",
            report_hash=report["report_hash"],
            settlement=(
                "anomaly" if report["usage"]["input_tokens"] > 1000 else self.settlement
            ),
        )
        self.conversation.accept_metering(ack, 1000)
        return ack


def _payload(path: str) -> dict[str, Any]:
    if path == "/api/version":
        return {"version": "0.32.5"}
    if path == "/api/tags":
        return {"models": [{"name": MODEL, "digest": MODEL_DIGEST}]}
    if path == "/api/embed":
        return {
            "model": MODEL,
            "embeddings": [SYNTHETIC_VECTOR],
            "prompt_eval_count": 7,
        }
    if path.endswith("/policies/search"):
        return {
            "snapshot_id": SNAPSHOT,
            "matches": [
                {
                    "index_id": INDEX,
                    "chunk_id": "P01.1",
                    "policy_version": "policy-v1",
                    "evidence_ref": f"business-policy:{INDEX}:P01.1",
                    "text": "Synthetic policy evidence.",
                    "source": "P01.md",
                    "distance": 0.0,
                }
            ],
        }
    kind = path.rsplit("/", 1)[-1]
    assert kind in {"order", "delivery"}
    return {
        "snapshot_id": SNAPSHOT,
        "evidence_ref": f"business-evidence:{SNAPSHOT}:{kind}",
        "kind": kind,
        "missing": False,
        kind: {"order_id": "order-1"},
    }


def _mutate(payload: dict[str, Any], path: str, failure: str) -> None:
    if path == "/api/version" and failure == "version":
        payload["version"] = "unregistered"
    elif path == "/api/tags" and failure == "digest":
        payload["models"][0]["digest"] = "0" * 64
    elif path == "/api/tags" and failure == "duplicate_model":
        payload["models"].append(copy.deepcopy(payload["models"][0]))
    elif path == "/api/embed":
        if failure == "model":
            payload["model"] = "unregistered"
        elif failure == "dimensions":
            payload["embeddings"] = [[1.0]]
        elif failure == "vector_underflow":
            payload["embeddings"] = [[1e-23] * 384]
        elif failure == "vector_overflow":
            payload["embeddings"] = [[1e18] * 384]
        elif failure == "vector_norm_bound":
            payload["embeddings"] = [[1.31e19] + [0.0] * 383]
        elif failure == "batch_count":
            payload["embeddings"].append(SYNTHETIC_VECTOR)
        elif failure == "usage_missing":
            payload.pop("prompt_eval_count")
        elif failure == "usage_boolean":
            payload["prompt_eval_count"] = True
        elif failure == "usage_negative":
            payload["prompt_eval_count"] = -1
        elif failure == "usage_unsafe":
            payload["prompt_eval_count"] = 2**53
        elif failure == "usage_overrun":
            payload["prompt_eval_count"] = 1001
    elif path.endswith("/policies/search"):
        if failure == "snapshot":
            payload["snapshot_id"] = str(uuid4())
        elif failure in {"index_id", "policy_version", "evidence_ref"}:
            payload["matches"][0][failure] = "wrong"
        elif failure == "duplicate_chunk":
            payload["matches"].append(copy.deepcopy(payload["matches"][0]))
        elif failure == "paragraph_size":
            payload["matches"][0]["text"] = "x" * 769
    elif "/business/" in path:
        kind = path.rsplit("/", 1)[-1]
        if failure == "snapshot":
            payload["snapshot_id"] = str(uuid4())
        elif failure == "evidence_ref":
            payload["evidence_ref"] = "wrong"
        elif failure == "order_binding":
            payload[kind]["order_id"] = "other-order"
        elif failure == "missing_with_fact":
            payload["missing"] = True
        elif failure == "wrong_kind":
            payload["kind"] = "ticket"
        elif failure == "missing_order":
            payload["missing"], payload[kind] = True, None


async def _case(
    name: str,
    *,
    failure: str = "",
    settlement: str = "settled",
    expect_failure: bool = False,
    arguments: Any = None,
    context: RunCallContext = CONTEXT,
    binding: SnapshotBinding = BINDING,
) -> tuple[list[ReceivedRequest], SyntheticHooks, tuple[Frame, ...]]:
    async def handler(
        _reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
        request: ReceivedRequest,
    ) -> None:
        payload = _payload(request.path)
        _mutate(payload, request.path, failure)
        await respond(writer, body=json.dumps(payload).encode("utf-8"))

    async with asyncio.timeout(5), HTTPFaultServer(handler) as server:
        # Only the test changes the fixed local model endpoint allowlist.
        with pytest.MonkeyPatch.context() as patch:
            patch.setattr(agent_dispatch, "OLLAMA_ORIGINS", (server.origin,))
            start = _execute_step(name)
            hooks = SyntheticHooks(start, settlement=settlement)
            dispatcher = AuthorizedDispatcher(
                start,
                hooks=hooks,
                endpoints={
                    "business": Endpoint(server.origin, bearer_key="synthetic-key"),
                    "ollama": Endpoint(server.origin),
                },
                clock=lambda: 1000,
            )
            tools = RunBusinessTools(dispatcher, binding)
            if arguments is None:
                arguments = (
                    {"query": "synthetic policy query"}
                    if name == "search_policy"
                    else {"order_id": "order-1"}
                )
            try:
                if expect_failure:
                    with pytest.raises(DispatchError):
                        await tools.execute(name, arguments, context=context)
                else:
                    result = await tools.execute(name, arguments, context=context)
                    assert result["snapshot_id"] == SNAPSHOT
            finally:
                await dispatcher.aclose()
            return list(server.requests), hooks, dispatcher.recorded_usage()


def _fingerprint(*fields: str) -> str:
    digest = hashlib.sha256()
    for value in fields:
        raw = value.encode("utf-8")
        digest.update(len(raw).to_bytes(8, "big"))
        digest.update(raw)
    return digest.hexdigest()


@pytest.mark.parametrize("settlement", ["settled", "anomaly"])
def test_embedding_cached_tokens_remain_reported_but_cannot_continue(
    monkeypatch: pytest.MonkeyPatch,
    settlement: str,
) -> None:
    """A structurally valid anomalous embedding report cannot reach policy HTTP."""
    original = agent_run_tools._embedding_usage

    def cached(response: Any) -> Any:
        evidence = original(response)
        assert evidence is not None
        return replace(evidence, cached_input_tokens=1)

    monkeypatch.setattr(agent_run_tools, "_embedding_usage", cached)
    requests, hooks, reports = asyncio.run(
        _case("search_policy", settlement=settlement, expect_failure=True)
    )
    assert len(requests) == len(hooks.intents) == 3
    assert len(reports) == 1 and reports[0]["provider_audit"] is None
    assert reports[0]["usage"]["cached_input_tokens"] == 1
    assert len(hooks.observations) == 2


@pytest.mark.parametrize(
    "value",
    [
        [1e-23] * 384,
        [1e18] * 384,
        [1.31e19] + [0.0] * 383,
        [3e38] + [0.0] * 383,
    ],
)
def test_embedding_rejects_float32_squared_norm_failures(value: Any) -> None:
    """Reject component underflow, accumulated overflow and Go's norm bound."""
    with pytest.raises(ToolError, match="INVALID_VECTOR"):
        vector(value)


@pytest.mark.parametrize("component", [1e-22, 1.0, 1e19])
def test_embedding_accepts_go_float32_norm_range(component: float) -> None:
    """Keep the original finite components when their float32 norm is usable."""
    values = [component] + [0.0] * 383
    assert vector(values) == values


def test_embedding_input_snapshot_is_independent_of_caller_list() -> None:
    """Identity probes cannot allow caller mutation to change the prepared batch."""
    texts = ["synthetic policy query"]
    body = embedding_body(texts)
    texts[0] = "changed query"
    assert body["input"] == ["synthetic policy query"]


@pytest.mark.parametrize("name", ["get_order", "get_delivery"])
def test_read_tool_uses_one_real_request_without_fake_zero_usage(name: str) -> None:
    """Validate the bound object before its one accepted observation."""
    requests, hooks, usage = asyncio.run(_case(name))
    kind = "order" if name == "get_order" else "delivery"
    assert [request.path for request in requests] == [
        f"/business/v1/snapshots/{SNAPSHOT}/{kind}"
    ]
    assert requests[0].body == b""
    assert requests[0].headers["authorization"] == "Bearer synthetic-key"
    assert len(hooks.intents) == len(hooks.observations) == 1
    assert hooks.observations[0]["business_outcome"] == "accepted"
    assert hooks.observations[0]["usage_disposition"] == "unknown"
    assert usage == () and hooks.reports == []


def test_search_uses_four_validated_http_calls_and_separate_run_hashes() -> None:
    """Hash actual bytes with the Run profile, preserving metadata/usage ordering."""
    requests, hooks, usage = asyncio.run(_case("search_policy"))
    assert [request.path for request in requests] == [
        "/api/version",
        "/api/tags",
        "/api/embed",
        f"/business/v1/snapshots/{SNAPSHOT}/policies/search",
    ]
    assert len(hooks.observations) == 4
    assert [frame["business_outcome"] for frame in hooks.observations] == [
        "accepted"
    ] * 4
    assert [frame["usage_disposition"] for frame in hooks.observations] == [
        "unknown",
        "unknown",
        "reported",
        "unknown",
    ]
    assert all("authorization" not in request.headers for request in requests[:3])
    assert requests[3].headers["authorization"] == "Bearer synthetic-key"
    assert json.loads(requests[2].body) == {
        "model": MODEL,
        "input": ["synthetic policy query"],
        "truncate": False,
    }
    assert json.loads(requests[3].body) == {
        "embedding_model": MODEL,
        "embedding_digest": MODEL_DIGEST,
        "query_vector": SYNTHETIC_VECTOR,
    }
    assert len(usage) == len(hooks.reports) == 1
    assert usage[0]["usage"]["input_tokens"] == 7
    assert usage[0]["usage"]["output_tokens"] == 0
    assert usage[0]["usage"]["usage_hash"] == usage_hash(usage[0]["usage"])
    assert CONTEXT.run_profile_hash != BINDING.profile_hash
    for request, intent in zip(requests, hooks.intents, strict=True):
        expected = _fingerprint(
            "jobforge.run.physical-input.v1",
            CONTEXT.run_profile_hash,
            CONTEXT.snapshot_content_hash,
            intent["subcall"],
            request.method,
            request.path,
            hashlib.sha256(request.body).hexdigest(),
        )
        assert intent["parameter_hash"] == expected


@pytest.mark.parametrize(
    ("failure", "calls"),
    [
        ("version", 1),
        ("digest", 2),
        ("duplicate_model", 2),
        ("model", 3),
        ("dimensions", 3),
        ("batch_count", 3),
        ("vector_underflow", 3),
        ("vector_overflow", 3),
        ("vector_norm_bound", 3),
    ],
)
def test_invalid_embedding_stage_never_accepts_or_authorizes_next_http(
    failure: str, calls: int
) -> None:
    """Version, digest and float32 vector validation precede accepted status."""
    requests, hooks, usage = asyncio.run(
        _case("search_policy", failure=failure, expect_failure=True)
    )
    assert len(requests) == len(hooks.intents) == calls
    assert (
        sum(item["business_outcome"] == "accepted" for item in hooks.observations)
        == calls - 1
    )
    if failure in {
        "dimensions",
        "batch_count",
        "vector_underflow",
        "vector_overflow",
        "vector_norm_bound",
    }:
        assert len(usage) == 1 and usage[0]["usage"]["input_tokens"] == 7
    else:
        assert usage == ()


@pytest.mark.parametrize("name", ["get_order", "get_delivery"])
@pytest.mark.parametrize(
    "failure",
    ["snapshot", "evidence_ref", "order_binding", "missing_with_fact", "wrong_kind"],
)
def test_read_response_binding_failure_is_rejected(name: str, failure: str) -> None:
    """A complete HTTP 200 cannot accept a different object or evidence ref."""
    requests, hooks, usage = asyncio.run(
        _case(name, failure=failure, expect_failure=True)
    )
    assert len(requests) == len(hooks.intents) == 1
    assert all(item["business_outcome"] != "accepted" for item in hooks.observations)
    assert usage == ()


@pytest.mark.parametrize(
    "failure",
    [
        "snapshot",
        "index_id",
        "policy_version",
        "evidence_ref",
        "duplicate_chunk",
        "paragraph_size",
    ],
)
def test_search_evidence_is_checked_before_last_accepted(failure: str) -> None:
    """A different frozen index or policy must fail after exactly four HTTP calls."""
    requests, hooks, usage = asyncio.run(
        _case("search_policy", failure=failure, expect_failure=True)
    )
    assert len(requests) == len(hooks.intents) == 4
    assert (
        sum(item["business_outcome"] == "accepted" for item in hooks.observations) == 3
    )
    assert len(usage) == 1


@pytest.mark.parametrize(
    "failure", ["usage_missing", "usage_boolean", "usage_negative", "usage_unsafe"]
)
def test_unobserved_embedding_usage_stays_unknown(failure: str) -> None:
    """Absent or invalid local counts cannot become a fabricated zero report."""
    requests, hooks, usage = asyncio.run(_case("search_policy", failure=failure))
    assert len(requests) == 4
    assert all(item["usage_disposition"] == "unknown" for item in hooks.observations)
    assert all(item["audit_hash"] is None for item in hooks.observations)
    assert all(item["business_outcome"] == "accepted" for item in hooks.observations)
    assert usage == () and hooks.reports == []


@pytest.mark.parametrize("condition", ["usage_overrun", "settlement_unconfirmed"])
def test_metering_stop_prevents_search_permission(condition: str) -> None:
    """Retain original complete usage while barring the fourth physical call."""
    requests, hooks, usage = asyncio.run(
        _case(
            "search_policy",
            failure="usage_overrun" if condition == "usage_overrun" else "",
            settlement="unconfirmed"
            if condition == "settlement_unconfirmed"
            else "settled",
            expect_failure=True,
        )
    )
    assert len(requests) == len(hooks.intents) == 3
    assert len(usage) == len(hooks.reports) == 1
    assert usage[0]["usage"]["input_tokens"] == (
        1001 if condition == "usage_overrun" else 7
    )


@pytest.mark.parametrize(
    ("name", "arguments"),
    [
        ("get_order", {"order_id": "wrong"}),
        ("get_delivery", {"order_id": 42}),
        ("get_order", {"order_id": "order-1", "tenant": "other"}),
        ("search_policy", {"query": "x", "top_k": 5}),
        ("search_policy", {"query": "中" * 171}),
        ("search_policy", {"query": "\ud800"}),
    ],
)
def test_run_arguments_fail_before_any_permission(name: str, arguments: Any) -> None:
    """Model arguments cannot choose an object or enlarge the fixed search."""
    requests, hooks, usage = asyncio.run(
        _case(name, arguments=arguments, expect_failure=True)
    )
    assert requests == [] and hooks.intents == [] and usage == ()


@pytest.mark.parametrize("mismatch", ["run_profile", "snapshot_hash", "snapshot_id"])
def test_binding_mismatch_cannot_spend_a_permission(mismatch: str) -> None:
    """Index profile hashes and different snapshots are never execution identity."""
    context = RunCallContext(
        BINDING.profile_hash if mismatch == "run_profile" else CONTEXT.run_profile_hash,
        "e" * 64 if mismatch == "snapshot_hash" else CONTEXT.snapshot_content_hash,
        CONTEXT.tool_invocation_id,
    )
    binding = SnapshotBinding(
        str(uuid4()) if mismatch == "snapshot_id" else SNAPSHOT,
        BINDING.order_id,
        BINDING.profile_hash,
        INDEX,
        BINDING.policy_version,
    )
    requests, hooks, usage = asyncio.run(
        _case("get_order", context=context, binding=binding, expect_failure=True)
    )
    assert requests == [] and hooks.intents == [] and usage == ()


@pytest.mark.parametrize("name", ["get_order", "get_delivery"])
def test_no_associated_order_preserves_explicit_missing_result(name: str) -> None:
    """Missing authorized objects stay evidence, without fabricating a fact."""
    binding = SnapshotBinding(SNAPSHOT, None, BINDING.profile_hash, INDEX, "policy-v1")
    requests, hooks, usage = asyncio.run(
        _case(
            name, failure="missing_order", binding=binding, arguments={"order_id": None}
        )
    )
    assert len(requests) == 1
    assert hooks.observations[0]["business_outcome"] == "accepted"
    assert usage == ()


@pytest.mark.parametrize(
    ("call_sequence", "usage_observed"),
    [(1, True), (2, True), (3, True), (4, True), (3, False)],
)
@pytest.mark.parametrize(
    "confirmation",
    [
        "before_confirmation",
        "lost_ack",
        "bad_hash",
        "wrong_binding",
        "wrong_call",
        "wrong_sequence",
        "missing_frame",
        "deadline",
        "stop",
        "cancel",
        "confirmed",
    ],
)
def test_search_http_and_final_return_wait_for_observation_ack(
    monkeypatch: pytest.MonkeyPatch,
    call_sequence: int,
    usage_observed: bool,
    confirmation: str,
) -> None:
    """Real TCP waits for free, settled and final ACKs from a synthetic peer."""

    async def run() -> None:
        async def handler(
            _reader: asyncio.StreamReader,
            writer: asyncio.StreamWriter,
            request: ReceivedRequest,
        ) -> None:
            payload = _payload(request.path)
            if request.path == "/api/embed" and not usage_observed:
                payload.pop("prompt_eval_count")
            await respond(writer, body=json.dumps(payload).encode())

        now = [1000]
        reached, release = asyncio.Event(), asyncio.Event()
        start = _execute_step("search_policy")
        hooks = SyntheticHooks(start)
        observe = hooks.observe

        async def intercept(observation: Frame) -> Frame:
            if observation["call_sequence"] != call_sequence:
                return await observe(observation)
            if confirmation == "before_confirmation":
                reached.set()
                await release.wait()
            ack = await observe(observation)
            reached.set()
            if confirmation in {"lost_ack", "deadline", "stop", "cancel", "confirmed"}:
                await release.wait()
            if confirmation == "bad_hash":
                ack["observation_hash"] = "e" * 64
            elif confirmation == "wrong_binding":
                ack["binding"]["fencing_token"] += 1
            elif confirmation == "wrong_call":
                ack["physical_call_id"] = str(uuid4())
            elif confirmation == "wrong_sequence":
                ack["call_sequence"] += 1
            elif confirmation == "missing_frame":
                # Exercise an old hook returning None, never treat it as proof.
                return cast(Frame, None)
            return ack

        monkeypatch.setattr(hooks, "observe", intercept)
        async with asyncio.timeout(5), HTTPFaultServer(handler) as server:
            monkeypatch.setattr(agent_dispatch, "OLLAMA_ORIGINS", (server.origin,))
            dispatcher = AuthorizedDispatcher(
                start,
                hooks=hooks,
                endpoints={
                    "business": Endpoint(server.origin),
                    "ollama": Endpoint(server.origin),
                },
                clock=lambda: now[0],
            )
            tools = RunBusinessTools(dispatcher, BINDING)
            arguments = {"query": "synthetic policy query"}
            task = asyncio.create_task(
                tools.execute("search_policy", arguments, context=CONTEXT)
            )
            try:
                await reached.wait()
                assert len(server.requests) == len(hooks.intents) == call_sequence
                if confirmation in {
                    "before_confirmation",
                    "lost_ack",
                    "deadline",
                    "stop",
                    "cancel",
                    "confirmed",
                }:
                    assert not task.done()
                    if confirmation == "confirmed":
                        release.set()
                    elif confirmation == "stop":
                        dispatcher.stop()
                    elif confirmation == "cancel":
                        task.cancel()
                    else:
                        # Jump exactly to the original call deadline; no control
                        # wait may restart it, including a lost synthetic ACK.
                        now[0] = 11000
                if confirmation == "confirmed":
                    result = await task
                    assert result["snapshot_id"] == SNAPSHOT
                    assert len(server.requests) == len(hooks.intents) == 4
                    assert not dispatcher.closed
                    if not usage_observed:
                        assert dispatcher.recorded_usage() == () and hooks.reports == []
                        assert hooks.observations[2]["usage_disposition"] == "unknown"
                        assert hooks.observations[2]["audit_hash"] is None
                    return
                with pytest.raises((DispatchError, asyncio.CancelledError)) as error:
                    await task
                if isinstance(error.value, DispatchError):
                    assert error.value.__context__ is error.value.__cause__ is None
                    expected = (
                        "TIMEOUT"
                        if confirmation
                        in {"before_confirmation", "lost_ack", "deadline"}
                        else "STOP_REQUESTED"
                        if confirmation == "stop"
                        else "PROTOCOL_ERROR"
                    )
                    assert error.value.code == expected
                assert dispatcher.closed
                assert len(dispatcher.recorded_usage()) == (
                    call_sequence >= 3 and usage_observed
                )
                with pytest.raises(DispatchError):
                    await tools.execute("search_policy", arguments, context=CONTEXT)
                assert len(server.requests) == len(hooks.intents) == call_sequence
            finally:
                await dispatcher.aclose()
                if not task.done():
                    task.cancel()
                await asyncio.gather(task, return_exceptions=True)

    asyncio.run(run())
