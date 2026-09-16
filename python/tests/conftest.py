"""Import the unpackaged S1 source tree without installing new dependencies."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
