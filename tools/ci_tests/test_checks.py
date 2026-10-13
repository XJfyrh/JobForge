"""Guard check selection, missing-result failures and runtime suite coverage."""

from __future__ import annotations

import copy
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from tools.check import commands, free_environment, preflight
from tools.ci_checks import (
    CHECKS,
    DEADLINES,
    SLOW_RECOVERY,
    git_paths,
    runtime_tests,
    select_checks,
    validate_gate,
    validate_runtime_log,
)
from tools.ci_docs import check_link, headings, targets
from tools.ci_images import main as check_images


class SelectionTests(unittest.TestCase):
    """Keep cheap changes cheap without bypassing executable data or unknown paths."""

    def test_documentation_and_observability(self) -> None:
        """Plain docs do not start databases; alert changes validate alert behavior."""
        self.assertEqual(
            select_checks(["docs/development.md", "sdk/python/README.md"]), []
        )
        self.assertEqual(
            set(select_checks(["deploy/prometheus/run-alerts.yml"])),
            {"python-lint", "observability-config"},
        )

    def test_protected_inputs_and_unknown_paths(self) -> None:
        """Contracts, executable Markdown and unmapped paths cannot take the docs path."""
        for path in (
            "examples/support-agent/runtime/policies/P03.md",
            "api/executor/v2/fixtures.json",
            "python/jobforge_agent/step.py",
            "sdk/python/jobforge/run_client.py",
            "migrations/0030_future.up.sql",
        ):
            with self.subTest(path=path):
                self.assertIn("agent-runtime-contract", select_checks([path]))
        for path in (
            "new-component/config.json",
            "new-component/instructions.md",
            "tests/testdata/README.md",
            "python/fixtures/README.md",
            "go.mod",
            "docs/adr/0036-new.md",
            ".github/workflows/ci.yml",
        ):
            self.assertEqual(select_checks([path]), list(CHECKS))

    def test_offline_tools_do_not_select_runtime(self) -> None:
        """Offline scorer edits still check packaging but avoid natural lease waits."""
        self.assertEqual(
            set(select_checks(["tools/support_s5/score.py"])),
            {"python-lint", "image-contract"},
        )
        self.assertEqual(
            set(select_checks(["tools/support_diagnosis/ledger.go"])),
            {"python-lint", "go-lint", "go-test"},
        )

    def test_changed_paths_union_is_monotonic(self) -> None:
        """A README addition cannot suppress the checks for another changed file."""
        first = select_checks(["internal/run/ledger.go"])
        self.assertEqual(select_checks(["README.md", "internal/run/ledger.go"]), first)
        self.assertIn(
            "proto-lint", select_checks(["proto/jobforge/agent/v1/agent.proto"])
        )

    def test_git_rename_and_uncommitted_inputs(self) -> None:
        """Preserve removed runtime paths across renames and include unstaged inputs."""
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)

            def git(*args: str) -> None:
                subprocess.run(
                    ["git", *args], cwd=root, check=True, capture_output=True
                )

            git("init", "--quiet")
            (root / "python").mkdir()
            (root / "python/old.py").write_text("original\n", encoding="utf-8")
            git("add", ".")
            git(
                "-c",
                "user.name=CI Test",
                "-c",
                "user.email=ci@example.invalid",
                "commit",
                "--quiet",
                "-m",
                "fixture",
            )
            (root / "docs").mkdir()
            git("mv", "python/old.py", "docs/moved source.md")
            (root / "untracked.json").write_text("{}", encoding="utf-8")
            with patch("tools.ci_checks.ROOT", root):
                self.assertEqual(
                    git_paths("HEAD", local=True),
                    ["docs/moved source.md", "python/old.py", "untracked.json"],
                )
                self.assertEqual(git_paths("HEAD"), [])


class GateTests(unittest.TestCase):
    """Selected jobs must succeed even when GitHub treats job skips as neutral."""

    def test_required_results(self) -> None:
        """Reject cancellations, missing jobs, unknown jobs and accidental skips."""
        selected = ["go-test"]
        needs = {name: {"result": "skipped"} for name in CHECKS}
        needs.update(
            {name: {"result": "success"} for name in ("changes", "docs", "go-test")}
        )
        validate_gate(needs, selected)
        for name in ("changes", "docs", "go-test"):
            for result in ("failure", "cancelled", "skipped", None):
                broken = copy.deepcopy(needs)
                broken[name]["result"] = result
                with self.assertRaises(ValueError):
                    validate_gate(broken, selected)
        del needs["go-test"]
        with self.assertRaises(ValueError):
            validate_gate(needs, selected)

    def test_missing_services_fail_before_tests(self) -> None:
        """A locally requested acceptance suite cannot silently lose its PG layer."""
        with patch.dict("os.environ", {}, clear=True):
            with self.assertRaisesRegex(ValueError, "JOBFORGE_TEST_DSN"):
                preflight(["go-test"])
            with self.assertRaisesRegex(ValueError, "JOBFORGE_BUSINESS_TEST_DSN"):
                preflight(["agent-runtime-contract"])

    def test_free_checks_cannot_inherit_paid_or_restore_hooks(self) -> None:
        """Even full checks must not opt into live models or manual restore exercises."""
        inherited = {
            "JOBFORGE_REAL_MODEL_URL": "https://example.invalid",
            "JOBFORGE_S5_REVIEW_DIR": "private-evidence",
            "JOBFORGE_TEST_WORKER_HELPER": "1",
            "JOBFORGE_TEST_DSN": "disposable",
            "PATH": "tools",
        }
        with patch.dict("os.environ", inherited, clear=True):
            env = free_environment(quick=False)
            self.assertEqual(env["JOBFORGE_TEST_DSN"], "disposable")
            for key in (
                "JOBFORGE_REAL_MODEL_URL",
                "JOBFORGE_S5_REVIEW_DIR",
                "JOBFORGE_TEST_WORKER_HELPER",
            ):
                self.assertNotIn(key, env)
            self.assertFalse(
                any(key.startswith("JOBFORGE_") for key in free_environment(quick=True))
            )

    def test_dynamic_redis_port_is_rejected_before_restart_tests(self) -> None:
        """Docker may reassign a dynamic published port after the test restarts Redis."""
        env = {
            "JOBFORGE_TEST_DSN": "disposable",
            "JOBFORGE_TEST_REDIS_URL": "redis://localhost:1234",
            "JOBFORGE_TEST_REDIS_CONTAINER": "owned-test-redis",
        }
        result = subprocess.CompletedProcess(
            [], 0, '{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":""}]}'
        )
        with (
            patch.dict("os.environ", env, clear=True),
            patch("tools.check.subprocess.run", return_value=result),
        ):
            with self.assertRaisesRegex(ValueError, "fixed published host port"):
                preflight(["go-test"])


