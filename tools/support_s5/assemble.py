"""Prepare S5 registration and bind original request/business/receipt audits."""

from __future__ import annotations

import argparse
from pathlib import Path

from tools.support_evaluation.assemble import assemble, read, register, save_new, sha
from tools.support_evaluation.evidence import load_package as development_package
from tools.support_s5.freeze import validate_blueprint
from tools.support_s5.package import load_package


def main() -> None:
    """All inputs are explicit local artifacts; no execution is started."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("register", "assemble"))
    parser.add_argument("--package", type=Path)
    parser.add_argument("--freeze", type=Path)
    parser.add_argument(
        "--seen-diagnostic",
        action="store_true",
        help="historical v1 package; excluded from unseen acceptance",
    )
    parser.add_argument("--out", required=True, type=Path)
    for name in (
        "config",
        "registration",
        "archive",
        "metadata",
        "before",
        "after",
        "receipts-before",
        "receipts-after",
    ):
        parser.add_argument("--" + name, type=Path)
    args = parser.parse_args()
    if (args.package is None) != (args.freeze is None):
        parser.error("formal package and prior freeze must be supplied together")
    if args.seen_diagnostic and args.package is None:
        parser.error("seen diagnostic requires its original v1 package and freeze")
    package = (
        load_package(args.package, args.freeze, historical=args.seen_diagnostic)
        if args.package is not None
        else development_package()
    )
    if args.mode == "register":
        if args.config is None:
            parser.error("registration requires prepared config")
        result = register(args.config, package=package, s5=True)
        if args.freeze is not None and not args.seen_diagnostic:
            validate_blueprint(
                read(args.freeze),
                result["profile"]["strategy"],
                result["strategy_blueprint"],
            )
    else:
        paths = (
            args.registration,
            args.archive,
            args.metadata,
            args.before,
            args.after,
            args.receipts_before,
            args.receipts_after,
        )
        if any(p is None for p in paths):
            parser.error(
                "assembly requires original export and both business/receipt audits"
            )
        result = assemble(*paths[:5], package=package, s5=True)
        before, after = (
            args.receipts_before.read_bytes(),
            args.receipts_after.read_bytes(),
        )
        result["receipt_audit"] = {
            "before_raw": before.decode(),
            "after_raw": after.decode(),
            "before_sha256": sha(before),
            "after_sha256": sha(after),
        }
    save_new(args.out, result)


if __name__ == "__main__":
    main()
