"""Historical costs and the shared inventory cannot disappear between batches."""

import pytest

from tools.support_s5.host import ledger


def test_historical_identity_and_known_cost_cannot_be_reset() -> None:
    """An empty clone fails; legitimate full-hold settlement succeeds."""
    old = {
        "database": "historical",
        "container": "actual-paid-pg",
        "user": "owner",
        "known": 13_212_370,
        "held": 2_228_224,
    }
    current = dict(old, known=13_220_000, held=0)
    ledger.validate_history({"phases": [old]}, {"phases": [current]})
    for changed in (dict(current, known=0), dict(current, container="empty-clone")):
        with pytest.raises(ValueError, match="HISTORICAL"):
            ledger.validate_history({"phases": [old]}, {"phases": [changed]})


def test_preprepared_batch_cannot_omit_a_new_paid_database() -> None:
    """B prepared against P is stale once A joins the shared stage inventory."""
    old = {"database": "P", "container": "pg", "user": "owner", "known": 100}
    a = dict(old, database="A", known=200)
    b = dict(old, database="B", known=0)
    with pytest.raises(ValueError, match="HISTORICAL_DATABASE_IDENTITY"):
        ledger.validate_stage({"phases": [old, a]}, {"phases": [old, b]}, "B")
    ledger.validate_stage({"phases": [old, a]}, {"phases": [old, a, b]}, "B")
