"""Strict executor v2 codec and independent bounded metering protocol.

The caller owns process supervision, lease renewal, monotonic clock injection and
trusted profile resolution. Passing validation alone never grants Run authority.
"""

import copy
import hashlib
import json
import math
import re
from typing import Any, BinaryIO

MAX_FRAME_BYTES = 384 * 1024
MAX_METERING_FRAME_BYTES = 8 * 1024
MAX_CHECKPOINT_BYTES = 256 * 1024
MAX_INTEGER = 2**53 - 1
Frame = dict[str, Any]

_UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_KEY = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
_HASH = re.compile(r"[0-9a-f]{64}")
_COMMON = {"version", "kind", "request_id", "binding", "emitted_mono_ms"}
_KINDS = {
    "execute_step": {"remaining_ms", "trace_context", "checkpoint", "input"},
    "call_intent": {"call_sequence", "subcall", "parameter_hash", "tool_invocation_id"},
    "call_permit": {
        "call_sequence",
        "subcall",
        "parameter_hash",
        "tool_invocation_id",
        "physical_call_id",
        "granted",
        "error_code",
        "dispatch_ms",
        "call_ms",
        "input_token_limit",
        "output_token_limit",
    },
    "call_observation": {
        "call_sequence",
        "physical_call_id",
        "transport_outcome",
        "http_status",
        "business_outcome",
        "error_code",
        "usage_disposition",
        "usage_hash",
    },
    "step_result": {"outcome", "error_code", "result"},
    "metering_report": {"call_sequence", "physical_call_id", "parameter_hash", "usage"},
    "metering_ack": {"call_sequence", "physical_call_id", "usage_hash", "settlement"},
}
_METERING_KINDS = {"metering_report", "metering_ack"}
_BINDING = {
    "tenant_id",
    "worker_id",
    "run_id",
    "step_id",
    "session_id",
    "profile_id",
    "profile_hash",
    "snapshot_id",
    "snapshot_hash",
    "input_hash",
    "attempt_no",
    "fencing_token",
    "cursor_version",
    "step_sequence",
    "step_kind",
}
_USAGE = {
    "input_tokens",
    "output_tokens",
    "cached_input_tokens",
    "receipt_hash",
    "usage_hash",
}
_STEPS = {
    "read_ticket",
    "get_order",
    "get_delivery",
    "search_policy",
    "model_proposal",
    "protocol_correction",
    "submit_proposal",
}
_ERRORS = {
    "",
    "BUDGET_EXHAUSTED",
    "STOP_REQUESTED",
    "STALE_LEASE",
    "PROFILE_UNAVAILABLE",
    "DEPENDENCY_UNAVAILABLE",
    "PROTOCOL_ERROR",
    "CALL_CONFLICT",
    "OUTPUT_INVALID",
    "INPUT_INVALID",
    "TIMEOUT",
}


class ProtocolError(ValueError):
    """Expose only a fixed code, never rejected protocol content or provider text."""

    def __init__(self, code: str = "PROTOCOL_ERROR") -> None:
        """Construct a sanitized error suitable for bounded diagnostics."""
        self.code = code
        super().__init__(code)


def _require(condition: bool) -> None:
    if not condition:
        raise ProtocolError()


def _pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        _require(key not in result)
        result[key] = value
    return result


def _constant(_: str) -> None:
    raise ProtocolError()


def _json_tree(value: Any, depth: int = 0) -> None:
    _require(depth <= 64)
    if isinstance(value, dict):
        for key, item in value.items():
            key.encode("utf-8", errors="strict")
            _json_tree(item, depth + 1)
    elif isinstance(value, list):
        for item in value:
            _json_tree(item, depth + 1)
    elif isinstance(value, str):
        value.encode("utf-8", errors="strict")
    elif isinstance(value, (int, float)):
        # Match Go's finite binary64 range check without rounding decoded ints.
        _require(math.isfinite(float(value)))


def _object(value: Any, fields: set[str], nullable: str = "") -> None:
    _require(isinstance(value, dict) and value.keys() == fields)
    _require(all(item is not None or key == nullable for key, item in value.items()))


