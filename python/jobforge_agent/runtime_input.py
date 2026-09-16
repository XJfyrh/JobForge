"""Validate the trusted RPC checkpoint projection without scheduling or RPCs."""

from __future__ import annotations

import copy
import hashlib
import json
import re
from dataclasses import dataclass
from datetime import datetime
from typing import Any

from jobforge_agent.protocol_v2 import Frame, ProtocolError, encode

EXECUTOR_VERSION = "linux-v2-ack-runtime-1"
RuntimeCheckpoint = dict[str, Any]
IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
HASH = re.compile(r"[0-9a-f]{64}")
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
TOOLS = frozenset({"get_order", "get_delivery", "search_policy"})
KINDS = TOOLS | {
    "read_ticket",
    "model_proposal",
    "protocol_correction",
    "submit_proposal",
}
RESULT_FIELDS = {
    "schema_version",
    "tool_invocation_id",
    "physical_call_id",
    "evidence_refs",
    "content",
    "proposal",
    "correction_required",
}
STEP_FIELDS = {
    "step_id",
    "sequence",
    "kind",
    "cursor_version",
    "input_hash",
    "profile_id",
    "profile_hash",
    "snapshot_id",
    "snapshot_hash",
}


class RuntimeInputError(ValueError):
    """Expose a fixed input failure and a separate local size fact."""

    def __init__(self, *, size_limit: bool = False) -> None:
        """Keep local byte-limit facts distinct from ordinary input rejection."""
        self.size_limit = size_limit
        super().__init__("INPUT_INVALID")


@dataclass(frozen=True)
class RuntimeInput:
    """Carry a copied, validated selection and protected checkpoint."""

    executor_version: str
    adapter_id: str
    tool_invocation_id: str
    checkpoint: RuntimeCheckpoint


def _require(value: bool) -> None:
    if not value:
        raise RuntimeInputError()


def _object(value: Any, keys: set[str], nullable: frozenset[str] = frozenset()) -> None:
    _require(isinstance(value, dict) and set(value) == keys)
    _require(all(item is not None or key in nullable for key, item in value.items()))


def _match(pattern: re.Pattern[str], value: Any) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def _integer(value: Any, lower: int = 1, upper: int = 2**53 - 1) -> bool:
    return type(value) is int and lower <= value <= upper


def _size(value: Any, limit: int) -> None:
    try:
        raw = json.dumps(
            value, ensure_ascii=False, allow_nan=False, separators=(",", ":")
        ).encode("utf-8")
    except (ValueError, TypeError, UnicodeError, RecursionError):
        raise RuntimeInputError() from None
    if len(raw) > limit:
        raise RuntimeInputError(size_limit=True)


def input_hash(
    profile_hash: str, snapshot_hash: str, cursor: int, commit_hash: str
) -> str:
    """Check the original string hash chain; never recanonicalize commit content."""
    digest = hashlib.sha256()
    for value in (
        "jobforge.run.step-input.v1",
        profile_hash,
        snapshot_hash,
        str(cursor),
        commit_hash,
    ):
        raw = value.encode("utf-8")
        digest.update(len(raw).to_bytes(8, "big"))
        digest.update(raw)
    return digest.hexdigest()


