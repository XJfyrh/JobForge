"""Generate package-local license files from the repository's sole source."""

from __future__ import annotations

import argparse
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TARGETS = ("python/LICENSE", "sdk/python/LICENSE")


def main() -> None:
    """Keep standalone wheel/sdist builds licensed without parent-directory globs."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sync", action="store_true", help="regenerate package copies")
    parser.add_argument("--check", action="store_true", help="check without writing")
    args = parser.parse_args()
    if args.sync and args.check:
        parser.error("choose --sync or --check")
    source = (ROOT / "LICENSE").read_bytes()
    for relative in TARGETS:
        path = ROOT / relative
        if args.sync:
            path.write_bytes(source)
        if not path.is_file() or path.read_bytes() != source:
            raise SystemExit(f"{relative}: run python -m tools.package_licenses --sync")
    print("Package licenses match LICENSE byte for byte.")


if __name__ == "__main__":
    main()
