"""Streaming export bounds, decoded archives and transport cleanup."""

from __future__ import annotations

import gzip
import json
from collections.abc import Iterator
from pathlib import Path

import httpx
import pytest

from tools.support_evaluation.export import CaptureTransport, ExportError


class Body(httpx.SyncByteStream):
    """Track consumption and closure of a synthetic streamed response."""

    def __init__(self, chunks: Iterator[bytes]) -> None:
        """Own only the supplied deterministic stream."""
        self.chunks = chunks
        self.closed = False
        self.reads = 0

    def __iter__(self) -> Iterator[bytes]:
        """Expose how many transport chunks the client requested."""
        for chunk in self.chunks:
            self.reads += 1
            yield chunk

    def close(self) -> None:
        """Record cleanup on both successful and failed reads."""
        self.closed = True


def test_capture_stops_before_unbounded_body_and_writes_no_receipt(
    tmp_path: Path,
) -> None:
    """Oversized evidence cannot consume the rest of the transport stream."""

    def chunks() -> Iterator[bytes]:
        for _ in range(257):
            yield b" " * 8192
        pytest.fail("capture continued after limit")

    body = Body(chunks())
    capture = CaptureTransport(
        tmp_path, httpx.MockTransport(lambda _: httpx.Response(200, stream=body))
    )
    with httpx.Client(transport=capture) as client:
        with pytest.raises(ExportError, match="API_RESPONSE_TOO_LARGE"):
            client.get("http://synthetic.invalid/v2/runs")
    assert body.closed and body.reads == 257
    assert capture.last is None and capture.sequence == 0
    assert not list(tmp_path.iterdir())


def test_capture_closes_failed_read_without_recording_complete_evidence(
    tmp_path: Path,
) -> None:
    """An interrupted read must not create a completed archive receipt."""

    def chunks() -> Iterator[bytes]:
        yield b"{"
        raise httpx.ReadError("synthetic stream failure")

    body = Body(chunks())
    capture = CaptureTransport(
        tmp_path, httpx.MockTransport(lambda _: httpx.Response(200, stream=body))
    )
    with httpx.Client(transport=capture) as client:
        with pytest.raises(httpx.ReadError):
            client.get("http://synthetic.invalid/v2/runs")
    assert body.closed
    assert capture.last is None and not list(tmp_path.iterdir())


def test_capture_decodes_compressed_body_once_and_preserves_archive(
    tmp_path: Path,
) -> None:
    """Archived bytes and SDK bytes remain equal after compression decoding."""
    raw = b'{"items":[],"next_cursor":null}'
    compressed = gzip.compress(raw)
    body = Body(iter([compressed]))
    capture = CaptureTransport(
        tmp_path,
        httpx.MockTransport(
            lambda _: httpx.Response(
                200,
                stream=body,
                headers={
                    "Content-Encoding": "gzip",
                    "Content-Length": str(len(compressed)),
                },
            )
        ),
    )
    with httpx.Client(transport=capture) as client:
        response = client.get("http://synthetic.invalid/v2/runs")
    assert response.content == raw
    assert response.json() == json.loads(raw)
    assert capture.last == raw and body.closed
    assert (tmp_path / "0001.json").read_bytes() == raw
    assert json.loads((tmp_path / "0001.receipt.json").read_bytes())["bytes"] == len(
        raw
    )
