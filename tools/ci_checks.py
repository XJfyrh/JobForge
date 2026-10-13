"""Select required checks from Git paths and validate the final CI result."""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CHECKS = (
    "go-lint",
    "go-test",
    "python-lint",
    "business-contract",
    "executor-process-probe",
    "proto-lint",
    "agent-runtime-contract",
    "image-contract",
    "observability-config",
)
GO = {"go-lint", "go-test"}
RUNTIME = GO | {
    "python-lint",
    "business-contract",
    "agent-runtime-contract",
    "image-contract",
}
FULL = set(CHECKS)
RUNTIME_PATTERN = r"^TestRun(Executor|SupportExecutor|SupportS5|SupportAgentExecutor|SupportLauncher|ProviderAuditExecutor|Recovery|ApprovalExecutor)"
SLOW_RECOVERY = {
    "TestRunRecoveryExecutorNaturalLoss",
    "TestRunRecoveryExecutorCommitBoundary",
    "TestRunRecoveryExternalProxyAndSupervisor",
}
DEADLINES = {
    "TestRunRecoveryExecutorAttemptDeadline",
    "TestRunRecoveryExecutorRecoveryLimit",
}


def select_checks(paths: list[str]) -> list[str]:
    """Use conservative fallbacks; executable data is never classified by suffix."""
    selected: set[str] = set()
    for path in paths:
        if path.startswith(("examples/", "api/", "proto/", "migrations/")):
            selected |= RUNTIME
            if path.startswith("proto/"):
                selected.add("proto-lint")
        elif path.startswith(("docs/product/", "docs/adr/")):
            selected |= FULL
        elif path in {"AGENTS.md", "CONTRIBUTING.md"}:
            selected |= FULL
        elif {"testdata", "fixture", "fixtures", "gold", "policies"}.intersection(
            path.split("/")
        ):
            selected |= FULL
        elif (
            path.startswith("docs/")
            or path in {"README.md", "SECURITY.md"}
            or path.endswith("/README.md")
        ):
            # Unknown non-Markdown files may be manifests consumed by tooling.
            if not path.endswith(".md") and not path.startswith("docs/evidence/"):
                selected |= FULL
            elif not path.endswith((".md", ".json")):
                selected |= FULL
            elif path.endswith(".json"):
                selected.add("python-lint")
        elif path.startswith(
            ("deploy/prometheus/", "deploy/grafana/", "deploy/otel/")
        ) or path in {
            "tools/generate_task_dashboard.py",
            "tools/generate_run_dashboard.py",
        }:
            selected |= {"observability-config", "python-lint"}
        elif path.startswith(
            (".github/", "tools/ci_", "tools/check", "tools/test-windows")
        ):
            selected |= FULL
        elif path.startswith(("python/", "sdk/python/", "tools/agentruntimecheck/")):
            selected |= RUNTIME
        elif path.startswith("tools/executorprobe/"):
            selected |= GO | {"python-lint", "executor-process-probe"}
        elif path.startswith("internal/runclock/"):
            selected |= RUNTIME | {"executor-process-probe"}
        elif path.startswith(("internal/", "cmd/")):
            selected |= RUNTIME
            if path.startswith("internal/observability/"):
                selected.add("observability-config")
        elif path.startswith("tests/integration/business/"):
            selected |= {"go-lint", "business-contract", "agent-runtime-contract"}
        elif path.startswith("tests/"):
            selected |= GO | {"agent-runtime-contract"}
        elif path.startswith("tools/support_diagnosis/"):
            selected |= {"python-lint"}
            if path.endswith(".go"):
                selected |= GO
        elif path.startswith(
            (
                "tools/support_evaluation/",
                "tools/support_s5/",
                "tools/support_approval/",
                "tools/support_lifecycle/",
            )
        ):
            selected |= {"python-lint", "image-contract"}
        elif path.startswith(
            (
                "tools/support_recovery/",
                "tools/supportrecoveryproxy/",
                "tools/supportapprovalproxy/",
            )
        ):
            selected |= RUNTIME
        elif (
            path.startswith("tools/agent_probe_data/")
            or path == "tools/agent_model_probe.py"
        ):
            selected.add("python-lint")
        elif path.startswith("tools/clockcheck/") or path.startswith("benchmarks/"):
            selected |= GO
        elif path.startswith("deploy/"):
            selected |= FULL
        else:
            selected |= FULL
    return [check for check in CHECKS if check in selected]


