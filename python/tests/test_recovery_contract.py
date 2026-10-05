"""Go-generated recovery identities and fixed-version runtime compatibility."""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest
from jobforge_agent.provider_audit import ProviderAuditError, execution_binding_hash
from jobforge_agent.runtime_input import (
    AGENT_EXECUTOR_VERSION,
    RECOVERY_EXECUTOR_VERSION,
    RuntimeInputError,
    parse_runtime_input,
)
from jsonschema import Draft202012Validator
from test_runtime_input import FIXTURE as RUNTIME

FIXTURE = json.loads(
    (
        Path(__file__).resolve().parents[2] / "api/support/recovery-v1/fixtures.json"
    ).read_bytes()
)
RUNTIME_SCHEMA = json.loads(
    (
        Path(__file__).resolve().parents[2]
        / "api/executor/v2/runtime-input.schema.json"
    ).read_bytes()
)


def binding(step: dict) -> dict:
    """Project the internal fixture Lease/Step into the existing v2 hash shape."""
    lease = FIXTURE["lease"]
    return {
        "tenant_id": lease["TenantID"],
        "run_id": lease["RunID"],
        "worker_id": lease["WorkerID"],
        "session_id": lease["SessionID"],
        "attempt_no": lease["AttemptNo"],
        "fencing_token": lease["FencingToken"],
        **{
            key: value for key, value in step.items() if key not in {"sequence", "kind"}
        },
        "step_sequence": step["sequence"],
        "step_kind": step["kind"],
    }


def test_original_recovery_identity_has_the_same_binding_hash() -> None:
    """No later cursor or newly configured profile replaces the original tuple."""
    assert FIXTURE["profile"]["executor_version"] == RECOVERY_EXECUTOR_VERSION
    assert FIXTURE["profile"]["definition"]["schema_version"] == 3
    assert (
        FIXTURE["profile"]["definition"]["program"]["recovery_policy"]
        == "confirmed_uncommitted_v1"
    )
    assert (
        execution_binding_hash(binding(FIXTURE["step"]))
        == FIXTURE["execution_binding_hash"]
    )


@pytest.mark.parametrize("version", [AGENT_EXECUTOR_VERSION, RECOVERY_EXECUTOR_VERSION])
def test_historical_and_recovery_input_versions_remain_readable(version: str) -> None:
    """Source schema and parser accept both registered agent runtime versions."""
    frame = copy.deepcopy(RUNTIME["valid"][0]["frame"])
    frame["input"]["adapter_id"] = "support-agent-v1"
    frame["input"]["executor_version"] = version
    Draft202012Validator(RUNTIME_SCHEMA).validate(
        {"input": frame["input"], "checkpoint": frame["checkpoint"]}
    )
    assert parse_runtime_input(frame).executor_version == version


@pytest.mark.parametrize("version", ["wrong-runtime", "linux-v2-recovery-runtime-2"])
def test_schema_and_parser_reject_unregistered_runtime_versions(version: str) -> None:
    """Adding recovery does not admit arbitrary or future runtime versions."""
    frame = copy.deepcopy(RUNTIME["valid"][0]["frame"])
    frame["input"]["adapter_id"] = "support-agent-v1"
    frame["input"]["executor_version"] = version
    assert not Draft202012Validator(RUNTIME_SCHEMA).is_valid(
        {"input": frame["input"], "checkpoint": frame["checkpoint"]}
    )
    with pytest.raises(RuntimeInputError):
        parse_runtime_input(frame)


@pytest.mark.parametrize("version", ["linux-v2-audit-runtime-1", "wrong-runtime"])
def test_agent_adapter_rejects_wrong_runtime(version: str) -> None:
    """The S1 fixed runtime cannot accept the bounded agent implementation."""
    frame = copy.deepcopy(RUNTIME["valid"][0]["frame"])
    frame["input"]["adapter_id"] = "support-agent-v1"
    frame["input"]["executor_version"] = version
    with pytest.raises(RuntimeInputError):
        parse_runtime_input(frame)


def test_proof_step_changes_never_reuse_original_binding() -> None:
    """Typed-valid but mismatched identity fields produce different hashes."""
    for field in (
        "step_id",
        "input_hash",
        "profile_id",
        "profile_hash",
        "snapshot_id",
        "snapshot_hash",
    ):
        step = copy.deepcopy(FIXTURE["step"])
        if field.endswith("hash"):
            step[field] = "f" * 64
        elif field == "profile_id":
            step[field] = "other-profile"
        else:
            step[field] = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
        assert (
            execution_binding_hash(binding(step)) != FIXTURE["execution_binding_hash"]
        )
    for name in ("null_cursor", "float_cursor", "wrong_sequence"):
        with pytest.raises(ProviderAuditError):
            execution_binding_hash(binding(json.loads(FIXTURE["invalid_steps"][name])))


def test_versioned_profile_schema_preserves_legacy_and_requires_recovery_policy() -> (
    None
):
    """Schema 1 remains historical; schema 3 is a separate closed artifact."""
    root = Path(__file__).resolve().parents[2] / "api/support"
    legacy_schema = json.loads((root / "profile-v1/schema.json").read_bytes())
    recovery_schema = json.loads(
        (root / "recovery-v1/profile-schema.json").read_bytes()
    )
    legacy = json.loads((root / "profile-v1/fixtures.json").read_bytes())["profile"][
        "definition"
    ]
    recovery = copy.deepcopy(FIXTURE["profile"]["definition"])
    Draft202012Validator(legacy_schema).validate(legacy)
    validator = Draft202012Validator(recovery_schema)
    validator.validate(recovery)
    assert not Draft202012Validator(legacy_schema).is_valid(recovery)
    assert not validator.is_valid(legacy)
    for policy in (None, "", "legacy"):
        wrong = copy.deepcopy(recovery)
        wrong["program"]["recovery_policy"] = policy
        assert not validator.is_valid(wrong)
    del recovery["program"]["recovery_policy"]
    assert not validator.is_valid(recovery)
