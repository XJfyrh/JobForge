"""Consume Go-generated audit vectors without any live provider or wire bridge."""

from __future__ import annotations

import base64
import json
from pathlib import Path
from typing import Any

import pytest
from jobforge_agent.provider_audit import (
    ProviderAuditError,
    ReportBinding,
    decode_call_report,
    decode_provider_audit,
    encode_call_report,
    encode_provider_audit,
    execution_binding_hash,
    observation_hash_v2,
)

_PATH = (
    Path(__file__).resolve().parents[2] / "api/executor/v2/fixtures/provider-audit.json"
)
_FIXTURE = json.loads(_PATH.read_bytes())


def _wire(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def _raw(case: dict[str, Any]) -> bytes:
    if "raw_base64" in case:
        return base64.b64decode(case["raw_base64"], validate=True)
    return case["raw"].encode("utf-8")


def test_go_fixture_is_present_and_has_the_accepted_sections() -> None:
    """Missing vectors are a failed contract test, never a passing skip."""
    assert _FIXTURE["schema_version"] == 1
    for section in (
        "binding_vectors",
        "valid_reports",
        "invalid_audits",
        "invalid_reports",
        "observations",
    ):
        assert _FIXTURE[section]


@pytest.mark.parametrize(
    "case", _FIXTURE["binding_vectors"], ids=lambda case: case["name"]
)
def test_go_execution_bindings_have_identical_python_hashes(
    case: dict[str, Any],
) -> None:
    """The original Lease/Step tuple has a byte-identical independent hash."""
    assert execution_binding_hash(case["binding"]) == case["execution_binding_hash"]


@pytest.mark.parametrize(
    "case", _FIXTURE["valid_reports"], ids=lambda case: case["name"]
)
def test_go_valid_reports_and_all_hashes_round_trip(case: dict[str, Any]) -> None:
    """Both languages agree on typed nulls, audit, usage, receipt and report."""
    binding = ReportBinding(**case["binding"])
    report = decode_call_report(_wire(case["report"]))
    report.verify(binding, case["report_hash"])
    assert report.hash(binding) == case["report_hash"]
    assert json.loads(encode_call_report(report)) == case["report"]
    audit = report.provider_audit
    if audit is not None:
        assert audit.audit_hash == audit.hash()
        assert decode_provider_audit(encode_provider_audit(audit)) == audit
    if report.usage is not None:
        assert report.usage.usage_hash == report.usage.hash()
        assert report.usage.receipt_hash == case["receipt_hash"]
        if audit is not None:
            assert audit.receipt_hash(binding.physical_call_id) == case["receipt_hash"]


@pytest.mark.parametrize(
    "case", _FIXTURE["invalid_audits"], ids=lambda case: case["name"]
)
def test_go_invalid_audits_are_also_rejected_in_python(case: dict[str, Any]) -> None:
    """Independent typed boundaries reject the exact shared malformed bytes."""
    with pytest.raises(ProviderAuditError):
        decode_provider_audit(_raw(case))


@pytest.mark.parametrize(
    "case", _FIXTURE["invalid_reports"], ids=lambda case: case["name"]
)
def test_go_invalid_report_or_binding_is_rejected_in_python(
    case: dict[str, Any],
) -> None:
    """Self-consistent hashes still fail mismatched call, profile and receipt."""
    with pytest.raises(ProviderAuditError):
        report = decode_call_report(_raw(case))
        report.verify(ReportBinding(**case["binding"]), case["report_hash"])


@pytest.mark.parametrize(
    "case", _FIXTURE["observations"], ids=lambda case: case["name"]
)
def test_go_observation_v2_domain_hashes_match(case: dict[str, Any]) -> None:
    """Domain error mapping and nullable hashes precede exact fingerprinting."""
    assert (
        observation_hash_v2(
            transport_outcome=case["transport_outcome"],
            http_status=case["http_status"],
            mapped_domain_error_code=case["error_code"],
            business_outcome=case["business_outcome"],
            usage_hash=case["usage"]["usage_hash"]
            if case["usage"] is not None
            else None,
            audit_hash=case["audit_hash"] or None,
        )
        == case["observation_hash"]
    )