class RuntimeTests(unittest.TestCase):
    """Keep every original runtime test in exactly one isolated shard."""

    def test_partition_includes_new_tests_and_rejects_removed_anchors(self) -> None:
        """New matching tests default to core instead of disappearing from CI."""
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            directory = root / "tests/integration"
            directory.mkdir(parents=True)
            names = (
                SLOW_RECOVERY
                | DEADLINES
                | {
                    "TestRunSupportFuture",
                    "TestRunSupportExecutorFuture",
                    "TestUnrelated",
                }
            )
            source = directory / "future_test.go"
            source.write_text(
                "\n".join(f"func {name}(t *testing.T) {{}}" for name in names),
                encoding="utf-8",
            )
            shards = runtime_tests(root)
            self.assertEqual(shards["core"], ["TestRunSupportExecutorFuture"])
            self.assertEqual(set(shards["recovery"]), SLOW_RECOVERY)
            self.assertEqual(set(shards["deadlines"]), DEADLINES)
            source.write_text("", encoding="utf-8")
            with self.assertRaises(ValueError):
                runtime_tests(root)

    def test_repository_suite_and_commands(self) -> None:
        """Every real shard is populated and keeps a bounded production-time test run."""
        shards = runtime_tests()
        self.assertTrue(all(shards.values()))
        flat = [name for names in shards.values() for name in names]
        self.assertEqual(len(flat), len(set(flat)))
        runtime = commands("agent-runtime-contract", "HEAD")
        self.assertEqual(
            sum("-test.timeout=1200s" in command for command in runtime), 3
        )

    def test_empty_partial_and_skipped_runs_fail(self) -> None:
        """A successful test binary exit cannot hide a mismatched selector or skip."""
        expected = ["TestRunExecutorA", "TestRunExecutorB"]
        output = (
            "--- PASS: TestRunExecutorA (1.00s)\n--- PASS: TestRunExecutorB (2.00s)\n"
        )
        validate_runtime_log(output, expected)
        for broken in (
            "PASS\n",
            output.splitlines()[0],
            output + "    --- SKIP: TestRunExecutorA/child (0.00s)\n",
        ):
            with self.assertRaises(ValueError):
                validate_runtime_log(broken, expected)


class ImageTests(unittest.TestCase):
    """Preserve host-only boundaries when image checks move out of the workflow."""

    def test_s5_sources_exclude_host_guard_and_replay(self) -> None:
        """Verify deployed sources without requiring host-only modules in the image."""
        with (
            patch("tools.ci_images.docker"),
            patch("tools.ci_images.verify_sources") as verify,
        ):
            check_images()
        _, paths, _, absent = verify.call_args.args
        names = {path.name for path in paths}
        self.assertTrue({"freeze.py", "score.py"} <= names)
        for name in ("guard.py", "replay.py"):
            self.assertNotIn(name, names)
            self.assertIn(f"/app/tools/support_s5/{name}", absent)
        self.assertIn("/app/tools/support_diagnosis", absent)
        self.assertIn("/app/examples/support-agent/evaluation", absent)


class DocsTests(unittest.TestCase):
    """Validate links consistently on Windows and Linux without editing inputs."""

    def test_case_and_unicode_anchor(self) -> None:
        """Reject wrong-case paths and absent headings on case-insensitive hosts."""
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder).resolve()
            source = root / "README.md"
            source.write_text("# 测试入口\n# 测试入口\n", encoding="utf-8")
            self.assertIsNone(check_link(root, source, "README.md#测试入口-1"))
            self.assertIsNotNone(check_link(root, source, "readme.md"))
            self.assertIsNotNone(check_link(root, source, "README.md#missing"))
            self.assertEqual(headings("# `run` check\n"), {"run-check"})

    def test_code_samples_and_external_links(self) -> None:
        """Ignore fenced example destinations but validate actual Markdown links."""
        self.assertEqual(
            targets("```md\n[x](missing.md)\n```\n[y](real.md)"), ["real.md"]
        )
        self.assertIsNone(
            check_link(Path.cwd(), Path.cwd() / "README.md", "https://example.com/a#b")
        )


if __name__ == "__main__":
    unittest.main()
