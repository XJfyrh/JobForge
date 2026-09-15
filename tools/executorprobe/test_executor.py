"""Deterministic request guardrails; real process acceptance runs in Linux Docker."""

import importlib.util
import io
import json
import sys
from pathlib import Path
from types import ModuleType, SimpleNamespace
from unittest.mock import MagicMock, Mock

import pytest


@pytest.fixture
def executor() -> ModuleType:
    """Load definitions without running the Linux-only entry point."""
    spec = importlib.util.spec_from_file_location(
        "executor_probe_guard", Path(__file__).with_name("executor.py")
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_valid_request_is_data(
    executor: ModuleType, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Keep metacharacters and Unicode inside a JSON string."""
    value = {"v": 1, "id": "step", "op": "echo", "value": "$(shell); 文档\n"}
    stream = io.TextIOWrapper(io.BytesIO(json.dumps(value).encode() + b"\n"))
    monkeypatch.setattr(sys, "stdin", stream)
    assert executor.read_request() == value


@pytest.mark.parametrize(
    "raw",
    [
        b"not json\n",
        b'{"v":true,"id":"step","op":"echo","value":""}\n',
        b'{"v":2,"id":"step","op":"echo","value":""}\n',
        b'{"v":1,"id":"step","op":"sh","value":""}\n',
        b'{"v":1,"id":"step","op":"echo","value":"","code":"x"}\n',
        b"{}",
        b"x" * 4096 + b"\n",
    ],
)
def test_invalid_request_never_reaches_dispatch(
    executor: ModuleType, monkeypatch: pytest.MonkeyPatch, raw: bytes
) -> None:
    """Reject shape, version, operation, framing and size errors."""
    monkeypatch.setattr(sys, "stdin", io.TextIOWrapper(io.BytesIO(raw)))
    with pytest.raises(ValueError):
        executor.read_request()


@pytest.mark.parametrize("combined_read", [False, True])
def test_guard_rejects_trailing_fragment_after_valid_result(
    executor: ModuleType,
    monkeypatch: pytest.MonkeyPatch,
    combined_read: bool,
) -> None:
    """Keep frame validation independent of how the OS partitions pipe reads."""
    request = {"v": 1, "id": "step", "op": "echo", "value": "synthetic"}
    result = b'{"v":1,"id":"step","kind":"result","value":"synthetic"}\n'
    tail = b"invalid trailing bytes"
    chunks = [b"READY\n", result, tail, b""]
    if combined_read:
        chunks = [b"READY\n" + result + tail, b""]
    child = Mock()
    child.pid = 123
    child.stdin = io.BytesIO()
    child.wait.return_value = 0
    child.poll.return_value = 0
    selector = MagicMock()
    selector.__enter__.return_value = selector
    selector.select.return_value = [(SimpleNamespace(data="result"), 1)]
    stdout = io.TextIOWrapper(io.BytesIO())
    monkeypatch.setattr(sys, "stdout", stdout)
    monkeypatch.setattr(
        sys, "stdin", io.TextIOWrapper(io.BytesIO(json.dumps(request).encode() + b"\n"))
    )
    monkeypatch.setattr(executor.os, "getpgrp", executor.os.getpid, raising=False)
    monkeypatch.setattr(executor.os, "read", Mock(side_effect=chunks))
    monkeypatch.setattr(executor.subprocess, "Popen", Mock(return_value=child))
    monkeypatch.setattr(
        executor.selectors, "DefaultSelector", Mock(return_value=selector)
    )

    with pytest.raises(ValueError, match="unterminated response frame"):
        executor.guard()

    assert tail not in stdout.buffer.getvalue()
    child.stdout.close.assert_called_once()