def git_paths(base: str, *, local: bool = False) -> list[str]:
    """Include both sides of renames and all local edits without parsing filenames."""
    commands = [["diff", "--name-only", "--no-renames", "-z", f"{base}...HEAD"]]
    if local:
        commands += [
            ["diff", "--name-only", "--no-renames", "-z", "HEAD"],
            ["ls-files", "--others", "--exclude-standard", "-z"],
        ]
    paths: set[str] = set()
    for args in commands:
        result = subprocess.run(
            ["git", *args], cwd=ROOT, check=True, capture_output=True
        )
        paths.update(p.decode("utf-8") for p in result.stdout.split(b"\0") if p)
    return sorted(paths)


def runtime_tests(root: Path = ROOT) -> dict[str, list[str]]:
    """Partition the original suite completely, keeping new matching tests in core."""
    names: set[str] = set()
    for path in (root / "tests/integration").glob("*_test.go"):
        names.update(
            re.findall(
                r"^func (Test\w+)\(\s*(?:\w+\s+)?\*testing\.T\s*,?\s*\)",
                path.read_text(encoding="utf-8"),
                re.MULTILINE,
            )
        )
    names = {name for name in names if re.match(RUNTIME_PATTERN, name)}
    if not (SLOW_RECOVERY | DEADLINES) <= names:
        raise ValueError("runtime partition references removed or renamed tests")
    return {
        "core": sorted(names - SLOW_RECOVERY - DEADLINES),
        "recovery": sorted(SLOW_RECOVERY),
        "deadlines": sorted(DEADLINES),
    }


def validate_gate(needs: dict[str, dict], selected: list[str]) -> None:
    """Reject missing, failed, cancelled, or unexpectedly skipped required jobs."""
    if not set(selected) <= FULL:
        raise ValueError("unknown check in plan")
    expected = {"changes", "docs", *CHECKS}
    if set(needs) != expected:
        raise ValueError("gate dependencies do not match the check catalog")
    for name, job in needs.items():
        result = job.get("result")
        required = name in {"changes", "docs", *selected}
        if result != "success" and not (not required and result == "skipped"):
            raise ValueError(f"{name}: {result}; required={required}")


def validate_runtime_log(output: str, expected: list[str]) -> None:
    """Require every selected top-level runtime test and reject environment skips."""
    if "--- SKIP:" in output:
        raise ValueError("unexpected skip in selected runtime acceptance")
    passed = set(re.findall(r"^--- PASS: (Test\w+) \(", output, re.MULTILINE))
    missing = set(expected) - passed
    if missing:
        raise ValueError("runtime tests did not pass: " + ", ".join(sorted(missing)))


def main() -> None:
    """Expose the plan, runtime partition and gate to GitHub Actions."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "command", choices=("plan", "runtime", "verify-runtime", "gate")
    )
    parser.add_argument("--base", default="origin/main")
    parser.add_argument("--full", action="store_true")
    parser.add_argument("--local", action="store_true")
    parser.add_argument("--shard", choices=("core", "recovery", "deadlines"))
    parser.add_argument("--log", type=Path)
    args = parser.parse_args()
    if args.command == "gate":
        validate_gate(
            json.loads(os.environ["CI_NEEDS"]), json.loads(os.environ["CI_SELECTED"])
        )
        print("All required checks passed.")
    elif args.command == "verify-runtime":
        if not args.shard or not args.log:
            parser.error("verify-runtime requires --shard and --log")
        validate_runtime_log(
            args.log.read_text(encoding="utf-8"), runtime_tests()[args.shard]
        )
    elif args.command == "runtime":
        shards = runtime_tests()
        if args.shard:
            print("^(" + "|".join(shards[args.shard]) + ")$")
        else:
            print(json.dumps(shards, indent=2))
    else:
        paths = [] if args.full else git_paths(args.base, local=args.local)
        selected = list(CHECKS) if args.full else select_checks(paths)
        print(json.dumps({"paths": paths, "checks": selected}, indent=2))
        if output := os.environ.get("GITHUB_OUTPUT"):
            with Path(output).open("a", encoding="utf-8") as stream:
                for check in CHECKS:
                    stream.write(f"{check}={str(check in selected).lower()}\n")
                stream.write("selected=" + json.dumps(selected) + "\n")
        if summary := os.environ.get("GITHUB_STEP_SUMMARY"):
            with Path(summary).open("a", encoding="utf-8") as stream:
                stream.write(
                    "### Required checks\n\n"
                    + "\n".join(f"- `{check}`" for check in selected)
                    + "\n\nDocs and workflow checks always run.\n"
                )


if __name__ == "__main__":
    main()
