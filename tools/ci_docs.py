"""Validate tracked Markdown's local targets, case and heading anchors."""

from __future__ import annotations

import os
import re
import subprocess
from pathlib import Path
from urllib.parse import unquote, urlsplit

from tools.ci_checks import ROOT


def headings(source: str) -> set[str]:
    """Collect GitHub-style heading IDs, duplicate suffixes and explicit anchors."""
    result = set(re.findall(r'\b(?:id|name)=["\']([^"\']+)["\']', source))
    counts: dict[str, int] = {}
    for title in re.findall(r"^#{1,6}\s+(.+?)\s*#*\s*$", source, re.MULTILINE):
        slug = re.sub(r"[^\w\- ]", "", title.lower()).replace(" ", "-")
        count = counts.get(slug, 0)
        counts[slug] = count + 1
        result.add(slug + (f"-{count}" if count else ""))
    return result


def targets(source: str) -> list[str]:
    """Read inline and reference link destinations outside fenced code examples."""
    source = re.sub(
        r"^(`{3,}|~{3,}).*?^\1\s*$", "", source, flags=re.MULTILINE | re.DOTALL
    )
    inline = re.findall(r"!?\[[^\]\n]*\]\((<[^>]+>|[^\s)]+)(?:\s+[^)]*)?\)", source)
    references = re.findall(r"^\s*\[[^\]]+\]:\s*(<[^>]+>|\S+)", source, re.MULTILINE)
    return [value.strip("<>") for value in inline + references]


def check_link(root: Path, source: Path, target: str) -> str | None:
    """Check a local link without making network requests or following external URLs."""
    url = urlsplit(target)
    if url.scheme or url.netloc or target.startswith("/"):
        return None
    # Path.resolve() normalizes the on-disk spelling on Windows, hiding bad case.
    path = (
        Path(os.path.abspath(source.parent / unquote(url.path))) if url.path else source
    )
    try:
        relative = path.relative_to(root)
    except ValueError:
        return None
    # Resolve case explicitly because Windows otherwise accepts Linux-broken paths.
    current = root
    for part in relative.parts:
        if not current.is_dir() or part not in {
            child.name for child in current.iterdir()
        }:
            return f"{source.relative_to(root)}: missing or wrong-case target {target}"
        current /= part
    if (
        url.fragment
        and path.suffix == ".md"
        and unquote(url.fragment) not in headings(path.read_text(encoding="utf-8"))
    ):
        return f"{source.relative_to(root)}: unknown anchor {target}"
    return None


def main() -> None:
    """Fail on broken local references; never rewrite contract or evidence files."""
    result = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "*.md"],
        cwd=ROOT,
        check=True,
        capture_output=True,
    )
    errors = []
    count = 0
    for name in sorted(set(result.stdout.decode("utf-8").split("\0")) - {""}):
        source = ROOT / name
        if not source.is_file():
            continue
        for target in targets(source.read_text(encoding="utf-8")):
            count += 1
            if error := check_link(ROOT, source, target):
                errors.append(error)
    if errors:
        raise SystemExit("\n".join(errors))
    print(f"Checked {count} Markdown references.")


if __name__ == "__main__":
    main()
