"""Run quick, affected or full free checks from the repository root."""

from __future__ import annotations

import argparse
import json
import os
import shlex
import shutil
import subprocess
import sys

from tools.ci_checks import (
    CHECKS,
    GO_TEST_SKIP_PATTERN,
    ROOT,
    git_paths,
    runtime_tests,
    select_checks,
    validate_runtime_log,
)

PYTHON_TESTS = (
    ("tools/ci_tests",),
    ("tools/support_schemas",),
    ("sdk/python/tests",),
    ("tools/agent_probe_data", "tools/executorprobe"),
    ("python/tests",),
    ("tools/support_evaluation",),
    ("tools/support_recovery",),
    ("tools/support_approval",),
    ("tools/support_s5", "tools/support_lifecycle", "tools/support_diagnosis"),
)
GOLANGCI_VERSION = "2.12.2"


def tool(name: str) -> str:
    """Prefer the documented repository-local binary, then PATH."""
    filename = name + (".exe" if os.name == "nt" else "")
    local = ROOT / ".tools/bin" / filename
    return str(local) if local.is_file() else shutil.which(name) or name


def commands(check: str, base: str) -> list[list[str]]:
    """Keep host test and lint commands shared by local runs and CI."""
    py = [sys.executable, "-m"]
    if check == "go-lint":
        return [
            ["go", "build", "./..."],
            ["go", "vet", "./..."],
            [tool("golangci-lint"), "run"],
            ["go", "run", "github.com/rhysd/actionlint/cmd/actionlint@v1.7.12"],
        ]
    if check == "go-test":
        # Real executor acceptance runs once in Linux shards with a no-skip guard.
        return [
            ["go", "test", "-race", "-count=1", "-skip", GO_TEST_SKIP_PATTERN, "./..."]
        ]
    if check == "python-lint":
        # Preserve the existing suite boundaries and their pytest configurations.
        result = [py + ["pytest", *paths] for paths in PYTHON_TESTS] + [
            py + ["ruff", "check", "."],
            py + ["ruff", "format", "--check", "."],
            py + ["mypy", "sdk/python"],
        ]
        for paths in (
            ["python/jobforge_agent"],
            ["tools/agent_model_probe.py", "tools/executorprobe/executor.py"],
            ["--explicit-package-bases", "tools/support_evaluation"],
            [
                "--explicit-package-bases",
                "tools/support_recovery",
                "tools/agentruntimecheck/recovery_supervisor_check.py",
            ],
            ["--explicit-package-bases", "tools/support_approval"],
            [
                "--explicit-package-bases",
                "tools/support_s5",
                "tools/support_lifecycle",
                "tools/support_diagnosis",
            ],
            [
                "--explicit-package-bases",
                "tools/check.py",
                "tools/ci_checks.py",
                "tools/ci_docs.py",
                "tools/ci_images.py",
                "tools/support_schemas",
            ],
        ):
            result.append(py + ["mypy", "--platform", "linux", *paths])
        return result + [
            [sys.executable, "tools/check_sqlfluff_baseline.py"],
            py
            + ["sqlfluff", "lint", "migrations", "tools/support_diagnosis/migrations"],
        ]
    if check == "business-contract":
        return [
            [
                "go",
                "test",
                "-race",
                "-count=1",
                "./internal/business",
                "./internal/jsonstrict",
                "./tests/integration/business",
            ],
            [
                "docker",
                "build",
                "-f",
                "deploy/Dockerfile.business",
                "-t",
                "jobforge-business:checks",
                ".",
            ],
        ]
    if check == "executor-process-probe":
        build = [
            "docker",
            "build",
            "-f",
            "tools/executorprobe/Dockerfile",
            "-t",
            "jobforge-executor-probe:checks",
            ".",
        ]
        run = [
            "docker",
            "run",
            "--rm",
            "--init",
            "--network",
            "none",
            "--cpus",
            "1",
            "--memory",
            "256m",
            "--pids-limit",
            "64",
            "jobforge-executor-probe:checks",
        ]
        return [build, run, run + ["./clock.test", "-test.v", "-test.timeout=30s"]]
    if check == "proto-lint":
        return [
            [tool("buf"), "lint"],
            [tool("buf"), "breaking", "--against", f".git#ref={base}"],
        ]
    if check == "image-contract":
        return [[sys.executable, "-m", "tools.ci_images"]]
    if check == "observability-config":
        result = [
            [sys.executable, f"tools/generate_{kind}_dashboard.py"]
            for kind in ("task", "run")
        ]
        result += [
            [
                "git",
                "diff",
                "--exit-code",
                "--",
                "deploy/grafana/dashboards/jobforge-tasks.json",
                "deploy/grafana/run-dashboards/jobforge-runs.json",
            ]
        ]
        prom = [
            "docker",
            "run",
            "--rm",
            "--mount",
            f"type=bind,source={ROOT / 'deploy/prometheus'},target=/etc/prometheus,readonly",
            "--entrypoint",
            "promtool",
            "prom/prometheus:v3.14.0",
        ]
        return result + [
            prom + operation
            for operation in (
                ["check", "config", "/etc/prometheus/prometheus.yml"],
                ["test", "rules", "/etc/prometheus/alerts.test.yml"],
                ["check", "config", "/etc/prometheus/run.yml"],
                ["test", "rules", "/etc/prometheus/run-alerts.test.yml"],
            )
        ]
    if check == "agent-runtime-contract":
        build = ["docker", "build", "-f", "tools/agentruntimecheck/Dockerfile"]
        run = ["docker", "run", "--rm", "--init", "--network", "none"]
        result = [
            build
            + [
                "--target",
                "process-check",
                "-t",
                "jobforge-runtime:checks-process",
                ".",
            ],
            run
            + [
                "--cpus",
                "1",
                "--memory",
                "384m",
                "--pids-limit",
                "64",
                "jobforge-runtime:checks-process",
            ],
            build
            + [
                "--target",
                "integration-check",
                "-t",
                "jobforge-runtime:checks-integration",
                ".",
            ],
            run
            + [
                "jobforge-runtime:checks-integration",
                "/app/worker.test",
                "-test.v",
                "-test.timeout=120s",
            ],
        ]
        # Local groups run sequentially on the explicitly supplied disposable DB.
        # CI shards have separate runners and service databases.
        for names in runtime_tests().values():
            container = [
                "docker",
                "run",
                "--rm",
                "--init",
                "--add-host",
                "control:127.0.0.1",
            ]
            if sys.platform == "linux":
                container += ["--add-host", "host.docker.internal:host-gateway"]
            for key in ("JOBFORGE_TEST_DSN", "JOBFORGE_BUSINESS_TEST_DSN"):
                value = os.environ.get(key, "")
                value = value.replace("@localhost:", "@host.docker.internal:").replace(
                    "@127.0.0.1:", "@host.docker.internal:"
                )
                container += ["-e", f"{key}={value}"]
            result.append(
                container
                + [
                    "--cpus",
                    "2",
                    "--memory",
                    "512m",
                    "--pids-limit",
                    "96",
                    "jobforge-runtime:checks-integration",
                    "/app/integration.test",
                    "-test.v",
                    "-test.timeout=1200s",
                    "-test.run=^(" + "|".join(names) + ")$",
                ]
            )
        return result
    raise ValueError(f"unknown check: {check}")