def validate_step_result(result: Any, kind: str) -> None:
    """Check the protected envelope and closed registered proposal formats."""
    _require(kind in KINDS)
    _object(result, RESULT_FIELDS, frozenset({"content", "proposal"}))
    _size(result, 8192 if kind in TOOLS | {"read_ticket"} else 16384)
    _require(_integer(result["schema_version"], 1, 1))
    _require(type(result["correction_required"]) is bool)
    refs = result["evidence_refs"]
    _require(
        isinstance(refs, list)
        and len(refs) <= 32
        and all(isinstance(item, str) for item in refs)
    )
    for key in ("tool_invocation_id", "physical_call_id"):
        _require(result[key] == "" or _match(UUID, result[key]))
    proposal = result["proposal"]
    if proposal is not None:
        if isinstance(proposal, dict) and "conclusion" in proposal:
            from jobforge_agent.dispatch import DispatchError
            from jobforge_agent.support_contract import validate_persisted_shape

            try:
                validate_persisted_shape(proposal)
            except DispatchError as error:
                raise RuntimeInputError(size_limit=error.fact == "size_limit") from None
        else:
            _object(proposal, {"decision", "summary", "evidence_refs", "action"})
        _require(
            all(
                isinstance(proposal[key], str)
                for key in ("decision", "summary", "action")
            )
        )
        _require(
            isinstance(proposal["evidence_refs"], list)
            and all(isinstance(item, str) for item in proposal["evidence_refs"])
        )
    if kind in TOOLS | {"read_ticket"}:
        _require(
            proposal is None
            and not result["correction_required"]
            and isinstance(result["content"], dict)
        )
        _require(bool(result["tool_invocation_id"]) == (kind in TOOLS))
        _require(bool(result["physical_call_id"]) == (kind in TOOLS))
    else:
        _require(
            result["tool_invocation_id"] == ""
            and result["content"] is None
            and refs == []
        )
        _require(bool(result["physical_call_id"]) == (kind != "submit_proposal"))
        if result["correction_required"]:
            _require(kind == "model_proposal" and proposal is None)
        else:
            _require(proposal is not None)


def _snapshot(snapshot: Any, binding: Frame) -> None:
    _object(
        snapshot,
        {
            "tenant_id",
            "ticket_id",
            "snapshot_id",
            "snapshot_hash",
            "version_vector_json",
            "ticket_binding_json",
            "index_id",
            "index_profile_hash",
        },
    )
    for key in ("tenant_id", "snapshot_id", "snapshot_hash"):
        _require(snapshot[key] == binding[key])
    _require(
        _match(IDENTIFIER, snapshot["ticket_id"])
        and _match(UUID, snapshot["index_id"])
        and _match(HASH, snapshot["index_profile_hash"])
    )
    ticket, vector = snapshot["ticket_binding_json"], snapshot["version_vector_json"]
    _size(ticket, 8192)
    _size(vector, 8192)
    _object(
        ticket,
        {
            "tenant_id",
            "ticket_id",
            "revision",
            "observed_at",
            "order_id",
            "policy_version",
            "subject",
            "description",
            "status",
        },
        frozenset({"order_id"}),
    )
    _require(
        ticket["tenant_id"] == snapshot["tenant_id"]
        and ticket["ticket_id"] == snapshot["ticket_id"]
    )
    _require(
        _integer(ticket["revision"]) and _match(IDENTIFIER, ticket["policy_version"])
    )
    _require(ticket["order_id"] is None or _match(IDENTIFIER, ticket["order_id"]))
    _require(_match(IDENTIFIER, ticket["status"]))
    for key, limit in (("subject", 256), ("description", 1024)):
        _require(isinstance(ticket[key], str))
        _require(
            len(ticket[key].encode("utf-8")) <= limit and "\x00" not in ticket[key]
        )
    _require(
        isinstance(ticket["observed_at"], str)
        and re.fullmatch(
            r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})",
            ticket["observed_at"],
        )
        is not None
    )
    try:
        parsed = datetime.fromisoformat(ticket["observed_at"].replace("Z", "+00:00"))
        _require(
            parsed.tzinfo is not None and parsed.replace(tzinfo=None) != datetime.min
        )
    except ValueError:
        raise RuntimeInputError() from None
    _object(
        vector, {"schema_version", "ticket", "order", "delivery", "policy", "index"}
    )
    _require(_integer(vector["schema_version"], 1, 1))
    _object(vector["ticket"], {"id", "revision"})
    _require(
        vector["ticket"]["id"] == ticket["ticket_id"]
        and type(vector["ticket"]["revision"]) is int
        and vector["ticket"]["revision"] == ticket["revision"]
    )
    for name, revision in (("order", "revision"), ("delivery", "aggregate_revision")):
        value = vector[name]
        _object(value, {"id", "exists", revision}, frozenset({"id", revision}))
        _require(type(value["exists"]) is bool)
        _require(value["id"] is None or _match(IDENTIFIER, value["id"]))
        _require(
            (value["id"] is not None and _integer(value[revision]))
            if value["exists"]
            else value[revision] is None
        )
    _require(vector["order"]["id"] == ticket["order_id"])
    if not vector["order"]["exists"]:
        _require(vector["delivery"]["id"] is None and not vector["delivery"]["exists"])
    _object(vector["policy"], {"version", "revision", "corpus_sha256"})
    _require(
        vector["policy"]["version"] == ticket["policy_version"]
        and _integer(vector["policy"]["revision"])
        and _match(HASH, vector["policy"]["corpus_sha256"])
    )
    _object(vector["index"], {"id", "profile_hash", "content_hash"})
    _require(
        vector["index"]["id"] == snapshot["index_id"]
        and vector["index"]["profile_hash"] == snapshot["index_profile_hash"]
        and _match(HASH, vector["index"]["content_hash"])
    )


