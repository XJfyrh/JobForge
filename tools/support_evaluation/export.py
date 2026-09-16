"""Read Run evidence through the SDK, retaining exact accepted JSON numbers."""

from __future__ import annotations

import hashlib
import json
import os
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import httpx
from jobforge import RunClient


class ExportError(ValueError):
    """A fixed diagnostic; protected response content is never a message."""


def json_bytes(value: Any) -> bytes:
    """Encode a private export, never a log or trace payload."""
    return (
        json.dumps(value, ensure_ascii=False, allow_nan=False, indent=2) + "\n"
    ).encode()


def atomic_json(path: Path, value: Any) -> None:
    """Flush a row transition before the driver makes its next SDK request."""
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("wb") as target:
        os.chmod(temporary, 0o600)
        target.write(json_bytes(value))
        target.flush()
        os.fsync(target.fileno())
    os.replace(temporary, path)
    if os.name == "posix":
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)


class CaptureTransport(httpx.BaseTransport):
    """Observe the SDK's single exchange without issuing another request.

    The private response archive contains protected steps. Credentials, request
    headers and request bodies are never copied. This is control API evidence,
    not evidence that a model HTTP request reached its provider.
    """

    def __init__(self, archive: Path, inner: httpx.BaseTransport | None = None) -> None:
        """Capture one client's responses into its fresh archive."""
        self.inner = inner if inner is not None else httpx.HTTPTransport(retries=0)
        self.archive = archive
        self.last: bytes | None = None
        self.sequence = 0

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        """Delegate exactly once and retain response bytes before SDK parsing."""
        self.last = None
        response = self.inner.handle_request(request)
        raw = response.read()
        if len(raw) > 2 * 1024 * 1024:
            raise ExportError("API_RESPONSE_TOO_LARGE")
        self.sequence += 1
        # Each export uses a fresh exclusive directory; filenames never replace
        # earlier samples, including samples taken before late reports arrive.
        name = f"{self.sequence:04d}.json"
        with (self.archive / name).open("xb") as target:
            os.chmod(target.name, 0o600)
            target.write(raw)
        atomic_json(
            self.archive / f"{self.sequence:04d}.receipt.json",
            {
                "captured_at": datetime.now(UTC).isoformat(),
                "method": request.method,
                "path": request.url.path,
                "query": request.url.query.decode("ascii"),
                "status": response.status_code,
                "bytes": len(raw),
                "sha256": hashlib.sha256(raw).hexdigest(),
                "file": name,
            },
        )
        self.last = raw
        return response

    def close(self) -> None:
        """Release the underlying transport."""
        self.inner.close()

    def object(self) -> dict[str, Any]:
        """Use only the response already validated by the preceding SDK call."""
        if self.last is None:
            raise ExportError("API_RESPONSE_MISSING")
        value: dict[str, Any] = json.loads(self.last)
        return value


class _Number(str):
    """A token from the same validated response, before float conversion."""


def _raw_json(value: Any) -> str:
    if isinstance(value, _Number):
        return str(value)
    if isinstance(value, dict):
        return (
            "{"
            + ",".join(
                json.dumps(key, ensure_ascii=False) + ":" + _raw_json(child)
                for key, child in value.items()
            )
            + "}"
        )
    if isinstance(value, list):
        return "[" + ",".join(_raw_json(child) for child in value) + "]"
    return json.dumps(value, ensure_ascii=False, allow_nan=False)


def export_run(
    client: RunClient, capture: CaptureTransport, run_id: str
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """Fetch bounded complete pages once; missing pages never count as complete."""
    client.get(run_id)
    run = capture.object()
    client.result(run_id)
    result = capture.object()
    steps: list[dict[str, Any]] = []
    after = 0
    for _ in range(33):
        page = client.steps(run_id, after=after, limit=100)
        document = capture.object()
        exact = json.loads(capture.last or b"", parse_int=_Number, parse_float=_Number)
        for record, tokens in zip(document["items"], exact["items"], strict=True):
            if record["sequence"] <= after or len(steps) >= 32:
                raise ExportError("STEP_PAGE_INCOMPLETE")
            after = record["sequence"]
            steps.append({"record": record, "output_json": _raw_json(tokens["output"])})
        if page.next_after is None:
            break
        if not page.items or page.next_after != after:
            raise ExportError("STEP_PAGE_INCOMPLETE")
    else:
        raise ExportError("STEP_PAGE_INCOMPLETE")
    events: list[dict[str, Any]] = []
    after = 0
    for _ in range(100):
        event_page = client.events(run_id, after=after, limit=100)
        document = capture.object()
        for event in document["items"]:
            if event["sequence"] <= after:
                raise ExportError("EVENT_PAGE_INCOMPLETE")
            after = event["sequence"]
            events.append(event)
        if event_page.next_after is None:
            break
        if not event_page.items or event_page.next_after != after:
            raise ExportError("EVENT_PAGE_INCOMPLETE")
    else:
        raise ExportError("EVENT_PAGE_INCOMPLETE")
    client.calls(run_id)
    calls = capture.object()
    return {"run": run, "steps": steps, "result": result, "calls": calls}, events
