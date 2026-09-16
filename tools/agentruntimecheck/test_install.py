"""Install an explicit synthetic adapter/origin only in the integration target.

Production never copies or executes this build helper. There is no runtime URL
switch: the test image has a fixed loopback endpoint and a different registry.
"""

from __future__ import annotations

import importlib.util
import shutil
from pathlib import Path


def main() -> None:
    """Fail closed when the installed production source no longer matches."""
    spec = importlib.util.find_spec("jobforge_agent")
    if spec is None or not spec.submodule_search_locations:
        raise RuntimeError("installed executor package missing")
    paths = list(spec.submodule_search_locations)
    if len(paths) != 1:
        raise RuntimeError("ambiguous executor package")
    package = Path(paths[0])
    original = '"https://api.deepseek.com"'
    replacement = '"http://127.0.0.1:18093"'
    for name in ("dispatch.py", "deepseek.py", "step.py"):
        path = package / name
        source = path.read_text(encoding="utf-8")
        if source.count(original) != 1:
            raise RuntimeError("fixed provider source count changed")
        path.write_text(source.replace(original, replacement), encoding="utf-8")
    registry = package / "runtime_registry.py"
    if registry.read_text(encoding="utf-8").count("REGISTRY") != 1:
        raise RuntimeError("production registry source changed")
    shutil.copyfile("/tmp/runtime_fixture_registry.py", registry)
    destination = Path("/etc/jobforge/executor.json")
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile("/tmp/test-manifest.json", destination)


if __name__ == "__main__":
    main()