def preflight(checks: list[str]) -> None:
    """Fail before a selected acceptance layer can silently skip its dependencies."""
    required: set[str] = set()
    if "go-lint" in checks:
        installed = json.loads(
            subprocess.run(
                [tool("golangci-lint"), "version", "--json"],
                capture_output=True,
                text=True,
                check=True,
                cwd=ROOT,
            ).stdout
        )
        if installed.get("version") != GOLANGCI_VERSION:
            raise ValueError(f"golangci-lint {GOLANGCI_VERSION} required")
    if "go-test" in checks:
        required |= {
            "JOBFORGE_TEST_DSN",
            "JOBFORGE_TEST_REDIS_URL",
            "JOBFORGE_TEST_REDIS_CONTAINER",
        }
    if "business-contract" in checks or "agent-runtime-contract" in checks:
        required.add("JOBFORGE_BUSINESS_TEST_DSN")
    if "agent-runtime-contract" in checks:
        required.add("JOBFORGE_TEST_DSN")
    missing = sorted(key for key in required if not os.environ.get(key))
    if missing:
        raise ValueError(
            "Missing disposable test services: "
            + ", ".join(missing)
            + "; see docs/tests.md"
        )
    if "go-test" in checks:
        result = subprocess.run(
            [
                "docker",
                "inspect",
                "--format",
                "{{json .HostConfig.PortBindings}}",
                os.environ["JOBFORGE_TEST_REDIS_CONTAINER"],
            ],
            cwd=ROOT,
            check=True,
            capture_output=True,
            text=True,
        )
        bindings = json.loads(result.stdout) or {}
        if any(
            not binding.get("HostPort")
            for values in bindings.values()
            for binding in (values or [])
        ):
            raise ValueError(
                "Redis restart tests require a fixed published host port; dynamic Docker ports change after stop/start."
            )
    if required:
        subprocess.run(
            [sys.executable, "-I", "-c", "import jobforge, httpx"], check=True, cwd=ROOT
        )


