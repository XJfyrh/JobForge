"""Run one separately approved, isolated real-provider acceptance without key files."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import signal
import subprocess
import time
import urllib.request
from datetime import UTC, datetime
from pathlib import Path

PRICING = "https://api-docs.deepseek.com/zh-cn/quick_start/pricing/"
BATCH_MICROYUAN = 3_000_000
PROJECT_MICROYUAN = 50_000_000
# Reviewed public rate snapshot; any change requires a fresh price review.
PRICE_SHA256 = "5a7b1832592387340f2fc456399b34b89b05f3fa167c2e35909e2fa4afe021e3"


def docker(*args: str) -> str:
    """Run Docker without logging command environments or private stdin."""
    result = subprocess.run(["docker", *args], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError("docker operation failed")
    return result.stdout.strip()


def validate_prior_exposure(prior: int) -> None:
    """Require cumulative known costs and unreleased holds before admitting a batch."""
    if (
        type(prior) is not int
        or prior < 0
        or prior + BATCH_MICROYUAN > PROJECT_MICROYUAN
    ):
        raise ValueError("cumulative exposure leaves insufficient authorized budget")


def receipt_exposure(receipt: dict) -> tuple[int, int]:
    """Release the provisional bound only against consistent, complete PG evidence."""
    known, held = (
        receipt.get("known_cost_microyuan"),
        receipt.get("held_cost_microyuan"),
    )
    if (
        type(known) is not int
        or type(held) is not int
        or known < 0
        or held < 0
        or known + held > BATCH_MICROYUAN
        or receipt.get("batch_cap_microyuan") != BATCH_MICROYUAN
    ):
        raise ValueError("invalid receipt; retain full batch hold and stop")
    calls = receipt.get("calls", {}).get("items")
    if not isinstance(calls, list):
        raise ValueError("missing call evidence; retain full batch hold and stop")
    costs = [
        (row.get("known_cost_microyuan"), row.get("held_cost_microyuan"))
        for row in calls
    ]
    if any(
        type(k) is not int or type(h) is not int or k < 0 or h < 0 for k, h in costs
    ):
        raise ValueError("invalid call exposure")
    if sum(k for k, _ in costs) != known or sum(h for _, h in costs) != held:
        raise ValueError("call/account exposure mismatch")
    return known, held


def main() -> int:
    """Admit once, retain evidence, and clean only resources created by this run."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--execute-approved", action="store_true", required=True)
    parser.add_argument("--receipt-dir", type=Path, required=True)
    parser.add_argument("--prior-exposure-microyuan", type=int, required=True)
    parser.add_argument("--image", default="jobforge-review-runtime:provider-check")
    args = parser.parse_args()
    validate_prior_exposure(args.prior_exposure_microyuan)
    if not all(
        os.environ.get(k)
        for k in ("DEEPSEEK_API_KEY", "HTTPS_PROXY", "CODEX_PROXY_CERT")
    ):
        raise SystemExit("required existing credential/proxy/CA entry missing")
    # Existing directory is a one-shot barrier, including previous failed attempts.
    args.receipt_dir.mkdir(parents=True, exist_ok=False)
    evidence = args.receipt_dir.resolve()
    with urllib.request.urlopen(PRICING, timeout=15) as response:
        raw = response.read(600_000)
    if hashlib.sha256(raw).hexdigest() != PRICE_SHA256:
        raise SystemExit("price snapshot changed; review before any paid dispatch")
    page = raw.decode()
    if not all(
        value in page
        for value in ("deepseek-flash", "DeepSeek-V4.1-Flash", "0.04元", "2元", "8元")
    ):
        raise SystemExit("current price cannot be verified; no paid dispatch")
    (evidence / "public-pricing.html").write_bytes(raw)
    # This is an existing public trust certificate, never the API credential.
    ca = evidence / "public-proxy-ca.pem"
    shutil.copyfile(os.environ["CODEX_PROXY_CERT"], ca)
    ca.chmod(0o644)
    ledger = {
        "started_at": datetime.now(UTC).isoformat(),
        "prior_conservative_microyuan": args.prior_exposure_microyuan,
        "project_cap_microyuan": PROJECT_MICROYUAN,
        "batch_cap_microyuan": BATCH_MICROYUAN,
        "pending_upper_microyuan": BATCH_MICROYUAN,
        "state": "reserved_not_started",
        "account_debit_checked": False,
        "price_sha256": hashlib.sha256(raw).hexdigest(),
    }

    def save() -> None:
        with (evidence / "ledger.json").open("w") as file:
            json.dump(ledger, file, indent=2)
            file.flush()
            os.fsync(file.fileno())

    save()
    containers: list[str] = []
    proxy_names = (
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "NO_PROXY",
        "http_proxy",
        "https_proxy",
        "all_proxy",
        "no_proxy",
    )
    clear_proxy = [part for name in proxy_names for part in ("-e", name + "=")]

    def stop(_number, _frame) -> None:
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        pg = docker(
            "create",
            "--label",
            "jobforge.purpose=approved-provider-acceptance",
            "-p",
            "127.0.0.1::5432",
            "-e",
            "POSTGRES_USER=test",
            "-e",
            "POSTGRES_PASSWORD=test",
            "-e",
            "POSTGRES_DB=jobforge_test",
            "postgres:16-alpine",
        )
        containers.append(pg)
        docker("start", pg)
        for _ in range(60):
            ready = subprocess.run(
                [
                    "docker",
                    "exec",
                    pg,
                    "pg_isready",
                    "-U",
                    "test",
                    "-d",
                    "jobforge_test",
                ],
                capture_output=True,
            )
            if ready.returncode == 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError("isolated PostgreSQL not ready")
        port = docker("port", pg, "5432").removeprefix("127.0.0.1:")
        if not port.isdecimal():
            raise RuntimeError("unexpected PostgreSQL binding")
        runner = docker(
            "create",
            "--interactive",
            *clear_proxy,
            "--init",
            "--network",
            "host",
            "--label",
            "jobforge.purpose=approved-provider-acceptance",
            "--cpus",
            "2",
            "--memory",
            "512m",
            "--pids-limit",
            "96",
            "--ulimit",
            "core=0",
            "--mount",
            f"type=bind,src={ca},dst=/opt/jobforge-provider-ca.pem,readonly",
            "-e",
            f"JOBFORGE_TEST_DSN=postgres://test:test@127.0.0.1:{port}/jobforge_test?sslmode=disable",
            "-e",
            "JOBFORGE_REAL_WORKER_ACCEPTANCE=approved-single-run-v1",
            args.image,
        )
        containers.append(runner)
        config = json.loads(
            docker("inspect", "--format", "{{json .Config.Env}}", runner)
        )
        if any(
            v.split("=", 1)[0] == "DEEPSEEK_API_KEY"
            or (v.split("=", 1)[0] in proxy_names and v.partition("=")[2])
            for v in config
        ):
            raise RuntimeError("runner inherited forbidden ambient configuration")
        ledger["docker_config_contains_provider_secret"] = False
        ledger["state"] = "started_full_hold_until_receipt"
        save()
        private_input = json.dumps(
            {
                "api_key": os.environ["DEEPSEEK_API_KEY"],
                "proxy": os.environ["HTTPS_PROXY"],
                "ca": "/opt/jobforge-provider-ca.pem",
                "observed_on": datetime.now(UTC).strftime("%Y-%m-%d"),
                "price_sha": ledger["price_sha256"],
            }
        )
        # Credentials travel only over stdin and the local Docker connection.
        # No key enters argv, Docker's stored Env, build args, files or diagnostics.
        with (evidence / "run.log").open("w") as output:
            process = subprocess.Popen(
                ["docker", "start", "--attach", "--interactive", runner],
                stdin=subprocess.PIPE,
                stdout=output,
                stderr=subprocess.STDOUT,
                text=True,
            )
            try:
                process.communicate(private_input, timeout=210)
            finally:
                private_input = ""
                if process.poll() is None:
                    process.kill()
                    process.wait()
        exit_code = int(docker("inspect", "--format", "{{.State.ExitCode}}", runner))
        ledger["runner_exit_code"] = exit_code
        records = []
        for line in (evidence / "run.log").read_text().splitlines():
            if "REAL_PROVIDER_RECEIPT " in line:
                records.append(json.loads(line.split("REAL_PROVIDER_RECEIPT ", 1)[1]))
        if len(records) != 1:
            raise RuntimeError(
                "complete receipt unavailable; retain full batch hold and stop"
            )
        receipt = records[0]
        (evidence / "receipt.json").write_text(json.dumps(receipt, indent=2))
        known, held = receipt_exposure(receipt)
        ledger.update(
            known_cost_microyuan=known,
            held_cost_microyuan=held,
            pending_upper_microyuan=known + held,
            remaining_authorized_microyuan=PROJECT_MICROYUAN
            - args.prior_exposure_microyuan
            - known
            - held,
            state="receipt_retained_no_further_calls",
        )
        save()
        return (
            0
            if exit_code == 0 and held == 0 and receipt["state"] == "awaiting_approval"
            else 1
        )
    except (Exception, KeyboardInterrupt) as error:
        ledger["failure_type"] = type(error).__name__
        if ledger["state"] == "reserved_not_started":
            ledger["pending_upper_microyuan"] = 0
            ledger["state"] = "failed_before_worker_start_no_paid_dispatch"
        save()
        return 1
    finally:
        cleaned = True
        for container in reversed(containers):
            result = subprocess.run(
                ["docker", "rm", "-fv", container], capture_output=True
            )
            cleaned = cleaned and result.returncode == 0
        ledger["owned_containers_cleaned"] = cleaned
        ledger["finished_at"] = datetime.now(UTC).isoformat()
        save()
        print(json.dumps(ledger, indent=2))
        print(f"Evidence: {evidence}")


if __name__ == "__main__":
    raise SystemExit(main())
