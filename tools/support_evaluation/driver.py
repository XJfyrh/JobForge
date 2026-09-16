"""One fixed 40-case SDK batch. No Worker RPC, model calls or automatic retries."""

from __future__ import annotations

import argparse
import hashlib
import json
import signal
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from jobforge import RunClient
from jobforge.errors import JobForgeError

from tools.support_evaluation.export import (
    CaptureTransport,
    ExportError,
    atomic_json,
    export_run,
)

CONFIG = Path("/etc/jobforge/cloud")
OUTPUT = Path("/var/lib/jobforge/exports")
KEYS = Path("/run/secrets/driver.json")
CONTROL = "http://control:8093"


class BatchStopped(RuntimeError):
    """The current invocation ends; export remains available."""


def instant(value: str) -> datetime:
    """Read the generated UTC batch deadline."""
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def continuation(evidence: dict[str, Any]) -> bool:
    """Check visible completion; PostgreSQL alone authorizes the next call.

    The public export cannot prove the internal attempt/session/fence barrier.
    These checks do not replace Claim/Reserve's persistent batch guard.
    """
    run, result, calls = evidence["run"], evidence["result"], evidence["calls"]
    if calls["batch_frozen"] or calls["batch_stop_code"] is not None:
        return False
    chat = [call for call in calls["items"] if call["subcall"] == "chat"]
    for call in chat:
        audit = call["provider_audit"]
        if (
            not call["usage_known"]
            or call["measurement_anomaly"]
            or call["report_hash"] is None
            or call["report_conflict"]
            or call["observed_at"] is None
            or audit is None
            or audit["identity_state"] != "compatible"
            or audit["mode_state"] != "nonthinking"
        ):
            return False
    steps = [item["record"] for item in evidence["steps"]]
    if not steps or not chat:
        return False
    if run["state"] in {"awaiting_approval", "succeeded"}:
        expected = "proposal" if run["state"] == "awaiting_approval" else "no_action"
        return bool(
            result["available"]
            and result["kind"] == expected
            and result["ref"]
            == (
                run["proposal_ref"]
                if expected == "proposal"
                else steps[-1]["output_ref"]
            )
            and steps[-1]["kind"] == "submit_proposal"
        )
    return bool(
        run["state"] == "failed"
        and run["error"] is not None
        and run["error"]["code"] == "MODEL_PROTOCOL_ERROR"
        and len(chat) == 2
        and steps[-1]["kind"] == "model_proposal"
        and steps[-1]["output"]["correction_required"]
        and all(call["business_outcome"] == "rejected" for call in chat)
        and all(call["error_code"] == "MODEL_PROTOCOL_ERROR" for call in chat)
    )


