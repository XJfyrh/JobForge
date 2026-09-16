"""Production registry: business strategies require a separately accepted adapter."""

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from jobforge_agent.runtime_adapters import RegisteredAdapter

# Test builds replace this entire fixed module; production has no test selector.
REGISTRY: dict[str, "RegisteredAdapter"] = {}
