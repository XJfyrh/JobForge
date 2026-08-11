"""Verify the frozen SQLFluff allowlist for applied historical migrations."""

from __future__ import annotations

import sys
from collections import Counter
from pathlib import Path

EXPECTED_ENTRIES = (
    "migrations/0006_add_trace_context.up.sql",
    "migrations/0006_add_trace_context.down.sql",
    "migrations/0013_create_tenant_quota_counters.up.sql",
)


def fail(message: str) -> int:
    """Report a baseline validation failure without reading SQL contents."""
    print(f"SQLFluff baseline invalid: {message}", file=sys.stderr)
    return 1


def main() -> int:
    """Validate the exact ordered baseline and all referenced paths."""
    repo_root = Path(__file__).resolve().parents[1]
    ignore_path = repo_root / ".sqlfluffignore"
    try:
        lines = ignore_path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        return fail(f"cannot read {ignore_path.name}: {exc}")

    entries = tuple(
        stripped
        for line in lines
        if (stripped := line.strip()) and not stripped.startswith("#")
    )
    duplicates = tuple(entry for entry, count in Counter(entries).items() if count > 1)
    if duplicates:
        return fail(f"duplicate entries: {', '.join(duplicates)}")
    if entries != EXPECTED_ENTRIES:
        return fail(
            "effective entries must exactly match the ordered frozen baseline; "
            f"expected {EXPECTED_ENTRIES!r}, got {entries!r}"
        )

    missing_paths = tuple(
        entry for entry in entries if not (repo_root / entry).is_file()
    )
    if missing_paths:
        return fail(f"paths do not exist: {', '.join(missing_paths)}")

    print(f"SQLFluff baseline valid: {len(entries)} historical migrations")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
