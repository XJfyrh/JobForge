"""Deterministic request guardrails; real process acceptance runs in Linux Docker."""

import importlib.util
import io
import json
import sys
from pathlib import Path
from types import ModuleType

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
