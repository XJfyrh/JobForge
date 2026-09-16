"""Installed SDK queries real PG audit rows via production HTTP, no model calls."""

from __future__ import annotations

import json
import sys

import httpx
import pytest

from jobforge import NotFoundError, RunClient, UnauthorizedError


def main() -> None:
    """Check one seeded synthetic fact scenario without any mutation endpoint."""
    url, run_id, call_id, scenario = sys.argv[1:]
    with (
        RunClient(url, "audit-reader") as reader,
        RunClient(url, "audit-operator") as operator,
        RunClient(url, "audit-foreign") as foreign,
        RunClient(url, "invalid") as unauthorized,
    ):
        report = reader.calls(run_id)
        repeat = operator.calls(run_id)
        assert report.run_id == repeat.run_id == run_id
        assert report.items == repeat.items
        assert report.captured_at <= repeat.captured_at
        assert len(report.items) == 7  # Six actual non-chat reservations precede chat.
        calls = [call for call in report.items if call.physical_call_id == call_id]
        assert len(calls) == 1
        call = calls[0]
        assert call.subcall == "chat" and call.ordinal == 7
        assert call.reserved.total_tokens == 150
        assert call.known_tokens + call.held_tokens <= 150
        if scenario == "missing":
            assert call.audit_status == "missing"
            assert call.provider_audit is None and call.report_hash is None
            assert not call.usage_known and call.observed_usage is None
            assert call.held_tokens == 150 and call.settled_usage is None
            assert not report.batch_frozen
        else:
            assert call.audit_status == "recorded"
            assert call.provider_audit is not None and call.report_hash is not None
            assert call.report_recorded_at is not None
            assert call.audit_hash == call.provider_audit.audit_hash
            if scenario == "incompatible":
                assert call.provider_audit.identity_state == "incompatible"
                assert call.provider_audit.response_model == "different-model"
                assert call.observed_usage is not None
                assert call.observed_usage.input_tokens == 20
                assert call.settled_usage is None and not call.usage_known
                assert call.known_cost_microyuan == 0 and call.held_tokens == 150
                assert report.batch_stop_code == "PROVIDER_IDENTITY_INVALID"
            elif scenario == "unavailable":
                assert not call.provider_audit.response_complete
                assert call.provider_audit.http_status == 0
                assert call.provider_audit.reasoning_tokens is None
                assert call.observed_usage is None and call.settled_usage is None
                assert not call.usage_known and call.held_tokens == 150
                assert report.batch_stop_code == "CHAT_USAGE_UNKNOWN"
            else:
                assert call.usage_known and call.settled_usage is not None
                assert call.observed_usage == call.settled_usage
                assert call.known_tokens == 30 and call.held_tokens == 0
                assert call.settled_usage.input_tokens == 20  # First report wins.
                if scenario == "conflict":
                    assert call.report_conflict
                    assert not call.measurement_anomaly
                    assert report.batch_stop_code == "REPORT_CONFLICT"
                else:
                    assert call.error_code == "MODEL_PROTOCOL_ERROR"
                    assert (
                        call.http_status == 200 and call.business_outcome == "rejected"
                    )
                    assert call.observed_at is not None and not report.batch_frozen
            assert report.batch_frozen == (scenario != "known_rejected")
        with pytest.raises(NotFoundError):
            foreign.calls(run_id)
        with pytest.raises(UnauthorizedError):
            unauthorized.calls(run_id)
    with httpx.Client(
        base_url=url, headers={"Authorization": "Bearer audit-reader"}
    ) as raw:
        response = raw.get(f"/v2/runs/{run_id}/calls")
        assert response.status_code == 200 and len(response.content) <= 256 * 1024
        body = json.dumps(response.json())
        for forbidden in (
            "worker_id",
            "session_id",
            "fencing_token",
            "control_token",
            "response_body",
        ):
            assert forbidden not in body
        for query in ("limit=1", "tenant=tenant-b", "cursor=1"):
            assert raw.get(f"/v2/runs/{run_id}/calls?{query}").status_code == 400
    print("PASS real HTTP audit SDK: " + scenario)


if __name__ == "__main__":
    main()
