"""Deterministic resource guardrails; synthetic vectors are not acceptance."""

import asyncio
import hashlib
import json
import time
from pathlib import Path
from typing import Any

import httpx
import pytest
from jobforge_agent import preparation, retrieval
from jobforge_agent.__main__ import parser
from jobforge_agent.business import BusinessTools
from jobforge_agent.embedding import MODEL, MODEL_DIGEST, OllamaEmbedding
from jobforge_agent.errors import ToolError
from jobforge_agent.http import BoundedHTTP


def write_corpus(path: Path, body: str = "Synthetic test policy paragraph.") -> None:
    """Create an explicitly synthetic registered corpus for boundary tests."""
    path.mkdir()
    digest = hashlib.sha256()
    for filename in preparation.REGISTERED_FILES:
        stem = Path(filename).stem
        raw = (
            f"# {stem}: Synthetic test\n\n"
            f"<!-- paragraph_id: {stem}.1 -->\n{body}\n\n"
            f"<!-- paragraph_id: {stem}.2 -->\nSecond synthetic test paragraph.\n"
        ).encode("utf-8")
        (path / filename).write_bytes(raw)
        digest.update(filename.encode() + b"\n" + raw + b"\n")
    (path / "manifest.json").write_text(
        json.dumps(
            {
                "schema_version": 1,
                "policy_version": "synthetic-policy-v1",
                "chunking_version": "paragraph-v1",
                "corpus_sha256": digest.hexdigest(),
                "files": preparation.REGISTERED_FILES,
            }
        ),
        encoding="utf-8",
    )


