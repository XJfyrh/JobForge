"""Verify failed barrier restoration cannot leave a success receipt."""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace
from typing import BinaryIO, cast

import pytest

from tools.support_lifecycle import exercise


def test_hba_failure_attempts_both_and_records_failed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A completed dump/restore is still failed if either access gate remains."""
    review = tmp_path / "review"
    review.mkdir()
    (review / "paused.ready").write_text(
        json.dumps({"http_stopped": True, "workers_stopped": True})
    )
    (review / "environment.json").write_text(
        json.dumps(
            {
                "cloud_calls": 0,
                "source_kind": "synthetic_mechanism_review",
                "control_database": "jobforge_run_test_" + "a" * 32,
                "business_database": "jobforge_s4_" + "b" * 32,
            }
        )
    )
    restored = []

    def command(args: list[str], data: bytes | None = None) -> bytes:
        if args[:2] == ["docker", "top"]:
            return b"docker-init integration.test"
        if "cat" in args:
            return b"original-hba"
        if data == b"original-hba":
            restored.append(args[3])
            if args[3] == "business":
                raise RuntimeError("synthetic restore failure")
        return b"source-head"

    def sql(_container: str, user: str, _database: str, query: str) -> bytes:
        if query == "show hba_file":
            return b"/var/lib/postgresql/data/pg_hba.conf"
        if "count(*) from pg_stat_activity" in query:
            return b"0"
        if "json_agg(rolname)" in query:
            return json.dumps([user]).encode()
        if "json_agg(json_build_array" in query:
            return b"[]"
        return b"t"

    def subprocess_run(args: list[str], **kwargs: object) -> SimpleNamespace:
        if "pg_dump" in args:
            cast(BinaryIO, kwargs["stdout"]).write(b"synthetic-dump")
        return SimpleNamespace(returncode=0)

    monkeypatch.setattr(exercise, "command", command)
    monkeypatch.setattr(exercise, "sql", sql)
    monkeypatch.setattr(exercise, "snapshot", lambda *_: {"table": {"rows": 1}})
    monkeypatch.setattr(exercise.subprocess, "run", subprocess_run)
    output = tmp_path / "failed-output"
    with pytest.raises(RuntimeError, match="HBA restore failed"):
        exercise.exercise(review, output, "review", "control", "business")
    assert restored == ["business", "control"]
    report = json.loads((output / "manifest.json").read_bytes())
    assert report["passed"] is False
    assert report["hba_restore_failures"] == ["business"]
