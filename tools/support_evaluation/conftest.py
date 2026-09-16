"""Use repository SDK and executor sources for offline tests, never installed drift."""

import sys
from pathlib import Path

REPOSITORY = Path(__file__).resolve().parents[2]
for relative in ("python", "sdk/python"):
    sys.path.insert(0, str(REPOSITORY / relative))
