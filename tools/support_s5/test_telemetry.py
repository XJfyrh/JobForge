"""Deployment traces preserve the SDK link without credential inheritance."""

import os

import pytest

from tools.support_evaluation.launcher import child_environment
from tools.support_s5 import telemetry


def test_s5_children_get_trace_settings_but_not_credentials(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Opt-in propagates to both fixed processes; secret files stay role scoped."""
    monkeypatch.setenv("JOBFORGE_OTEL_EXPORTER", "otlp")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
    monkeypatch.setenv("JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE", "/run/secrets/worker")
    monkeypatch.setenv("DEEPSEEK_API_KEY", "synthetic-do-not-inherit")
    for worker in (False, True):
        environment = child_environment(worker, s5=True)
        assert environment["JOBFORGE_OTEL_EXPORTER"] == "otlp"
        assert "DEEPSEEK_API_KEY" not in environment
        assert ("JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE" in environment) == worker
        assert "JOBFORGE_OTEL_EXPORTER" not in child_environment(worker)
    monkeypatch.delenv("JOBFORGE_OTEL_EXPORTER")
    assert telemetry.setup() is None
    monkeypatch.setenv("JOBFORGE_OTEL_EXPORTER", "otlp")
    monkeypatch.setenv("JOBFORGE_OTEL_SAMPLE_RATIO", "nan")
    with pytest.raises(ValueError, match="INVALID_TRACE_CONFIGURATION"):
        telemetry.setup()
    assert os.environ["DEEPSEEK_API_KEY"] == "synthetic-do-not-inherit"
