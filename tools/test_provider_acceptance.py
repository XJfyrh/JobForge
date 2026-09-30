"""Offline checks for cumulative admission and conservative receipt handling."""

import pytest

from tools.run_provider_acceptance import receipt_exposure, validate_prior_exposure


@pytest.mark.parametrize("prior", [-1, True, 0.1, 47_000_001, 50_000_000])
def test_prior_exposure_cannot_reset_or_exceed_project_authorization(prior) -> None:
    """Both known cost and old holds must fit alongside the new batch bound."""
    with pytest.raises(ValueError):
        validate_prior_exposure(prior)


def test_prior_exposure_accepts_exact_remaining_bound() -> None:
    """No rounding slack may be taken beyond the total authorization."""
    validate_prior_exposure(47_000_000)


def test_unknown_call_retains_full_hold_instead_of_becoming_free() -> None:
    """A failed real call remains exposure even when no usage was observed."""
    receipt = {
        "batch_cap_microyuan": 3_000_000,
        "known_cost_microyuan": 0,
        "held_cost_microyuan": 2_105_344,
        "calls": {
            "items": [{"known_cost_microyuan": 0, "held_cost_microyuan": 2_105_344}]
        },
    }
    assert receipt_exposure(receipt) == (0, 2_105_344)
    receipt["held_cost_microyuan"] = 0
    with pytest.raises(ValueError):
        receipt_exposure(receipt)


@pytest.mark.parametrize(
    "receipt",
    [
        {},
        {"known_cost_microyuan": 0, "held_cost_microyuan": 0},
        {
            "batch_cap_microyuan": 3_000_000,
            "known_cost_microyuan": -1,
            "held_cost_microyuan": 0,
        },
    ],
)
def test_incomplete_or_invalid_evidence_cannot_release_provisional_hold(
    receipt,
) -> None:
    """Missing evidence is a stop condition, never a zero-cost success."""
    with pytest.raises(ValueError):
        receipt_exposure(receipt)