def _integer(value: Any, low: int, high: int) -> bool:
    return type(value) is int and low <= value <= high


def _match(pattern: re.Pattern[str], value: Any) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def _member(value: Any, choices: set[str]) -> bool:
    return isinstance(value, str) and value in choices


def _json_bytes(value: Any) -> bytes:
    return json.dumps(
        value, ensure_ascii=False, allow_nan=False, separators=(",", ":")
    ).encode("utf-8")


def _binding(value: Any) -> None:
    _object(value, _BINDING)
    for key in ("tenant_id", "worker_id", "profile_id"):
        _require(_match(_KEY, value[key]))
    for key in ("run_id", "step_id", "session_id", "snapshot_id"):
        _require(_match(_UUID, value[key]))
    for key in ("profile_hash", "snapshot_hash", "input_hash"):
        _require(_match(_HASH, value[key]))
    _require(_integer(value["attempt_no"], 1, MAX_INTEGER))
    _require(_integer(value["fencing_token"], 1, MAX_INTEGER))
    _require(_integer(value["cursor_version"], 0, MAX_INTEGER))
    _require(_integer(value["step_sequence"], 1, 32))
    _require(_member(value["step_kind"], _STEPS))


def _subcalls(step: str) -> list[str]:
    if step in {"get_order", "get_delivery"}:
        return [step]
    if step == "search_policy":
        return ["profile_version", "profile_tags", "query_embedding", "search_policy"]
    if step in {"model_proposal", "protocol_correction"}:
        return ["chat"]
    return []


def _usage(value: Any) -> None:
    _object(value, _USAGE)
    _require(_integer(value["input_tokens"], 0, MAX_INTEGER))
    _require(_integer(value["output_tokens"], 0, MAX_INTEGER))
    _require(_integer(value["cached_input_tokens"], 0, value["input_tokens"]))
    _require(_match(_HASH, value["receipt_hash"]))
    _require(_match(_HASH, value["usage_hash"]))
    _require(value["usage_hash"] == usage_hash(value))


def usage_hash(usage: Frame) -> str:
    """Return the existing length-prefixed ledger fingerprint for token usage."""
    digest = hashlib.sha256()
    for field in (
        "jobforge.run.usage.v1",
        str(usage["input_tokens"]),
        str(usage["output_tokens"]),
        str(usage["cached_input_tokens"]),
        usage["receipt_hash"],
    ):
        raw = field.encode("utf-8")
        digest.update(len(raw).to_bytes(8, "big"))
        digest.update(raw)
    return digest.hexdigest()


def _permit(frame: Frame) -> None:
    _require(type(frame["granted"]) is bool)
    _require(_member(frame["error_code"], _ERRORS))
    _require(_integer(frame["input_token_limit"], 0, MAX_INTEGER))
    _require(_integer(frame["output_token_limit"], 0, 1024))
    _require(frame["input_token_limit"] + frame["output_token_limit"] <= MAX_INTEGER)
    if not frame["granted"]:
        _require(frame["physical_call_id"] == "" and frame["error_code"] != "")
        for field in (
            "dispatch_ms",
            "call_ms",
            "input_token_limit",
            "output_token_limit",
        ):
            _require(_integer(frame[field], 0, 0))
        return
    paid = frame["subcall"] in {"chat", "query_embedding"}
    _require(_match(_UUID, frame["physical_call_id"]) and frame["error_code"] == "")
    _require(_integer(frame["dispatch_ms"], 1, 30000))
    _require(_integer(frame["call_ms"], 1, 60000 if paid else 10000))
    _require(frame["dispatch_ms"] <= frame["call_ms"])
    if not paid:
        _require(frame["input_token_limit"] == frame["output_token_limit"] == 0)


