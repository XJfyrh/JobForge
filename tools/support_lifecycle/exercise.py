"""Stopped dual-database backup/restore of an explicitly owned free review.

Custom-format dumps contain protected content and stay outside the repository.
Restore uses fresh, network-isolated containers, with all application roles
NOLOGIN. This utility never starts a Worker, model client or business writer.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import time
import uuid
from pathlib import Path
from typing import Any

IMAGE = "pgvector/pgvector:0.8.6-pg16-bookworm@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b"


def command(args: list[str], data: bytes | None = None) -> bytes:
    """Run a bounded command without exposing protected failure output."""
    result = subprocess.run(args, input=data, capture_output=True, timeout=120)
    if result.returncode:
        # PostgreSQL diagnostics can include protected row contents.
        raise RuntimeError("backup/restore command failed; protected output withheld")
    return result.stdout


def sql(container: str, user: str, database: str, query: str) -> bytes:
    """Use a local administrator socket and fixed generated SQL."""
    return command(
        [
            "docker",
            "exec",
            "-i",
            container,
            "psql",
            "-X",
            "-v",
            "ON_ERROR_STOP=1",
            "-U",
            user,
            "-d",
            database,
            "-At",
        ],
        query.encode(),
    ).strip()


def snapshot(container: str, user: str, database: str) -> dict[str, Any]:
    """Return per-table row counts and canonical content digests."""
    tables = json.loads(
        sql(
            container,
            user,
            database,
            "select json_agg(s) from (select table_schema,table_name from information_schema.tables where table_type='BASE TABLE' and table_schema not in ('pg_catalog','information_schema') order by 1,2) s",
        )
    )
    result = {}
    for table in tables:
        schema, name = table["table_schema"], table["table_name"]
        if not all(
            re.fullmatch(r"[a-z_][a-z_0-9]*", value) for value in (schema, name)
        ):
            raise ValueError("unexpected table identifier")
        raw = sql(
            container,
            user,
            database,
            f"select coalesce(jsonb_agg(v order by v::text),'[]') from (select to_jsonb(t) v from {schema}.{name} t) rows",
        )
        rows = json.loads(raw)
        # Source and restore can use different database collations. Hash the
        # same row multiset using Python's explicit byte ordering.
        canonical = sorted(
            json.dumps(
                row, sort_keys=True, separators=(",", ":"), ensure_ascii=False
            ).encode()
            for row in rows
        )
        result[f"{schema}.{name}"] = {
            "rows": len(rows),
            "sha256": hashlib.sha256(b"\n".join(canonical)).hexdigest(),
        }
    return result


def exercise(
    review: Path, output: Path, review_container: str, control: str, business: str
) -> dict[str, Any]:
    """Gate owned sources, dump both databases and compare isolated restores."""
    repo = Path(__file__).resolve().parents[2]
    if output.is_relative_to(repo) or output.exists():
        raise ValueError(
            "choose a fresh absolute output directory outside the repository"
        )
    paused = json.loads((review / "paused.ready").read_bytes())
    manifest = json.loads((review / "environment.json").read_bytes())
    if (
        paused != {"http_stopped": True, "workers_stopped": True}
        or manifest["cloud_calls"] != 0
        or manifest["source_kind"] != "synthetic_mechanism_review"
    ):
        raise ValueError("explicit free-review pause required")
    processes = command(["docker", "top", review_container]).decode()
    if "agent-worker" in processes or "/usr/local/bin/python" in processes:
        raise ValueError("review execution processes still present")
    databases = [
        (
            control,
            "jobforge",
            manifest["control_database"],
            "control",
            r"jobforge_run_test_[0-9a-f]{32}",
        ),
        (
            business,
            "jobforge_business_bootstrap",
            manifest["business_database"],
            "business",
            r"jobforge_s4_[0-9a-f]{32}",
        ),
    ]
    output.mkdir(parents=True)
    gates = []
    report: dict[str, Any] = {
        "schema_version": 1,
        "source_kind": manifest["source_kind"],
        "cloud_calls": 0,
        "application_writers_started": 0,
        "restore_image": IMAGE,
        "exercise_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "review_image_id": command(
            ["docker", "inspect", "--format", "{{.Image}}", review_container]
        )
        .decode()
        .strip(),
        "source_head": command(["git", "-C", str(repo), "rev-parse", "HEAD"])
        .decode()
        .strip(),
        "source_dirty": bool(
            command(["git", "-C", str(repo), "status", "--porcelain"])
        ),
        "restore_execution": {
            "enabled_profiles": [],
            "workers": [],
            "availability_source": "deployment_not_database",
        },
        "databases": {},
    }
    try:
        for container, user, database, _kind, pattern in databases:
            if not re.fullmatch(pattern, database):
                raise ValueError("database is outside the explicitly owned review")
            hba = sql(container, user, "postgres", "show hba_file").decode()
            if hba != "/var/lib/postgresql/data/pg_hba.conf":
                raise ValueError("unexpected HBA location")
            original = command(["docker", "exec", container, "cat", hba])
            gates.append((container, user, hba, original))
            barrier = f"host {database} all 0.0.0.0/0 reject\nhost {database} all ::/0 reject\n".encode()
            command(
                [
                    "docker",
                    "exec",
                    "-i",
                    container,
                    "sh",
                    "-c",
                    'cat > "$PGDATA/pg_hba.conf"',
                ],
                barrier + original,
            )
            sql(container, user, "postgres", "select pg_reload_conf()")
        # Both TCP gates are installed before either snapshot. Only local
        # administrator sockets can enter these exact owned databases.
        for container, user, database, kind, _ in databases:
            sql(
                container,
                user,
                "postgres",
                f"select pg_terminate_backend(pid) from pg_stat_activity where datname='{database}'",
            )
            if (
                sql(
                    container,
                    user,
                    "postgres",
                    f"select count(*) from pg_stat_activity where datname='{database}'",
                )
                != b"0"
            ):
                raise ValueError("database sessions remain at barrier")
            before = snapshot(container, user, database)
            dump = output / f"{kind}.dump"
            with dump.open("xb") as stream:
                result = subprocess.run(
                    [
                        "docker",
                        "exec",
                        container,
                        "pg_dump",
                        "-U",
                        user,
                        "-d",
                        database,
                        "-Fc",
                    ],
                    stdout=stream,
                    stderr=subprocess.PIPE,
                    timeout=120,
                )
            if result.returncode:
                raise RuntimeError("protected dump failed")
            if before != snapshot(container, user, database):
                raise ValueError("source changed during stopped backup")
            restored = f"jobforge-s5-restore-{kind}-{uuid.uuid4().hex[:8]}"
            command(
                [
                    "docker",
                    "run",
                    "-d",
                    "--name",
                    restored,
                    "--network",
                    "none",
                    "--label",
                    "jobforge.s5.owned=true",
                    "-e",
                    f"POSTGRES_USER={user}",
                    "-e",
                    "POSTGRES_HOST_AUTH_METHOD=trust",
                    "-e",
                    f"POSTGRES_DB={database}",
                    IMAGE,
                ]
            )
            for _ in range(30):
                ready = subprocess.run(
                    [
                        "docker",
                        "exec",
                        restored,
                        "pg_isready",
                        "-U",
                        user,
                        "-d",
                        database,
                    ],
                    capture_output=True,
                    timeout=5,
                )
                if ready.returncode == 0:
                    break
                time.sleep(1)
            else:
                raise RuntimeError("fresh restore database unavailable")
            roles = json.loads(
                sql(
                    container,
                    user,
                    "postgres",
                    "select json_agg(rolname) from pg_roles where rolname like 'jobforge%'",
                )
            )
            for role in roles:
                if not re.fullmatch(r"jobforge[a-z_0-9]*", role):
                    raise ValueError("unexpected role")
                if role != user:
                    sql(
                        restored,
                        user,
                        "postgres",
                        f"create role {role} nologin nosuperuser nocreatedb nocreaterole nobypassrls",
                    )
            members = json.loads(
                sql(
                    container,
                    user,
                    "postgres",
                    "select coalesce(json_agg(json_build_array(p.rolname,c.rolname)),'[]') from pg_auth_members m join pg_roles p on p.oid=m.roleid join pg_roles c on c.oid=m.member where p.rolname like 'jobforge%' and c.rolname like 'jobforge%'",
                )
            )
            for parent, child in members:
                if parent not in roles or child not in roles:
                    raise ValueError("unexpected membership")
                sql(restored, user, "postgres", f"grant {parent} to {child}")
            command(
                [
                    "docker",
                    "exec",
                    "-i",
                    restored,
                    "pg_restore",
                    "--exit-on-error",
                    "-U",
                    user,
                    "-d",
                    database,
                ],
                dump.read_bytes(),
            )
            after = snapshot(restored, user, database)
            if before != after:
                raise ValueError(
                    "restored table identities/content/budgets do not match"
                )
            report["databases"][kind] = {
                "database": database,
                "source_container": container,
                "restored_container": restored,
                "dump_sha256": hashlib.sha256(dump.read_bytes()).hexdigest(),
                "dump_bytes": dump.stat().st_size,
                "tables": after,
                "all_tables_equal": True,
                "non_admin_roles_nologin": True,
                "network": "none",
            }
    finally:
        gate_failures = []
        for container, user, _hba, original in reversed(gates):
            try:
                command(
                    [
                        "docker",
                        "exec",
                        "-i",
                        container,
                        "sh",
                        "-c",
                        'cat > "$PGDATA/pg_hba.conf"',
                    ],
                    original,
                )
                sql(container, user, "postgres", "select pg_reload_conf()")
            except (RuntimeError, subprocess.SubprocessError):
                gate_failures.append(container)
        if gate_failures:
            report["passed"] = False
            report["hba_restore_failures"] = gate_failures
            (output / "manifest.json").write_text(
                json.dumps(report, ensure_ascii=False, indent=2) + "\n",
                encoding="utf-8",
            )
            raise RuntimeError("HBA restore failed on " + ", ".join(gate_failures))
    # The source access barriers must both be restored before a successful
    # receipt exists. A finally failure cannot leave a misleading passed file.
    report["passed"] = True
    report["hba_restored"] = True
    (output / "manifest.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    return report


def main() -> None:
    """Parse explicit exercise locations and print only a safe summary."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--review-dir", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--review-container", required=True)
    parser.add_argument("--control-container", required=True)
    parser.add_argument("--business-container", required=True)
    args = parser.parse_args()
    if not args.review_dir.is_absolute() or not args.output_dir.is_absolute():
        parser.error("absolute directories required")
    result = exercise(
        args.review_dir.resolve(),
        args.output_dir.resolve(),
        args.review_container,
        args.control_container,
        args.business_container,
    )
    print(
        json.dumps(
            {
                "passed": result["passed"],
                "cloud_calls": 0,
                "databases": len(result["databases"]),
            }
        )
    )


if __name__ == "__main__":
    main()
