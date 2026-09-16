"""Adversarial OS peer, packaged only in the process-test image.

This deliberately violates production protocol/lifecycle rules. No production
entry point imports this module or accepts its mode argument.
"""

import json
import os
import signal
import sys

from jobforge_agent.provider_audit import (
    ReportBinding,
    decode_call_report,
    execution_binding_hash,
)


def write(fd: int, value: dict) -> None:
    """Send a full synthetic frame without unbounded test buffering."""
    wire = json.dumps(value, separators=(",", ":")).encode() + b"\n"
    while wire:
        wire = wire[os.write(fd, wire) :]


def main() -> None:
    """Exercise a single explicitly selected test-only failure."""
    mode = sys.argv[1]
    with open("../../api/executor/v2/fixtures/frames.json", encoding="utf-8") as source:
        frames = json.load(source)["valid_frames"]
    report = next(frame for frame in frames if frame["kind"] == "metering_report")
    result = next(frame for frame in frames if frame["kind"] == "step_result")
    signal.signal(signal.SIGTERM, signal.SIG_IGN)

    if mode in {
        "block_input",
        "block_metering",
        "full_events",
        "full_metering",
        "residual",
    }:
        write(1, result)
        if mode == "full_events":
            while True:
                write(1, result)
        if mode == "full_metering":
            while True:
                write(5, report)
        if mode == "residual":
            if os.fork() != 0:
                os._exit(0)
        while True:
            signal.pause()

    request = json.loads(sys.stdin.buffer.readline())
    for key in ("version", "request_id", "binding", "emitted_mono_ms"):
        result[key] = report[key] = request[key]
    content = decode_call_report(
        json.dumps(
            {"usage": report["usage"], "provider_audit": report["provider_audit"]}
        ).encode()
    )
    report["report_hash"] = content.hash(
        ReportBinding(
            execution_binding_hash(report["binding"]),
            report["physical_call_id"],
            report["parameter_hash"],
            "chat",
            "deepseek-flash",
        )
    )
    if mode in {"metering_closed_timeout", "metering_closed_bad_frame"}:
        signal.signal(signal.SIGUSR1, lambda _sig, _frame: os._exit(69))
        os.close(4)
        if mode == "metering_closed_bad_frame":
            # Test controls this barrier separately from the closed ACK reader.
            signal.signal(
                signal.SIGUSR2, lambda _sig, _frame: os.write(5, b"invalid\n")
            )
        write(5, report)
        while True:
            signal.pause()
    if mode.startswith("exit_"):
        os.close(1)
        os._exit(int(mode.removeprefix("exit_")))
    if mode == "signal":
        os.kill(os.getpid(), signal.SIGKILL)
    if mode == "wrong_ordinary":
        write(1, request)
    elif mode == "wrong_metering":
        report["kind"] = "metering_ack"
        report.pop("parameter_hash")
        report.pop("usage")
        report.pop("provider_audit")
        report["settlement"] = "settled"
        write(5, report)
    elif mode == "bad_then_metering":
        signal.signal(signal.SIGTERM, lambda _sig, _frame: write(5, report))
        os.write(1, b'{"synthetic_secret":"never disclose"}\n')
        while True:
            signal.pause()
    elif mode == "oversize":
        os.write(1, b"x" * (384 * 1024))
    elif mode == "metering_oversize":
        os.write(5, b"x" * 8192)
    elif mode == "fragment":
        os.write(1, b'{"kind":')
    elif mode == "metering_fragment":
        os.write(5, b'{"kind":')
    else:
        write(1, result)
        if mode == "trailing":
            write(1, result)
        if mode == "late_stderr":
            signal.signal(signal.SIGUSR1, lambda _sig, _frame: os.write(2, b"x" * 8193))
            os.close(1)
            while True:
                signal.pause()
    os._exit(0)


if __name__ == "__main__":
    main()