def _validate(frame: Frame) -> None:
    kind = frame.get("kind")
    if not isinstance(kind, str) or kind not in _KINDS:
        raise ProtocolError()
    _object(
        frame,
        _COMMON | _KINDS[kind],
        "usage_hash" if kind == "call_observation" else "",
    )
    _require(_integer(frame["version"], 2, 2))
    _require(_integer(frame["emitted_mono_ms"], 0, MAX_INTEGER))
    _require(_match(_UUID, frame["request_id"]))
    _binding(frame["binding"])
    if kind == "execute_step":
        _require(_integer(frame["remaining_ms"], 1, 180000))
        trace = frame["trace_context"]
        _require(
            isinstance(trace, str)
            and len(trace) <= 512
            and all(32 <= ord(char) <= 126 for char in trace)
        )
        _require(isinstance(frame["checkpoint"], dict))
        _require(isinstance(frame["input"], dict))
    elif kind in {"call_intent", "call_permit"}:
        _require(_integer(frame["call_sequence"], 1, 44))
        _require(frame["subcall"] in _subcalls(frame["binding"]["step_kind"]))
        _require(_match(_HASH, frame["parameter_hash"]))
        _require(
            frame["tool_invocation_id"] == ""
            if frame["subcall"] == "chat"
            else _match(_UUID, frame["tool_invocation_id"])
        )
        if kind == "call_permit":
            _permit(frame)
    elif kind == "call_observation":
        _require(_integer(frame["call_sequence"], 1, 44))
        _require(_match(_UUID, frame["physical_call_id"]))
        _require(
            _member(frame["usage_disposition"], {"unknown", "reported"})
            and (
                frame["usage_hash"] is None
                if frame["usage_disposition"] == "unknown"
                else _match(_HASH, frame["usage_hash"])
            )
        )
        _require(_member(frame["error_code"], _ERRORS))
        if frame["transport_outcome"] == "response":
            _require(_integer(frame["http_status"], 100, 599))
            _require(_member(frame["business_outcome"], {"accepted", "rejected"}))
        else:
            _require(
                frame["transport_outcome"] == "unknown"
                and _integer(frame["http_status"], 0, 0)
                and frame["business_outcome"] == "unknown"
                and frame["usage_disposition"] == "unknown"
            )
        _require(
            (frame["business_outcome"] == "accepted") == (frame["error_code"] == "")
        )
    elif kind in _METERING_KINDS:
        _require(_integer(frame["call_sequence"], 1, 44))
        _require(_match(_UUID, frame["physical_call_id"]))
        if kind == "metering_report":
            _require(_match(_HASH, frame["parameter_hash"]))
            _usage(frame["usage"])
        else:
            _require(_match(_HASH, frame["usage_hash"]))
            _require(
                _member(frame["settlement"], {"settled", "anomaly", "unconfirmed"})
            )
    else:
        _require(isinstance(frame["result"], dict))
        _require(_member(frame["error_code"], _ERRORS))
        _require(
            _member(frame["outcome"], {"success", "error"})
            and ((frame["outcome"] == "success") == (frame["error_code"] == ""))
        )


def decode(line: bytes) -> Frame:
    """Decode exactly one LF-terminated, bounded UTF-8 JSON Lines frame.

    Raises:
        ProtocolError: On malformed, duplicate, unknown, oversized or wrong fields.
    """
    return _decode(line, False)


def decode_metering(line: bytes) -> Frame:
    """Decode only a bounded report/ACK from the dedicated metering pipe."""
    return _decode(line, True)


def _decode(line: bytes, metering: bool) -> Frame:
    if len(line) > (MAX_METERING_FRAME_BYTES if metering else MAX_FRAME_BYTES):
        raise ProtocolError("FRAME_LIMIT")
    try:
        _require(
            line.endswith(b"\n") and b"\n" not in line[:-1] and b"\r" not in line[:-1]
        )
        value = json.loads(
            line.decode("utf-8"), object_pairs_hook=_pairs, parse_constant=_constant
        )
        _require(isinstance(value, dict))
        _json_tree(value)
        _validate(value)
        _require((value["kind"] in _METERING_KINDS) == metering)
        _raw_sizes(line.decode("utf-8"), value)
        return value
    except (
        UnicodeError,
        ValueError,
        TypeError,
        KeyError,
        RecursionError,
        OverflowError,
    ) as error:
        if isinstance(error, ProtocolError):
            raise
        raise ProtocolError() from None


