"""Build and verify production and external operator image boundaries."""

from __future__ import annotations

import hashlib
import json
import subprocess
import tempfile
from pathlib import Path

from tools.ci_checks import ROOT


def docker(*args: str) -> None:
    """Execute Docker without shell interpolation."""
    subprocess.run(["docker", *args], cwd=ROOT, check=True)


def verify_sources(
    image: str, paths: list[Path], imports: str, absent: list[str]
) -> None:
    """Compare installed operator sources with the checked-out source bytes."""
    with tempfile.TemporaryDirectory(prefix="jobforge-image-check-") as folder:
        manifest = Path(folder) / "expected.json"
        expected = {
            path.as_posix(): hashlib.sha256((ROOT / path).read_bytes()).hexdigest()
            for path in paths
        }
        manifest.write_text(json.dumps(expected), encoding="utf-8")
        script = (
            "import hashlib,json,sys; from pathlib import Path; sys.path.insert(0,'/app'); "
            + imports
            + "; expected=json.loads(Path('/tmp/expected.json').read_bytes()); "
            "assert expected; assert all(hashlib.sha256((Path('/app')/path).read_bytes()).hexdigest()==digest for path,digest in expected.items()); "
            + f"assert all(not Path(path).exists() for path in {absent!r}); "
            "print('Installed operator sources verified:',len(expected))"
        )
        docker(
            "run",
            "--rm",
            "--init",
            "--network",
            "none",
            "--entrypoint",
            "python",
            "--mount",
            f"type=bind,source={manifest},target=/tmp/expected.json,readonly",
            image,
            "-I",
            "-c",
            script,
        )


def main() -> None:
    """Check package boundaries without running models or test databases."""
    worker = "jobforge-agent-worker:checks-production"
    approval = "jobforge-support-approval:checks"
    s5 = "jobforge-support-s5:checks"
    docker("build", "-f", "deploy/Dockerfile.agent-worker", "-t", worker, ".")
    script = (
        "from jobforge_agent.runtime_registry import REGISTRY; "
        "from jobforge_agent.dispatch import DEEPSEEK_ORIGIN; "
        "from pathlib import Path; from importlib.util import find_spec; "
        "assert set(REGISTRY)=={'support-fixed-v1','support-agent-v1'}; "
        "assert REGISTRY['support-fixed-v1'].__class__.__module__=='jobforge_agent.support_adapter'; "
        "assert REGISTRY['support-agent-v1'].__class__.__module__=='jobforge_agent.support_agent'; "
        "assert DEEPSEEK_ORIGIN=='https://api.deepseek.com'; "
        "assert find_spec('jobforge_agent.runtime_recovery_faults') is None; "
        "assert all(not Path(p).exists() for p in ['/tmp/runtime_recovery_faults.py','/tmp/test_install.py','/etc/jobforge/executor.json','/checks','/src/examples/support-agent/evaluation'])"
    )
    docker(
        "run",
        "--rm",
        "--init",
        "--network",
        "none",
        "--entrypoint",
        "python",
        worker,
        "-I",
        "-c",
        script,
    )
    docker(
        "build",
        "--build-arg",
        f"WORKER_IMAGE={worker}",
        "-f",
        "deploy/Dockerfile.support-approval",
        "-t",
        approval,
        ".",
    )
    paths = [
        path.relative_to(ROOT)
        for folder in ("tools/support_approval", "tools/support_evaluation")
        for path in (ROOT / folder).iterdir()
        if path.suffix in {".py", ".sql"}
        and not path.name.startswith("test_")
        and path.name != "fixtures.py"
    ]
    paths += [
        Path("tools/support_recovery") / name
        for name in ("driver.py", "plan.py", "continuation.py")
    ]
    verify_sources(
        approval,
        paths,
        "from tools.support_approval.driver import verify_release; from tools.support_approval.report import report",
        [],
    )
    docker(
        "build",
        "--build-arg",
        f"OPERATOR_IMAGE={approval}",
        "-f",
        "deploy/Dockerfile.support-s5",
        "-t",
        s5,
        ".",
    )
    # Host replay and submission guard are manifest-bound, not operator sources.
    paths = [
        path.relative_to(ROOT)
        for path in (ROOT / "tools/support_s5").iterdir()
        if path.suffix == ".py"
        and not path.name.startswith("test_")
        and path.name not in {"replay.py", "guard.py"}
    ]
    verify_sources(
        s5,
        paths,
        "from tools.support_s5.driver import main; from tools.support_s5.freeze import validate; from tools.support_s5.score import score_export",
        [
            "/app/examples/support-agent/evaluation",
            "/app/tools/support_s5/replay.py",
            "/app/tools/support_s5/guard.py",
            "/app/tools/support_s5/host",
            "/app/tools/support_diagnosis",
        ],
    )


if __name__ == "__main__":
    main()