def test_registered_chunking_preserves_ids_and_checks_hash(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Only paragraph bodies enter embedding, with stable references and hash."""
    corpus = tmp_path / "policies"
    write_corpus(corpus)
    monkeypatch.setattr(preparation, "POLICIES_PATH", corpus)
    profile, chunks = preparation.load_registered_corpus()
    assert len(chunks) == 20
    assert chunks[0] == {
        "chunk_id": "P01.1",
        "source": "P01.md",
        "text": "Synthetic test policy paragraph.",
    }
    assert profile["dimensions"] == 384
    with (corpus / "P01.md").open("ab") as output:
        output.write(b"mutation")
    with pytest.raises(ToolError, match="CORPUS_HASH_MISMATCH"):
        preparation.load_registered_corpus()


@pytest.mark.parametrize(
    "mutation",
    ["filename", "extra_field", "boolean_schema", "chunk_bytes", "total_bytes"],
)
def test_corpus_rejects_unregistered_or_oversized_input(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, mutation: str
) -> None:
    """Reject path overrides, ambiguous metadata and UTF-8 resource overflows."""
    corpus = tmp_path / "policies"
    write_corpus(corpus, "中" * 257 if mutation == "chunk_bytes" else "test")
    manifest_path = corpus / "manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if mutation == "filename":
        manifest["files"][0] = "../gold.json"
    elif mutation == "extra_field":
        manifest["cloud_endpoint"] = "https://invalid.example"
    elif mutation == "boolean_schema":
        manifest["schema_version"] = True
    elif mutation == "total_bytes":
        (corpus / "P01.md").write_bytes(b"x" * (64 * 1024 + 1))
    manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
    monkeypatch.setattr(preparation, "POLICIES_PATH", corpus)
    with pytest.raises(ToolError, match="INVALID_CORPUS|INPUT_TOO_LARGE"):
        preparation.load_registered_corpus()


@pytest.mark.parametrize("failure", [None, "second_batch", "cancel"])
def test_prepare_report_precedes_calls_and_never_claims_publication(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, failure: str | None
) -> None:
    """Persist unknown before synthetic sends, retaining failures and interruptions."""
    corpus, output = tmp_path / "policies", tmp_path / "output"
    write_corpus(corpus)
    monkeypatch.setattr(preparation, "POLICIES_PATH", corpus)
    requests: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request.url.path)
        reports = list(output.glob("*.prepare.json"))
        assert len(reports) == 1
        report = json.loads(reports[0].read_text(encoding="utf-8"))
        assert report["state"] == "unknown"
        assert report["profile"]["embedding_digest"] == MODEL_DIGEST
        if request.url.path == "/api/version":
            return httpx.Response(200, json={"version": "0.32.5"})
        if request.url.path == "/api/tags":
            return httpx.Response(
                200, json={"models": [{"name": MODEL, "digest": MODEL_DIGEST}]}
            )
        assert report["embedding_requests"] == requests.count("/api/embed")
        if failure == "cancel":
            raise asyncio.CancelledError()
        if failure == "second_batch" and report["embedding_requests"] == 2:
            return httpx.Response(
                503, json={"error": {"code": "DEPENDENCY_UNAVAILABLE"}}
            )
        payload = json.loads(request.content)
        assert len(payload["input"]) <= 16
        return httpx.Response(
            200,
            json={
                "model": MODEL,
                "embeddings": [[1.0] + [0.0] * 383] * len(payload["input"]),
            },
        )

    def embedding(origin: str, **kwargs: Any) -> OllamaEmbedding:
        return OllamaEmbedding(origin, transport=httpx.MockTransport(handler), **kwargs)

    monkeypatch.setattr(preparation, "OllamaEmbedding", embedding)
    if failure is None:
        report_path = asyncio.run(preparation.prepare(output, "http://127.0.0.1:11434"))
        report = json.loads(report_path.read_text(encoding="utf-8"))
        assert report["state"] == "prepared"
        assert report["embedding_requests"] == 2
        assert report["identity_requests"] == 4
        upload_raw = (output / report["upload_filename"]).read_bytes()
        assert hashlib.sha256(upload_raw).hexdigest() == report["upload_sha256"]
        upload = json.loads(upload_raw)
        assert set(upload) == {"schema_version", "prepare_id", "profile", "chunks"}
        assert len(upload["chunks"]) == 20
        assert all(len(chunk["embedding"]) == 384 for chunk in upload["chunks"])
    else:
        expected = asyncio.CancelledError if failure == "cancel" else ToolError
        with pytest.raises(expected):
            asyncio.run(preparation.prepare(output, "http://127.0.0.1:11434"))
        report = json.loads(
            next(output.glob("*.prepare.json")).read_text(encoding="utf-8")
        )
        assert report["state"] == "unknown"
        assert report["embedding_requests"] == (1 if failure == "cancel" else 2)
        assert not list(output.glob("*.vectors.json"))
    assert "published" not in report


def test_query_set_has_exact_twenty_rows_and_no_gold(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Queries are fixed registration data, never a dynamic task queue."""
    path = tmp_path / "queries.jsonl"
    rows = [
        {
            "query_id": f"RQ-{number:02d}",
            "query": "test",
            "policy_revision": "synthetic-policy-v1",
        }
        for number in range(1, 21)
    ]
    path.write_text("\n".join(json.dumps(row) for row in rows), encoding="utf-8")
    monkeypatch.setattr(retrieval, "QUERIES_PATH", path)
    assert len(retrieval.load_registered_queries("synthetic-policy-v1")[0]) == 20
    rows[0]["expected_answer"] = "not permitted in runtime"
    path.write_text("\n".join(json.dumps(row) for row in rows), encoding="utf-8")
    with pytest.raises(ToolError, match="INVALID_QUERY_SET"):
        retrieval.load_registered_queries("synthetic-policy-v1")


@pytest.mark.parametrize(
    "flag", ["--chat-endpoint", "--api-key", "--corpus", "--query", "--budget"]
)
def test_resource_cli_cannot_accept_cloud_or_dynamic_tasks(flag: str) -> None:
    """The fixed free initializer offers no generic cloud or task dispatch input."""
    with pytest.raises(SystemExit):
        parser().parse_args(["prepare", "--output-dir", "output", flag, "value"])


def test_expired_embedding_deadline_does_not_dispatch() -> None:
    """A caller deadline is enforced before version or model HTTP calls."""

    def handler(_request: httpx.Request) -> httpx.Response:
        raise AssertionError("expired call cannot dispatch")

    async def run() -> None:
        embedding = OllamaEmbedding(transport=httpx.MockTransport(handler))
        try:
            with pytest.raises(ToolError, match="DEADLINE_EXCEEDED"):
                await embedding.embed(["test"], deadline=time.monotonic() - 1)
        finally:
            await embedding.aclose()

    asyncio.run(run())


def test_retrieval_preserves_all_rows_and_failed_calls(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Synthetic HTTP verifies the fixed denominator and explicit failed row."""
    corpus = tmp_path / "policies"
    write_corpus(corpus)
    monkeypatch.setattr(preparation, "POLICIES_PATH", corpus)
    profile, _ = preparation.load_registered_corpus()
    queries = tmp_path / "queries.jsonl"
    queries.write_text(
        "\n".join(
            json.dumps(
                {
                    "query_id": f"RQ-{i:02d}",
                    "query": "test",
                    "policy_revision": profile["policy_version"],
                }
            )
            for i in range(1, 21)
        ),
        encoding="utf-8",
    )
    monkeypatch.setattr(retrieval, "QUERIES_PATH", queries)
    snapshot_id = "9b0d4f0f-b32a-49e4-9c5e-a03c27364f05"
    index_id = "3669df59-41d1-4dca-bf32-bc7dfac73064"
    searches = 0

    def model(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/version":
            return httpx.Response(200, json={"version": "0.32.5"})
        if request.url.path == "/api/tags":
            return httpx.Response(
                200, json={"models": [{"name": MODEL, "digest": MODEL_DIGEST}]}
            )
        return httpx.Response(
            200, json={"model": MODEL, "embeddings": [[1.0] + [0.0] * 383]}
        )

    def service(request: httpx.Request) -> httpx.Response:
        nonlocal searches
        if request.method == "GET":
            return httpx.Response(
                200,
                json={
                    "snapshot_id": snapshot_id,
                    "schema_version": 1,
                    "tenant_id": "synthetic-tenant",
                    "ticket": {"tenant_id": "synthetic-tenant", "order_id": "order-1"},
                    "index": {
                        "index_id": index_id,
                        "tenant_id": "synthetic-tenant",
                        "profile_hash": "a" * 64,
                        "profile": profile,
                    },
                },
            )
        searches += 1
        if searches == 2:
            return httpx.Response(
                503, json={"error": {"code": "DEPENDENCY_UNAVAILABLE"}}
            )
        return httpx.Response(
            200,
            json={
                "snapshot_id": snapshot_id,
                "matches": [
                    {
                        "index_id": index_id,
                        "chunk_id": "P01.1",
                        "policy_version": profile["policy_version"],
                        "evidence_ref": f"business-policy:{index_id}:P01.1",
                        "source": "P01.md",
                        "text": "synthetic test policy",
                        "distance": 0.0,
                    }
                ],
            },
        )

    def http(origin: str, **kwargs: Any) -> BoundedHTTP:
        return BoundedHTTP(origin, transport=httpx.MockTransport(service), **kwargs)

    def embedding(origin: str, **kwargs: Any) -> OllamaEmbedding:
        return OllamaEmbedding(origin, transport=httpx.MockTransport(model), **kwargs)

    def tools(*args: Any, **kwargs: Any) -> BusinessTools:
        return BusinessTools(*args, transport=httpx.MockTransport(service), **kwargs)

    monkeypatch.setattr(retrieval, "BoundedHTTP", http)
    monkeypatch.setattr(retrieval, "OllamaEmbedding", embedding)
    monkeypatch.setattr(retrieval, "BusinessTools", tools)
    report_path = asyncio.run(
        retrieval.retrieve(
            tmp_path / "output",
            "http://127.0.0.1:11434",
            "http://127.0.0.1:8088",
            "synthetic-key",
            snapshot_id,
        )
    )
    report = json.loads(report_path.read_text(encoding="utf-8"))
    assert report["state"] == "completed"
    assert len(report["rows"]) == 20
    assert report["embedding_requests"] == 20
    assert report["business_requests"] == 21
    assert report["identity_requests"] == 40
    assert report["failed_operations"] == 1
    assert report["rows"][1] == {
        "query_id": "RQ-02",
        "status": "failed",
        "error_code": "DEPENDENCY_UNAVAILABLE",
    }
    assert sum(row["status"] == "returned" for row in report["rows"]) == 19
