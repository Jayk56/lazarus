#!/usr/bin/env python3
"""Parse repository YAML sources or a rendered YAML stream."""

from pathlib import Path
import sys

import yaml


def parse(name: str, text: str) -> None:
    try:
        list(yaml.safe_load_all(text))
    except yaml.YAMLError as exc:
        raise SystemExit(f"{name}: invalid YAML: {exc}") from exc


if sys.argv[1:] == ["-"]:
    parse("standard input", sys.stdin.read())
elif sys.argv[1:]:
    for argument in sys.argv[1:]:
        path = Path(argument)
        parse(str(path), path.read_text(encoding="utf-8"))
else:
    excluded = Path("deploy/helm/lazarus/templates")
    ignored_parts = {".agents", ".claude", ".codex", ".git", ".local", ".uv-cache"}
    paths = sorted(
        path
        for pattern in ("*.yml", "*.yaml")
        for path in Path(".").rglob(pattern)
        if excluded not in path.parents and not ignored_parts.intersection(path.parts)
    )
    for path in paths:
        parse(str(path), path.read_text(encoding="utf-8"))
    print(f"Parsed {len(paths)} YAML source files.")