def free_environment(*, quick: bool) -> dict[str, str]:
    """Keep only disposable service settings, never inherited model/restore hooks."""
    allowed = (
        set()
        if quick
        else {
            "JOBFORGE_TEST_DSN",
            "JOBFORGE_BUSINESS_TEST_DSN",
            "JOBFORGE_TEST_REDIS_URL",
            "JOBFORGE_TEST_REDIS_CONTAINER",
        }
    )
    env = {
        key: value
        for key, value in os.environ.items()
        if not key.upper().startswith("JOBFORGE_") or key.upper() in allowed
    }
    env["PYTHONPATH"] = os.pathsep.join(
        str(ROOT / path) for path in ("python", "sdk/python", ".")
    )
    env["MYPYPATH"] = os.pathsep.join(
        str(ROOT / path) for path in ("python", "sdk/python")
    )
    if not quick:
        env["JOBFORGE_TEST_PYTHON"] = sys.executable
    # Check this checkout's executor sources even when an older wheel is installed.
    for name in ("PYTHONPATH", "MYPYPATH"):
        env[name] = str(ROOT / "python") + (
            os.pathsep + env[name] if env.get(name) else ""
        )
    return env


def run(command: list[str], env: dict[str, str], *, dry_run: bool) -> None:
    """Preserve command exit codes and keep connection strings out of progress logs."""
    shown = ["<test DSN>" if "DSN=" in part else part for part in command]
    print("+ " + shlex.join(shown), flush=True)
    if not dry_run and "/app/integration.test" in command:
        lines = []
        with subprocess.Popen(
            command,
            cwd=ROOT,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            encoding="utf-8",
            errors="replace",
        ) as process:
            assert process.stdout is not None
            for line in process.stdout:
                print(line, end="", flush=True)
                if "--- PASS:" in line or "--- SKIP:" in line:
                    lines.append(line)
            if process.wait() != 0:
                raise SystemExit(f"Runtime check failed (exit {process.returncode}).")
        pattern = next(value for value in command if value.startswith("-test.run="))
        expected = pattern.removeprefix("-test.run=^(").removesuffix(")$").split("|")
        validate_runtime_log("".join(lines), expected)
    elif not dry_run:
        subprocess.run(command, cwd=ROOT, env=env, check=True)


def main() -> None:
    """Run the selected free layers; quick never boots services or calls models."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "mode", nargs="?", choices=("quick", "affected", "full"), default="quick"
    )
    parser.add_argument("--base", default="origin/main")
    parser.add_argument("--only", choices=CHECKS)
    parser.add_argument(
        "--plan", action="store_true", help="print commands without executing"
    )
    args = parser.parse_args()
    env = free_environment(quick=args.mode == "quick" and not args.only)
    if args.mode == "quick" and not args.only:
        tasks = [
            [
                sys.executable,
                "-m",
                "pytest",
                "tools/ci_tests",
                "sdk/python/tests",
                "-q",
            ],
            [sys.executable, "-m", "ruff", "check", "."],
            [sys.executable, "-m", "ruff", "format", "--check", "."],
            ["go", "test", "./internal/...", "./cmd/...", "./tools/..."],
        ]
        print(
            "Quick feedback only; database, Linux process and model acceptance are not selected."
        )
    else:
        checks = (
            [args.only]
            if args.only
            else list(CHECKS)
            if args.mode == "full"
            else select_checks(git_paths(args.base, local=True))
        )
        print("Selected checks: " + ", ".join(checks), flush=True)
        if not args.plan:
            preflight(checks)
        tasks = [] if args.only else [[sys.executable, "-m", "tools.ci_docs"]]
        tasks += [command for check in checks for command in commands(check, args.base)]
    for command in tasks:
        run(command, env, dry_run=args.plan)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as exc:
        raise SystemExit(
            f"Check failed (exit {exc.returncode}); see command output above."
        ) from None
    except (ValueError, OSError) as exc:
        raise SystemExit(str(exc)) from None