def _raw_sizes(text: str, frame: Frame) -> None:
    # Limits measure source UTF-8 bytes, so whitespace and escaped Unicode cannot
    # create different Go/Python acceptance decisions at a protected-data boundary.
    limits = {"checkpoint": MAX_CHECKPOINT_BYTES, "input": 16384, "result": 16384}
    if frame["binding"]["step_kind"] in {
        "read_ticket",
        "get_order",
        "get_delivery",
        "search_policy",
    }:
        limits["result"] = 8192
    decoder = json.JSONDecoder()
    index = text.index("{") + 1
    while True:
        while text[index].isspace():
            index += 1
        if text[index] == "}":
            return
        key, index = decoder.raw_decode(text, index)
        while text[index].isspace() or text[index] == ":":
            index += 1
        start = index
        _, index = decoder.raw_decode(text, index)
        if key in limits:
            _require(len(text[start:index].encode("utf-8")) <= limits[key])
        while text[index].isspace():
            index += 1
        if text[index] == ",":
            index += 1


def encode(frame: Frame) -> bytes:
    """Encode and validate an explicit frame without silently dropping fields."""
    return _encode(frame, False)


def encode_metering(frame: Frame) -> bytes:
    """Encode only a validated metering frame for its dedicated pipe."""
    return _encode(frame, True)


def _encode(frame: Frame, metering: bool) -> bytes:
    try:
        line = _json_bytes(frame) + b"\n"
    except (UnicodeError, ValueError, TypeError, RecursionError):
        raise ProtocolError() from None
    _decode(line, metering)
    return line


def read_frame(stream: BinaryIO) -> Frame:
    """Read at most one bounded line; the supervisor owns I/O deadlines."""
    line = stream.readline(MAX_FRAME_BYTES + 1)
    if not line:
        raise EOFError()
    return decode(line)


def read_metering_frame(stream: BinaryIO) -> Frame:
    """Read one metering frame without consuming the following frame."""
    line = stream.readline(MAX_METERING_FRAME_BYTES + 1)
    if not line:
        raise EOFError()
    return decode_metering(line)


