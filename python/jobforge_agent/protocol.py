"""Strict executor codec for api/executor/v1/schema.json; no network or process I/O.

The caller owns process supervision, lease renewal, monotonic clock injection and
trusted profile resolution. Passing validation alone never grants Run authority.
"""

import copy
import json
import math
import re
from typing import Any, BinaryIO

MAX_FRAME_BYTES = 384 * 1024
MAX_CHECKPOINT_BYTES = 256 * 1024
MAX_INTEGER = 2**53 - 1
Frame = dict[str, Any]

_UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_KEY = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
_HASH = re.compile(r"[0-9a-f]{64}")
_COMMON = {"version", "kind", "request_id", "binding"}
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
        "usage_known",
        "usage",
    },
    "step_result": {"outcome", "error_code", "result"},
}
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
    elif isinstance(value, float):
        _require(math.isfinite(value))


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
    if value is None:
        return
    _object(value, _USAGE)
    _require(_integer(value["input_tokens"], 0, MAX_INTEGER))
    _require(_integer(value["output_tokens"], 0, 1024))
    _require(_integer(value["cached_input_tokens"], 0, value["input_tokens"]))
    _require(value["input_tokens"] + value["output_tokens"] <= MAX_INTEGER)
    _require(_match(_HASH, value["receipt_hash"]))
    _require(_match(_HASH, value["usage_hash"]))


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
    _object(frame, _COMMON | _KINDS[kind], "usage")
    _require(_integer(frame["version"], 1, 1))
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
            type(frame["usage_known"]) is bool
            and frame["usage_known"] == (frame["usage"] is not None)
        )
        _usage(frame["usage"])
        _require(_member(frame["error_code"], _ERRORS))
        if frame["transport_outcome"] == "response":
            _require(_integer(frame["http_status"], 100, 599))
            _require(_member(frame["business_outcome"], {"accepted", "rejected"}))
        else:
            _require(
                frame["transport_outcome"] == "unknown"
                and _integer(frame["http_status"], 0, 0)
                and frame["business_outcome"] == "unknown"
                and not frame["usage_known"]
            )
        _require(
            (frame["business_outcome"] == "accepted") == (frame["error_code"] == "")
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
    if len(line) > MAX_FRAME_BYTES:
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
        _raw_sizes(line.decode("utf-8"), value)
        return value
    except (UnicodeError, ValueError, TypeError, KeyError, RecursionError) as error:
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
    try:
        line = _json_bytes(frame) + b"\n"
    except (UnicodeError, ValueError, TypeError, RecursionError):
        raise ProtocolError() from None
    decode(line)
    return line


def read_frame(stream: BinaryIO) -> Frame:
    """Read at most one bounded line; the supervisor owns I/O deadlines."""
    line = stream.readline(MAX_FRAME_BYTES + 1)
    if not line:
        raise EOFError()
    return decode(line)


class Conversation:
    """Fail-closed sequential protocol guard for one step and one caller thread."""

    def __init__(self) -> None:
        """Create an unstarted exchange without granting execution authority."""
        self._closed = False
        self._blocked = False
        self._start: Frame | None = None
        self._phase = "idle"
        self._deadline = 0
        self._last_time = -1
        self._call_deadline = 0
        self._intent: Frame = {}
        self._permit_frame: Frame = {}
        self._call_index = 0
        self._call_sequence = 0
        self._seen_calls: set[str] = set()
        self._tool_id = ""

    def accept(self, frame: Frame, now_ms: int) -> None:
        """Validate a frame with an injected monotonic millisecond clock.

        Args:
            frame: Exact frame being sent or received.
            now_ms: Monotonic local clock; equality at any deadline is expired.

        Raises:
            ProtocolError: Any malformed, out-of-order, expired or mismatched frame.
        """
        try:
            encode(frame)
            self._accept(copy.deepcopy(frame), now_ms)
            self._last_time = now_ms
        except ProtocolError:
            self._closed = True
            raise

    def _accept(self, frame: Frame, now: int) -> None:
        _require(type(now) is int and now >= self._last_time and not self._closed)
        if self._start is None:
            _require(frame["kind"] == "execute_step")
            self._start = frame
            self._deadline = now + frame["remaining_ms"]
            return
        _require(
            frame["request_id"] == self._start["request_id"]
            and frame["binding"] == self._start["binding"]
            and now < self._deadline
        )
        kind = frame["kind"]
        calls = _subcalls(frame["binding"]["step_kind"])
        if kind == "call_intent":
            _require(
                self._phase == "idle"
                and not self._blocked
                and self._call_index < len(calls)
                and frame["subcall"] == calls[self._call_index]
                and frame["call_sequence"] == self._call_sequence + 1
            )
            self._intent = frame
            _require(
                self._call_index == 0 or frame["tool_invocation_id"] == self._tool_id
            )
            self._tool_id = frame["tool_invocation_id"]
            self._phase = "intent"
            self._call_sequence = frame["call_sequence"]
        elif kind == "call_permit":
            _require(self._phase == "intent")
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
            _require(
                frame["physical_call_id"] not in self._seen_calls
                and now + frame["call_ms"] <= self._deadline
            )
            self._seen_calls.add(frame["physical_call_id"])
            self._permit_frame, self._phase = frame, "call"
            self._call_deadline = now + frame["call_ms"]
        elif kind == "call_observation":
            _require(self._permit_frame.get("dispatch_ms") == 0)
            _require(
                self._phase == "call"
                and frame["call_sequence"] == self._permit_frame["call_sequence"]
                and frame["physical_call_id"] == self._permit_frame["physical_call_id"]
                and now < self._call_deadline
            )
            if frame["usage"] is not None:
                _require(
                    self._permit_frame["subcall"] in {"chat", "query_embedding"}
                    and frame["usage"]["input_tokens"]
                    <= self._permit_frame["input_token_limit"]
                    and frame["usage"]["output_tokens"]
                    <= self._permit_frame["output_token_limit"]
                )
            self._phase = "idle"
            self._call_index += 1
            self._blocked = frame["business_outcome"] != "accepted"
        elif kind == "step_result":
            _require(
                self._phase == "idle"
                and (not self._blocked or frame["outcome"] == "error")
                and (frame["outcome"] != "success" or self._call_index == len(calls))
            )
            self._closed = True
        else:
            raise ProtocolError()

    def can_dispatch(self, physical_call_id: str, now_ms: int) -> None:
        """Consume local permission once, immediately before a physical HTTP send."""
        try:
            _require(
                not self._closed
                and self._phase == "call"
                and physical_call_id == self._permit_frame["physical_call_id"]
                and self._last_time
                <= now_ms
                < self._last_time + self._permit_frame["dispatch_ms"]
                and now_ms < self._deadline
                and self._permit_frame["dispatch_ms"] > 0
            )
            self._permit_frame["dispatch_ms"] = 0
            self._last_time = now_ms
        except ProtocolError:
            self._closed = True
            raise

    def stop(self) -> None:
        """Invalidate all local permission immediately on stop, timeout or lease loss."""
        self._closed = True