def _step(step: Any, binding: Frame, sequence: int, previous: str) -> None:
    _object(step, STEP_FIELDS)
    _require(
        _match(UUID, step["step_id"])
        and isinstance(step["kind"], str)
        and step["kind"] in KINDS
    )
    _require(
        _integer(step["sequence"], sequence, sequence)
        and _integer(step["cursor_version"], sequence - 1, sequence - 1)
    )
    for key in ("profile_id", "profile_hash", "snapshot_id", "snapshot_hash"):
        _require(step[key] == binding[key])
    _require(
        step["input_hash"]
        == input_hash(
            binding["profile_hash"], binding["snapshot_hash"], sequence - 1, previous
        )
    )


def parse_runtime_input(frame: Frame) -> RuntimeInput:
    """Parse one standard execute frame, rejecting added authority or identity."""
    _require(isinstance(frame, dict))
    _size(frame.get("checkpoint"), 256 * 1024)
    _size(frame.get("input"), 16384)
    try:
        encode(frame)
    except ProtocolError as error:
        raise RuntimeInputError(size_limit=error.code == "FRAME_LIMIT") from None
    _require(frame["kind"] == "execute_step")
    selection, checkpoint, binding = (
        frame["input"],
        frame["checkpoint"],
        frame["binding"],
    )
    _object(
        selection,
        {"schema_version", "executor_version", "adapter_id", "tool_invocation_id"},
    )
    _require(_integer(selection["schema_version"], 1, 1))
    _require(
        selection["executor_version"] == EXECUTOR_VERSION
        and _match(IDENTIFIER, selection["adapter_id"])
    )
    _require(
        _match(UUID, selection["tool_invocation_id"])
        if binding["step_kind"] in TOOLS
        else selection["tool_invocation_id"] == ""
    )
    _object(checkpoint, {"cursor_version", "next_step", "steps", "snapshot"})
    _size(checkpoint, 256 * 1024)
    cursor = checkpoint["cursor_version"]
    _require(_integer(cursor, 0, 31) and cursor == binding["cursor_version"])
    _require(
        isinstance(checkpoint["steps"], list) and len(checkpoint["steps"]) == cursor
    )
    _snapshot(checkpoint["snapshot"], binding)
    previous = ""
    seen: set[str] = set()
    for sequence, accepted in enumerate(checkpoint["steps"], 1):
        _object(accepted, {"step", "commit_hash", "result_json", "result_ref"})
        _step(accepted["step"], binding, sequence, previous)
        _require(accepted["step"]["step_id"] not in seen)
        seen.add(accepted["step"]["step_id"])
        _require(_match(HASH, accepted["commit_hash"]))
        _require(accepted["result_ref"] == f"run-step:{binding['run_id']}:{sequence}")
        validate_step_result(accepted["result_json"], accepted["step"]["kind"])
        previous = accepted["commit_hash"]
    step = checkpoint["next_step"]
    _step(step, binding, cursor + 1, previous)
    _require(step["step_id"] not in seen)
    for source, target in (
        ("step_id", "step_id"),
        ("sequence", "step_sequence"),
        ("kind", "step_kind"),
        ("input_hash", "input_hash"),
    ):
        _require(step[source] == binding[target])
    return RuntimeInput(
        selection["executor_version"],
        selection["adapter_id"],
        selection["tool_invocation_id"],
        copy.deepcopy(checkpoint),
    )
