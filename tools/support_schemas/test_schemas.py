"""Check exact generation, schema syntax and Go-produced deployment vectors."""

from __future__ import annotations

import copy
import hashlib
import json

import pytest
from jsonschema import (  # type: ignore[import-untyped]
    Draft202012Validator,
    ValidationError,
)

from tools.support_schemas.generate import ROOT, generated


def test_all_deployment_schema_bytes_and_go_vectors() -> None:
    """Every published schema has reproducible bytes and a Go-validated example."""
    outputs = generated()
    assert set(outputs) == set((ROOT / "api/support").glob("*/profile-schema.json"))
    vectors = json.loads(
        (ROOT / "tests/testdata/support-profile-schema-vectors.json").read_bytes()
    )
    assert {path.parent.name for path in outputs} == set(vectors)
    for path, raw in outputs.items():
        assert raw == path.read_bytes(), path
        schema = json.loads(raw)
        Draft202012Validator.check_schema(schema)
        validator = Draft202012Validator(schema)
        vector = vectors[path.parent.name]
        assert vector["schema_sha256"] == hashlib.sha256(raw).hexdigest()
        sample = vector["definition"]
        validator.validate(sample)
        for field in schema["required"]:
            missing = copy.deepcopy(sample)
            del missing[field]
            with pytest.raises(ValidationError):
                validator.validate(missing)
        assert not validator.is_valid(dict(sample, arbitrary_runtime="forbidden"))
