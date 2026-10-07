#!/usr/bin/env python3
"""Validate the complete asset set produced by the full Release workflow."""

from __future__ import annotations

import argparse
from pathlib import Path
import sys


def expected_assets(version: str) -> set[str]:
    archives = {
        f"sub2api_{version}_linux_amd64.tar.gz",
        f"sub2api_{version}_linux_arm64.tar.gz",
        f"sub2api_{version}_windows_amd64.zip",
        f"sub2api_{version}_darwin_amd64.tar.gz",
        f"sub2api_{version}_darwin_arm64.tar.gz",
    }
    return archives | {
        "checksums.txt",
        f"sub2api-reauth_{version}_linux_amd64.tar.gz",
        f"sub2api-reauth_{version}_linux_arm64.tar.gz",
        f"prism-browser_{version}.tar.gz",
        f"prism-browser_{version}.tar.gz.sha256",
    }


def verify_assets(version: str, actual: list[str] | set[str]) -> None:
    required = expected_assets(version)
    received = set(actual)
    missing = sorted(required - received)
    unexpected = sorted(received - required)
    if missing or unexpected or len(received) != len(list(actual)):
        details = []
        if missing:
            details.append("missing: " + ", ".join(missing))
        if unexpected:
            details.append("unexpected: " + ", ".join(unexpected))
        if len(received) != len(list(actual)):
            details.append("duplicate asset names")
        raise ValueError("release assets do not match the full workflow (" + "; ".join(details) + ")")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("version")
    parser.add_argument("--asset-list", type=Path, help="file containing one published asset name per line")
    args = parser.parse_args()
    lines = (args.asset_list.read_text() if args.asset_list else sys.stdin.read()).splitlines()
    verify_assets(args.version, [line for line in lines if line])
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ValueError as error:
        print(error, file=sys.stderr)
        raise SystemExit(1)