class Driver:
    """Own only case bookkeeping and single-exchange SDK calls."""

    def __init__(
        self, manifest: dict[str, Any], output: Path, keys: dict[str, str]
    ) -> None:
        """Bind the fixed manifest and fresh private output directory."""
        self.manifest, self.output, self.keys = manifest, output, keys
        self.stopped = False
        self.deadline = instant(manifest["valid_until"])
        cases = manifest["cases"]
        if len(cases) != 40 or len({row["case_id"] for row in cases}) != 40:
            raise BatchStopped("CASE_REGISTRATION_INVALID")
        if [row["ordinal"] for row in cases] != list(range(1, 41)):
            raise BatchStopped("CASE_ORDER_INVALID")
        self.rows = [
            dict(row, status="unattempted", run_id=None, error_code="") for row in cases
        ]
        self.state_path = output / "rows.json"

    def stop(self, *_: Any) -> None:
        """Latch a local stop; this cannot refund or cancel an external call."""
        self.stopped = True

    def check(self) -> None:
        """Do not submit or poll beyond the already frozen batch window."""
        if self.stopped:
            raise BatchStopped("OPERATOR_STOP")
        if datetime.now(UTC) >= self.deadline:
            raise BatchStopped("BATCH_DEADLINE")

    def save(self) -> None:
        """Persist all cases, including failures and unattempted rows."""
        atomic_json(self.state_path, {"schema_version": 1, "cases": self.rows})

    def run(self) -> None:
        """Persist all rows and each attempt before making its sole Submit."""
        if self.state_path.exists():
            raise BatchStopped("BATCH_ALREADY_ATTEMPTED")
        self.save()
        for row in self.rows:
            self.check()
            archive = self.output / row["case_id"]
            archive.mkdir(mode=0o700)
            capture = CaptureTransport(archive)
            with RunClient(
                CONTROL, self.keys[row["tenant_id"]], timeout=5, transport=capture
            ) as client:
                row["status"] = "submission_attempted"
                self.save()
                try:
                    self.check()
                    remaining = int((self.deadline - datetime.now(UTC)).total_seconds())
                    if remaining < 120:
                        raise BatchStopped("BATCH_DEADLINE")
                    accepted = client.submit(
                        row["ticket_id"],
                        row["business_request_key"],
                        self.manifest["profile_id"],
                        self.manifest["batch_key"],
                        idempotency_key=row["idempotency_key"],
                        run_timeout_seconds=min(360, remaining),
                    )
                    row["run_id"], row["status"] = accepted.run.run_id, "accepted"
                    self.save()
                    while True:
                        self.check()
                        current = client.get(accepted.run.run_id)
                        if current.state.value in {
                            "awaiting_approval",
                            "succeeded",
                            "failed",
                            "cancelled",
                        }:
                            break
                        if any(
                            account.frozen
                            for account in (
                                current.budget.family,
                                current.budget.tenant,
                                current.budget.batch,
                            )
                        ):
                            raise BatchStopped("BATCH_FROZEN")
                        time.sleep(0.25)
                    evidence, events = export_run(client, capture, accepted.run.run_id)
                    atomic_json(archive / "evidence.json", evidence)
                    atomic_json(archive / "events.json", events)
                    row["status"] = "finished"
                    self.save()
                    if not continuation(evidence):
                        raise BatchStopped("CASE_BARRIER_INCOMPLETE")
                except (JobForgeError, ExportError, OSError, BatchStopped) as exc:
                    row["error_code"] = (
                        str(exc)
                        if isinstance(exc, BatchStopped)
                        else type(exc).__name__
                    )
                    # A POST may have committed even if response archival failed.
                    # Keep attempted/accepted identity; never generate another key.
                    self.save()
                    raise BatchStopped("BATCH_STOPPED") from None


def export_known(
    manifest: dict[str, Any], original: Path, target: Path, keys: dict[str, str]
) -> None:
    """Append a new read-only snapshot of known IDs; never repeat Submit."""
    rows = json.loads((original / "rows.json").read_text(encoding="utf-8"))["cases"]
    if [row["case_id"] for row in rows] != [
        row["case_id"] for row in manifest["cases"]
    ]:
        raise BatchStopped("CASE_REGISTRATION_INVALID")
    for row in rows:
        if row["run_id"] is None:
            continue
        archive = target / row["case_id"]
        archive.mkdir(mode=0o700)
        capture = CaptureTransport(archive)
        with RunClient(
            CONTROL, keys[row["tenant_id"]], timeout=5, transport=capture
        ) as client:
            evidence, events = export_run(client, capture, row["run_id"])
            atomic_json(archive / "evidence.json", evidence)
            atomic_json(archive / "events.json", events)
    atomic_json(target / "rows.json", {"schema_version": 1, "cases": rows})


def main() -> int:
    """The launch mode is invoked only by the fixed parent supervisor."""
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("launch", "export"))
    parser.add_argument("--original", type=Path)
    args = parser.parse_args()
    manifest = json.loads((CONFIG / "launch.json").read_text(encoding="utf-8"))
    keys = json.loads(KEYS.read_text(encoding="utf-8"))
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%S.%fZ")
    output = OUTPUT / stamp
    output.mkdir(mode=0o700)
    atomic_json(
        output / "registration-receipt.json",
        {
            "launch_sha256": hashlib.sha256(
                (CONFIG / "launch.json").read_bytes()
            ).hexdigest()
        },
    )
    try:
        if args.mode == "export":
            if args.original is None:
                raise BatchStopped("ORIGINAL_EXPORT_REQUIRED")
            export_known(manifest, args.original, output, keys)
        else:
            driver = Driver(manifest, output, keys)
            signal.signal(signal.SIGTERM, driver.stop)
            signal.signal(signal.SIGINT, driver.stop)
            driver.run()
        atomic_json(output / "finished.json", {"complete": True})
        return 0
    except (JobForgeError, ExportError, OSError, BatchStopped):
        atomic_json(output / "finished.json", {"complete": False})
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
