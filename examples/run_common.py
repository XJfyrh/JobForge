"""Shared configuration for the explicit, synchronous Run examples."""

from __future__ import annotations

import json
import os
from dataclasses import asdict, is_dataclass
from datetime import datetime
from enum import Enum
from typing import Any

from jobforge import RunClient


def client() -> RunClient:
    """Use one public identity; do not discover or grant credentials."""
    return RunClient(
        os.environ["JOBFORGE_API_URL"], os.environ["JOBFORGE_API_KEY"], timeout=10
    )


def emit(value: Any) -> None:
    """Print the caller's requested view, without ever printing credentials."""

    def encode(item: Any) -> Any:
        if is_dataclass(item) and not isinstance(item, type):
            return asdict(item)
        if isinstance(item, datetime):
            return item.isoformat()
        if isinstance(item, Enum):
            return item.value
        raise TypeError("unsupported example output")

    print(json.dumps(value, default=encode, ensure_ascii=False, indent=2))
