"""Prepare a fixed registered policy corpus without Run or database access."""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import re
import time
from pathlib import Path
from typing import Any
from uuid import uuid4

from jobforge_agent.embedding import DIMENSIONS, MODEL, MODEL_DIGEST, OllamaEmbedding
from jobforge_agent.errors import ToolError
from jobforge_agent.http import strict_json

REPO_ROOT = Path(__file__).resolve().parents[2]
POLICIES_PATH = REPO_ROOT / "examples" / "support-agent" / "runtime" / "policies"
REGISTERED_FILES = [f"P{number:02d}.md" for number in range(1, 11)]
MAX_CORPUS_BYTES = 64 * 1024
MAX_UPLOAD_BYTES = 1024 * 1024


def canonical_json(value: Any) -> bytes:
    """Serialize bounded artifacts deterministically, forbidding NaN and infinity."""
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def bounded_file(path: Path, limit: int) -> bytes:
    """Read at most limit plus one byte, never first loading an unbounded file."""
    with path.open("rb") as source:
        raw = source.read(limit + 1)
    if len(raw) > limit:
        raise ToolError("INPUT_TOO_LARGE")
    return raw


def load_registered_corpus() -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """Read only the registered manifest and P01 through P10 runtime policies."""
    manifest = strict_json(bounded_file(POLICIES_PATH / "manifest.json", 4096))
    if (
        not isinstance(manifest, dict)
        or set(manifest)
        != {
            "schema_version",
            "policy_version",
            "chunking_version",
            "corpus_sha256",
            "files",
        }
        or type(manifest["schema_version"]) is not int
        or manifest["schema_version"] != 1
        or manifest["files"] != REGISTERED_FILES
        or manifest["chunking_version"] != "paragraph-v1"
        or not isinstance(manifest["policy_version"], str)
        or not re.fullmatch(
            "[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", manifest["policy_version"]
        )
    ):
        raise ToolError("INVALID_CORPUS")
    digest = hashlib.sha256()
    total = 0
    chunks: list[dict[str, Any]] = []
    for filename in REGISTERED_FILES:
        path = POLICIES_PATH / filename
        if path.resolve().parent != POLICIES_PATH.resolve():
            raise ToolError("INVALID_CORPUS")
        raw = bounded_file(path, MAX_CORPUS_BYTES - total)
        total += len(raw)
        digest.update(filename.encode("utf-8") + b"\n" + raw + b"\n")
        try:
            text = raw.decode("utf-8")
        except UnicodeError as exc:
            raise ToolError("INVALID_CORPUS") from exc
        if "\r" in text or "\x00" in text:
            raise ToolError("INVALID_CORPUS")
        parts = re.split(r"(?m)^<!-- paragraph_id: (P\d{2}\.\d+) -->\n", text)
        if len(parts) != 5 or not re.fullmatch(rf"# {path.stem}: [^\n]+\n+", parts[0]):
            raise ToolError("INVALID_CORPUS")
        for offset in (1, 3):
            chunk_id, body = parts[offset], parts[offset + 1].strip()
            if (
                chunk_id != f"{path.stem}.{(offset + 1) // 2}"
                or not body
                or len(body.encode("utf-8")) > 768
                or "\n\n" in body
                or "<!--" in body
            ):
                raise ToolError("INVALID_CORPUS")
            chunks.append({"chunk_id": chunk_id, "source": filename, "text": body})
    if not 1 <= len(chunks) <= 64 or digest.hexdigest() != manifest["corpus_sha256"]:
        raise ToolError("CORPUS_HASH_MISMATCH")
    return {
        "policy_version": manifest["policy_version"],
        "corpus_sha256": digest.hexdigest(),
        "chunker_version": "paragraph-v1",
        "embedding_model": MODEL,
        "embedding_digest": MODEL_DIGEST,
        "dimensions": DIMENSIONS,
    }, chunks


def atomic_json(path: Path, value: Any, limit: int = MAX_UPLOAD_BYTES) -> str:
    """Replace one completed artifact atomically after flushing its bounded bytes."""
    raw = canonical_json(value)
    if len(raw) > limit:
        raise ToolError("OUTPUT_TOO_LARGE")
    temporary = path.with_name(path.name + ".tmp-" + str(uuid4()))
    try:
        with temporary.open("xb") as output:
            output.write(raw)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)
    return hashlib.sha256(raw).hexdigest()


class PreparationReport:
    """Persist dispatch-intent counters; crashes retain an unknown outcome.

    Counters are conservative intents recorded before dispatch, not proof of a
    physical send: persistence, deadlines or cancellation can prevent the send.
    """

    def __init__(self, output_dir: Path, profile: dict[str, Any], kind: str) -> None:
        """Create the initial unknown report before any network request."""
        output_dir.mkdir(parents=True, exist_ok=True)
        self.identity = str(uuid4())
        self.path = output_dir / f"{self.identity}.{kind}.json"
        self.value: dict[str, Any] = {
            "schema_version": 1,
            "prepare_id": self.identity,
            "kind": kind,
            "state": "unknown",
            "profile": profile,
            "embedding_requests": 0,
            "identity_requests": 0,
            "business_requests": 0,
            "request_count_semantics": "dispatch_intent_before_send",
            "failed_operations": 0,
            "ollama_version": "0.32.5",
        }
        self.save()

    def save(self) -> None:
        """Persist a bounded report without recording credentials or HTTP bodies."""
        atomic_json(self.path, self.value)

    def before_send(self, path: str) -> None:
        """Record one send intent without claiming dispatch or remote completion."""
        key = (
            "embedding_requests"
            if path == "/api/embed"
            else "identity_requests"
            if path.startswith("/api/")
            else "business_requests"
        )
        self.value[key] += 1
        self.save()


async def prepare(output_dir: Path, local_origin: str) -> Path:
    """Generate at most four free embedding batches and an offline loader file."""
    profile, chunks = load_registered_corpus()
    report = PreparationReport(output_dir, profile, "prepare")
    embedding = OllamaEmbedding(local_origin, before_send=report.before_send)
    started = time.monotonic()
    try:
        async with asyncio.timeout(300):
            for offset in range(0, len(chunks), 16):
                batch = chunks[offset : offset + 16]
                vectors = await embedding.embed(
                    [item["text"] for item in batch], deadline=started + 300
                )
                for chunk, values in zip(batch, vectors, strict=True):
                    chunk["embedding"] = values
            upload = {
                "schema_version": 1,
                "prepare_id": report.identity,
                "profile": profile,
                "chunks": chunks,
            }
            destination = output_dir / f"{report.identity}.vectors.json"
            digest = atomic_json(destination, upload)
            report.value.update(
                {
                    "state": "prepared",
                    "chunk_count": len(chunks),
                    "upload_filename": destination.name,
                    "upload_sha256": digest,
                }
            )
            report.save()
    except (ToolError, TimeoutError, OSError) as exc:
        report.value["error_code"] = (
            exc.code if isinstance(exc, ToolError) else "PREPARATION_FAILED"
        )
        report.value["failed_operations"] += 1
        report.save()
        raise ToolError(report.value["error_code"]) from exc
    finally:
        await embedding.aclose()
    return report.path
