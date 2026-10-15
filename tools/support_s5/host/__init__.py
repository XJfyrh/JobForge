"""Host-only configuration and health checks for new S5 batches."""

import sys
from pathlib import Path

# Use checked-out Agent/SDK sources, rather than a past installed candidate.
_REPOSITORY = Path(__file__).resolve().parents[3]
for _relative in ("python", "sdk/python"):
    sys.path.insert(0, str(_REPOSITORY / _relative))
