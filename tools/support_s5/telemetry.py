"""Explicit bounded SDK export for the external S5 operator only."""

from __future__ import annotations

import logging
import os
import threading

from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.trace.sampling import TraceIdRatioBased


def setup() -> TracerProvider | None:
    """Use deployment OTLP opt-in; never print collector connection details."""
    if os.environ.get("JOBFORGE_OTEL_EXPORTER") != "otlp":
        return None
    ratio = float(os.environ.get("JOBFORGE_OTEL_SAMPLE_RATIO", "1"))
    if not 0 <= ratio <= 1:
        raise ValueError("INVALID_TRACE_CONFIGURATION")
    endpoint = os.environ["OTEL_EXPORTER_OTLP_ENDPOINT"].rstrip("/")
    logging.getLogger("opentelemetry.exporter.otlp.proto.http.trace_exporter").setLevel(
        logging.CRITICAL
    )
    provider = TracerProvider(
        resource=Resource.create({"service.name": "jobforge-support-s5-sdk"}),
        sampler=TraceIdRatioBased(ratio),
        shutdown_on_exit=False,
    )
    provider.add_span_processor(
        BatchSpanProcessor(
            OTLPSpanExporter(endpoint=endpoint + "/v1/traces", timeout=2),
            max_queue_size=256,
            max_export_batch_size=64,
            schedule_delay_millis=1000,
            export_timeout_millis=2000,
        )
    )
    trace.set_tracer_provider(provider)
    return provider


def stop(provider: TracerProvider) -> None:
    """Allow two seconds to drain; telemetry cannot delay launcher reaping."""
    draining = threading.Thread(target=provider.shutdown, daemon=True)
    draining.start()
    draining.join(timeout=2)
