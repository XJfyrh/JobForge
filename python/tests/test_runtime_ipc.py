"""Real Linux dual-pipe routing, EOF and trailing-frame process regressions."""

from __future__ import annotations

import contextlib
import os
import select
import subprocess
import sys
from collections.abc import Iterator
from pathlib import Path

import pytest
from jobforge_agent.protocol_v2 import (
    decode,
    decode_metering,
    encode,
    encode_metering,
    observation_hash,
)
from test_dispatch import start_frame

pytestmark = pytest.mark.skipif(
    sys.platform != "linux", reason="actual Linux nonblocking inherited FDs required"
)


@contextlib.contextmanager
def child(mode: str = "result") -> Iterator[tuple[subprocess.Popen[bytes], int, int]]:
    """Keep the Go-equivalent downlink open until after the real child exits."""
    read_in, write_in = os.pipe()
    read_out, write_out = os.pipe()
    process = subprocess.Popen(
        [
            sys.executable,
            "-u",
            str(Path(__file__).with_name("runtime_ipc_child.py")),
            mode,
            str(read_in),
            str(write_out),
        ],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        pass_fds=(read_in, write_out),
    )
    os.close(read_in)
    os.close(write_out)
    try:
        yield process, write_in, read_out
    finally:
        if process.poll() is None:
            process.kill()
        process.wait(timeout=3)
        for stream in (process.stdin, process.stdout, process.stderr):
            assert stream is not None
            stream.close()
        os.close(write_in)
        os.close(read_out)


def send(process: subprocess.Popen[bytes], value: bytes) -> None:
    """Write only a bounded test fixture."""
    assert process.stdin is not None
    process.stdin.write(value)
    process.stdin.flush()


def test_result_exits_without_commit_ack_or_parent_eof() -> None:
    """The child exits with the parent input still open, then both outputs EOF."""
    with child() as (process, _, meter):
        send(process, encode(start_frame("read_ticket")))
        assert process.wait(timeout=3) == 0
        assert process.stdout is not None
        result = decode(process.stdout.readline())
        assert result["kind"] == "step_result"
        assert process.stdout.read() == b""
        assert os.read(meter, 1) == b""


@pytest.mark.parametrize("tail", [b"{}\n", b"{", b" ", b"\n"])
def test_extra_or_partial_ordinary_frame_never_disappears(tail: bytes) -> None:
    """Read-ready trailing bytes make cleanup fail even after a candidate result."""
    with child() as (process, _, _):
        send(process, encode(start_frame("read_ticket")) + tail)
        assert process.wait(timeout=3) == 65


def test_metering_reader_failure_wakes_ordinary_waiter() -> None:
    """A broken independent metering lane closes ordinary continuation."""
    with child("wait") as (process, metering, _):
        send(process, encode(start_frame()))
        os.write(metering, b"{}\n")
        assert process.wait(timeout=3) == 65


def test_wrong_permit_binding_rejected_before_return() -> None:
    """The transport's route cannot deliver an ACK for a different request."""
    with child("authorize") as (process, _, _):
        send(process, encode(start_frame()))
        assert process.stdout is not None
        ready, _, _ = select.select([process.stdout], [], [], 3)
        assert ready
        intent = decode(process.stdout.readline())
        intent.update(
            kind="call_permit",
            request_id="00000000-0000-4000-8000-000000000099",
            physical_call_id="00000000-0000-4000-8000-000000000020",
            granted=True,
            error_code="",
            dispatch_ms=500,
            call_ms=2000,
            input_token_limit=2000,
            output_token_limit=1024,
        )
        send(process, encode(intent))
        assert process.wait(timeout=3) == 65


def test_independent_lanes_accept_ordinary_ack_before_metering_ack() -> None:
    """A waiting metering exchange cannot block reception on the ordinary lane."""
    with child("dual") as (process, metering, report_fd):
        send(process, encode(start_frame()))
        assert process.stdout is not None
        ready, _, _ = select.select([process.stdout], [], [], 3)
        assert ready
        observation = decode(process.stdout.readline())
        report_bytes = bytearray()
        while not report_bytes.endswith(b"\n"):
            ready, _, _ = select.select([report_fd], [], [], 3)
            assert ready
            chunk = os.read(report_fd, 8192)
            assert chunk, "metering EOF before a complete report"
            report_bytes.extend(chunk)
            assert len(report_bytes) <= 8192
        report = decode_metering(bytes(report_bytes))
        fields = (
            "version",
            "request_id",
            "binding",
            "emitted_mono_ms",
            "call_sequence",
            "physical_call_id",
        )
        ack = {key: observation[key] for key in fields}
        ack.update(
            kind="call_observation_ack", observation_hash=observation_hash(observation)
        )
        send(process, encode(ack))
        assert process.poll() is None
        meter_ack = {key: report[key] for key in fields}
        meter_ack.update(
            kind="metering_ack",
            report_hash=report["report_hash"],
            settlement="settled",
        )
        os.write(metering, encode_metering(meter_ack))
        assert process.wait(timeout=3) == 0
