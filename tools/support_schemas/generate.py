"""Generate versioned support schemas from shared constraints and explicit deltas."""

from __future__ import annotations

import argparse
import copy
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SOURCE = Path(__file__).resolve().parent


def generated() -> dict[Path, bytes]:
    """Preserve key order and each historical file's trailing newlines."""
    base = json.loads((SOURCE / "base.json").read_bytes())
    versions = json.loads((SOURCE / "versions.json").read_bytes())
    result = {}
    for version, specification in versions.items():
        schema = copy.deepcopy(base)
        for pointer, value in specification["replace"].items():
            path = pointer.strip("/").split("/")
            parent = schema
            for key in path[:-1]:
                parent = parent[key]
            parent[path[-1]] = value
        for pointer in specification["remove"]:
            path = pointer.strip("/").split("/")
            parent = schema
            for key in path[:-1]:
                parent = parent[key]
            del parent[path[-1]]
        for pointer, keys in specification["order"].items():
            parent = schema
            for key in pointer.strip("/").split("/") if pointer else []:
                parent = parent[key]
            ordered = {key: parent[key] for key in keys}
            parent.clear()
            parent.update(ordered)
        result[ROOT / "api/support" / version / "profile-schema.json"] = (
            json.dumps(schema, ensure_ascii=False, indent=2) + specification["trailing"]
        ).encode()
    return result


def main() -> None:
    """Check by default; writing requires an explicit generation command."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--write", action="store_true")
    args = parser.parse_args()
    for path, raw in generated().items():
        if args.write:
            path.write_bytes(raw)
        elif path.read_bytes() != raw:
            raise SystemExit(f"schema differs: {path.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