class Conversation:
    """Guard one sequential step; serialize all calls on the caller's thread.

    The caller injects CLOCK_BOOTTIME milliseconds from the shared Linux time
    namespace. Ordinary closure is permanent, while narrowly bound metering can
    still settle already dispatched calls. No method extends process lifetime.
    """

    def __init__(self) -> None:
        """Create an unstarted exchange without granting execution authority."""
        self._closed = False
        self._blocked = False
        self._start: Frame | None = None
        self._phase = "idle"
        self._deadline = 0
        self._last_time = -1
        self._emitted: dict[str, int] = {}
        self._intent: Frame = {}
        self._active_id = ""
        self._call_index = 0
        self._call_sequence = 0
        self._tool_id = ""
        self._resume_floor = 0
        self._calls: dict[str, Frame] = {}

    @property
    def closed(self) -> bool:
        """Report irreversible loss of ordinary execution authority."""
        return self._closed

    @property
    def deadline(self) -> int:
        """Return the original step deadline, or zero before execution starts."""
        return self._deadline

    @property
    def metering_pending(self) -> bool:
        """Report an ordinary observation waiting for its settled usage join."""
        return self._phase == "settlement"

    def _clock(self, frame: Frame, now: int, direction: str) -> None:
        _require(_integer(now, 0, MAX_INTEGER) and now >= self._last_time)
        emitted = frame["emitted_mono_ms"]
        _require(self._emitted.get(direction, 0) <= emitted <= now)
        self._last_time = now
        self._emitted[direction] = emitted

    def _identity(self, frame: Frame) -> None:
        _require(
            self._start is not None
            and frame["request_id"] == self._start["request_id"]
            and frame["binding"] == self._start["binding"]
        )

    def accept(self, frame: Frame, now_mono_ms: int) -> None:
        """Accept ordinary execution only before its anchored deadlines.

        Args:
            frame: Exact sent or received ordinary frame.
            now_mono_ms: Injected CLOCK_BOOTTIME milliseconds at actual receipt.

        Raises:
            ProtocolError: Malformed, expired, unordered, or mismatched frames.
        """
        try:
            encode(frame)
            self._accept(copy.deepcopy(frame), now_mono_ms)
        except ProtocolError:
            self._closed = True
            raise

    def _accept(self, frame: Frame, now: int) -> None:
        _require(not self._closed)
        kind = frame["kind"]
        direction = (
            "ordinary_parent"
            if kind in {"execute_step", "call_permit"}
            else "ordinary_child"
        )
        self._clock(frame, now, direction)
        if self._start is None:
            _require(kind == "execute_step")
            deadline = frame["emitted_mono_ms"] + frame["remaining_ms"]
            _require(now < deadline <= MAX_INTEGER)
            self._start, self._deadline = frame, deadline
            return
        self._identity(frame)
        _require(
            now < self._deadline
            and frame["emitted_mono_ms"] >= self._start["emitted_mono_ms"]
        )
        calls = _subcalls(frame["binding"]["step_kind"])
        if kind == "call_intent":
            _require(
                self._phase == "idle"
                and not self._blocked
                and frame["emitted_mono_ms"] >= self._resume_floor
                and self._call_index < len(calls)
                and frame["subcall"] == calls[self._call_index]
                and frame["call_sequence"] == self._call_sequence + 1
            )
            _require(
                self._call_index == 0 or frame["tool_invocation_id"] == self._tool_id
            )
            self._intent = frame
            self._tool_id = frame["tool_invocation_id"]
            self._phase = "intent"
            self._call_sequence = frame["call_sequence"]
        elif kind == "call_permit":
            self._permit(frame, now)
        elif kind == "call_observation":
            self._observe(frame, now)
        elif kind == "step_result":
            _require(
                self._phase == "idle"
                and frame["emitted_mono_ms"] >= self._resume_floor
                and (not self._blocked or frame["outcome"] == "error")
                and (frame["outcome"] != "success" or self._call_index == len(calls))
            )
            self._closed = True
        else:
            raise ProtocolError()

    def _permit(self, frame: Frame, now: int) -> None:
        _require(self._phase == "intent")
        _require(frame["emitted_mono_ms"] >= self._intent["emitted_mono_ms"])
        for field in (
            "call_sequence",
            "subcall",
            "parameter_hash",
            "tool_invocation_id",
        ):
            _require(frame[field] == self._intent[field])
        if not frame["granted"]:
            self._blocked, self._phase = True, "idle"
            return
        physical_id = frame["physical_call_id"]
        dispatch_deadline = frame["emitted_mono_ms"] + frame["dispatch_ms"]
        call_deadline = frame["emitted_mono_ms"] + frame["call_ms"]
        _require(
            physical_id not in self._calls
            and now < dispatch_deadline <= call_deadline <= self._deadline
        )
        self._calls[physical_id] = {
            "permit": frame,
            "dispatch_deadline": dispatch_deadline,
            "call_deadline": call_deadline,
            "dispatched_at": None,
            "report": None,
            "observation": None,
            "settlement": "",
            "ack_emitted": 0,
        }
        self._active_id, self._phase = physical_id, "call"

    def _observe(self, frame: Frame, now: int) -> None:
        _require(self._phase == "call" and frame["physical_call_id"] == self._active_id)
        call = self._calls[self._active_id]
        _require(
            call["dispatched_at"] is not None
            and frame["call_sequence"] == call["permit"]["call_sequence"]
            and call["dispatched_at"] <= frame["emitted_mono_ms"]
            and now < call["call_deadline"]
        )
        if frame["usage_disposition"] == "reported":
            _require(call["permit"]["subcall"] in {"chat", "query_embedding"})
            _require(
                call["report"] is None
                or frame["usage_hash"] == call["report"]["usage"]["usage_hash"]
            )
            self._phase = "settlement"
        else:
            _require(call["report"] is None)
            self._phase = "idle"
        call["observation"] = frame
        self._call_index += 1
        self._blocked = frame["business_outcome"] != "accepted"
        self._join(call, now)

    def _join(self, call: Frame, now: int) -> None:
        observation, report = call["observation"], call["report"]
        if observation is None or report is None:
            return
        if observation["usage_disposition"] == "unknown":
            self._closed = True
            return
        if observation["usage_hash"] != report["usage"]["usage_hash"]:
            # Preserve independently validated usage even when ordinary output
            # lied about its hash; the caller can still settle the original call.
            self._closed = True
            return
        if (
            self._phase != "settlement"
            or observation["physical_call_id"] != self._active_id
        ):
            return
        if now >= call["call_deadline"] or now >= self._deadline:
            self._closed = True
        if call["settlement"] == "settled" and not self._closed:
            self._phase = "idle"
            self._resume_floor = max(self._resume_floor, call["ack_emitted"])

    def can_dispatch(self, physical_call_id: str, now_mono_ms: int) -> None:
        """Consume permission once immediately before sending physical HTTP."""
        try:
            _require(_integer(now_mono_ms, 0, MAX_INTEGER))
            _require(
                not self._closed
                and self._phase == "call"
                and physical_call_id == self._active_id
                and self._last_time <= now_mono_ms < self._deadline
            )
            call = self._calls[self._active_id]
            _require(
                call["dispatched_at"] is None
                and now_mono_ms < call["dispatch_deadline"]
                and now_mono_ms < call["call_deadline"]
            )
            call["dispatched_at"] = now_mono_ms
            self._last_time = now_mono_ms
        except ProtocolError:
            self._closed = True
            raise

    def accept_metering(self, frame: Frame, now_mono_ms: int) -> None:
        """Join one report or ACK for an original dispatched call, even stopped.

        Complete reports above token bounds remain available for settlement but
        close ordinary execution immediately. All later confirmations preserve
        that closure. The caller must drain/join this pipe before committing.
        """
        try:
            encode_metering(frame)
            self._accept_metering(copy.deepcopy(frame), now_mono_ms)
        except ProtocolError:
            self._closed = True
            raise

    def _accept_metering(self, frame: Frame, now: int) -> None:
        self._identity(frame)
        direction = (
            "metering_child"
            if frame["kind"] == "metering_report"
            else "metering_parent"
        )
        self._clock(frame, now, direction)
        call = self._calls.get(frame["physical_call_id"])
        _require(call is not None)
        assert call is not None
        permit = call["permit"]
        _require(
            call["dispatched_at"] is not None
            and call["dispatched_at"] <= frame["emitted_mono_ms"]
            and frame["call_sequence"] == permit["call_sequence"]
            and permit["subcall"] in {"chat", "query_embedding"}
        )
        if now >= self._deadline:
            self._closed = True
        if frame["kind"] == "metering_report":
            _require(frame["parameter_hash"] == permit["parameter_hash"])
            old = call["report"]
            _require(old is None or old["usage"] == frame["usage"])
            if old is None:
                call["report"] = frame
            usage = frame["usage"]
            if (
                usage["input_tokens"] > permit["input_token_limit"]
                or usage["output_tokens"] > permit["output_token_limit"]
            ):
                self._closed = True
        else:
            report = call["report"]
            _require(
                report is not None
                and report["usage"]["usage_hash"] == frame["usage_hash"]
            )
            _require(frame["emitted_mono_ms"] >= report["emitted_mono_ms"])
            _require(
                frame["settlement"] != "settled"
                or (
                    report["usage"]["input_tokens"] <= permit["input_token_limit"]
                    and report["usage"]["output_tokens"] <= permit["output_token_limit"]
                )
            )
            previous = call["settlement"]
            _require(previous in {"", "unconfirmed", frame["settlement"]})
            call["settlement"] = frame["settlement"]
            if previous != "settled":
                call["ack_emitted"] = frame["emitted_mono_ms"]
            if frame["settlement"] != "settled":
                self._closed = True
        self._join(call, now)

    def stop(self) -> None:
        """Permanently close ordinary execution without discarding bound usage."""
        self._closed = True
