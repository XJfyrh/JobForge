"""Production registry of fixed, reviewed business adapters; no dynamic selector."""

from typing import TYPE_CHECKING

from jobforge_agent.support_adapter import SupportFixedAdapter
from jobforge_agent.support_agent import SupportAgentAdapter

if TYPE_CHECKING:
    from jobforge_agent.runtime_adapters import RegisteredAdapter

# Test builds replace this entire fixed module; production has no test selector.
REGISTRY: dict[str, "RegisteredAdapter"] = {
    "support-fixed-v1": SupportFixedAdapter(),
    "support-agent-v1": SupportAgentAdapter(),
}
