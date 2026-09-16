"""Fixed free local preparation commands, without chat or Run execution."""

from __future__ import annotations

import argparse
import asyncio
import json
import os
from pathlib import Path

from jobforge_agent.embedding import LOCAL_ORIGINS
from jobforge_agent.errors import ToolError
from jobforge_agent.preparation import prepare
from jobforge_agent.retrieval import retrieve


def parser() -> argparse.ArgumentParser:
    """Expose only fixed corpus preparation and registered query verification."""
    result = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    commands = result.add_subparsers(dest="command", required=True)
    for name in ("prepare", "retrieval"):
        command = commands.add_parser(name, allow_abbrev=False)
        command.add_argument("--output-dir", required=True, type=Path)
        command.add_argument(
            "--ollama-origin", choices=LOCAL_ORIGINS, default=LOCAL_ORIGINS[0]
        )
        if name == "retrieval":
            command.add_argument("--business-origin", required=True)
            command.add_argument("--snapshot-id", required=True)
    return result


def main() -> int:
    """Print only artifact references or sanitized errors, never raw exceptions."""
    arguments = parser().parse_args()
    try:
        if arguments.command == "prepare":
            path = asyncio.run(prepare(arguments.output_dir, arguments.ollama_origin))
        else:
            key = os.environ.get("JOBFORGE_BUSINESS_READ_KEY", "")
            if not key:
                raise ToolError("MISSING_BUSINESS_CREDENTIAL")
            path = asyncio.run(
                retrieve(
                    arguments.output_dir,
                    arguments.ollama_origin,
                    arguments.business_origin,
                    key,
                    arguments.snapshot_id,
                )
            )
        print(json.dumps({"report": str(path.resolve())}))
        return 0
    except (ToolError, OSError) as exc:
        code = exc.code if isinstance(exc, ToolError) else "LOCAL_IO_FAILURE"
        print(json.dumps({"error": {"code": code}}))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
