#!/usr/bin/env python3
"""
Determine the next n4k.nirmata release tag.

Tag format: v{major}.{minor}.{patch}-n4k.nirmata.{n4k_patch}[-{rc|dirty}{N}]

Usage:
  determine <release_type> [base_version]
  RELEASE_TYPE=ga BASE_VERSION=v1.16.3-n4k.nirmata.0 determine

  release_type : ga | rc | dirty
  base_version : optional fallback used only when no previous tag is found
                 e.g. v1.16.3-n4k.nirmata.0

Outputs:
  Prints the new tag to stdout.
  If $GITHUB_OUTPUT is set, appends "tag=<new_tag>" to that file.
"""

import os
import re
import subprocess
import sys
from collections import defaultdict

TAG_RE = re.compile(
    r"^v(\d+)\.(\d+)\.(\d+)-n4k\.nirmata\.(\d+)(?:-(rc|dirty)(\d+))?$"
)

# Lower value = more stable
STABILITY = {"ga": 0, "rc": 1, "dirty": 2}


def parse_tag(tag: str) -> dict | None:
    """Parse an n4k.nirmata tag string into a dict, or return None."""
    m = TAG_RE.match(tag.strip())
    if not m:
        return None
    major, minor, patch, n4k_patch, suffix_type, suffix_num = m.groups()
    t = suffix_type if suffix_type else "ga"
    return {
        "major": int(major),
        "minor": int(minor),
        "patch": int(patch),
        "n4k_patch": int(n4k_patch),
        "suffix_type": suffix_type,       # None | "rc" | "dirty"
        "suffix_num": int(suffix_num) if suffix_num else 0,
        "type": t,
        "raw": tag.strip(),
    }


def _run(cmd: list[str]) -> str:
    result = subprocess.run(cmd, capture_output=True, text=True)
    return result.stdout.strip()


def find_prev_tag() -> dict | None:
    """
    Find the most relevant previous tag reachable from HEAD.

    Strategy:
    1. Collect all tags reachable from HEAD, sorted newest-created first.
    2. Filter to n4k.nirmata pattern.
    3. For tags sharing the same commit, keep only the most stable one
       (GA > RC > Dirty); ties within the same type are broken by creatordate
       (already in order — first = most recent).
    4. Among surviving per-commit winners, pick the highest version:
       sort by (major, minor, patch, n4k_patch) descending, then stability
       ascending (more stable preferred on equal version).
    """
    raw = _run(["git", "tag", "--merged", "HEAD", "--sort=-creatordate"])
    if not raw:
        return None

    tags = [t for t in raw.splitlines() if t.strip()]

    # Parse and filter
    parsed: list[dict] = []
    for t in tags:
        p = parse_tag(t)
        if p:
            parsed.append(p)

    if not parsed:
        return None

    # Resolve each tag to its commit SHA
    sha_map: dict[str, str] = {}
    for p in parsed:
        sha = _run(["git", "rev-list", "-n", "1", p["raw"]])
        sha_map[p["raw"]] = sha

    # Group by commit SHA; per-commit keep the most stable (lowest STABILITY value),
    # breaking ties by position in `parsed` list (earlier = more recently created)
    commit_best: dict[str, dict] = {}
    for p in parsed:
        sha = sha_map[p["raw"]]
        if sha not in commit_best:
            commit_best[sha] = p
        else:
            existing = commit_best[sha]
            if STABILITY[p["type"]] < STABILITY[existing["type"]]:
                commit_best[sha] = p
            # same type → keep the one already stored (more recently created)

    candidates = list(commit_best.values())

    # Pick the highest version; on version tie prefer most stable
    candidates.sort(
        key=lambda t: (
            t["major"],
            t["minor"],
            t["patch"],
            t["n4k_patch"],
            -STABILITY[t["type"]],   # negate so that higher stability (lower value) wins
        ),
        reverse=True,
    )
    return candidates[0]


def build_base(t: dict) -> str:
    return f"v{t['major']}.{t['minor']}.{t['patch']}-n4k.nirmata.{t['n4k_patch']}"


def compute_next(prev: dict, curr_type: str) -> str:
    """
    Compute the next tag given the previous tag info and the desired release type.

    Transition table
    ----------------
    prev  → curr  : action
    GA    → GA    : bump n4k_patch
    GA    → RC    : bump n4k_patch, add -rc1
    GA    → Dirty : bump n4k_patch, add -dirty1
    RC    → GA    : strip suffix (keep base)
    RC    → RC    : increment rc counter
    Dirty → GA    : strip suffix (keep base)
    Dirty → Dirty : increment dirty counter
    RC    → Dirty : unsupported (cross-branch constraint)
    Dirty → RC    : unsupported (cross-branch constraint)
    """
    prev_type = prev["type"]

    if curr_type not in STABILITY:
        raise ValueError(f"Unknown release type: {curr_type!r}. Must be ga, rc, or dirty.")

    # Unsupported cross-branch transitions
    if (prev_type == "rc" and curr_type == "dirty") or (
        prev_type == "dirty" and curr_type == "rc"
    ):
        raise ValueError(
            f"Unsupported transition: {prev_type} → {curr_type}. "
            "RC tags live on main/release-* branches; dirty tags live on other branches. "
            "These release types cannot be mixed in a single lineage."
        )

    base = build_base(prev)

    if curr_type == "ga":
        if prev_type == "ga":
            return f"v{prev['major']}.{prev['minor']}.{prev['patch']}-n4k.nirmata.{prev['n4k_patch'] + 1}"
        # rc → ga or dirty → ga: strip suffix, keep base
        return base

    if curr_type == "rc":
        if prev_type == "rc":
            return f"{base}-rc{prev['suffix_num'] + 1}"
        # ga → rc: bump n4k_patch, start at rc1
        new_n4k = prev["n4k_patch"] + 1
        return f"v{prev['major']}.{prev['minor']}.{prev['patch']}-n4k.nirmata.{new_n4k}-rc1"

    if curr_type == "dirty":
        if prev_type == "dirty":
            return f"{base}-dirty{prev['suffix_num'] + 1}"
        # ga → dirty: bump n4k_patch, start at dirty1
        new_n4k = prev["n4k_patch"] + 1
        return f"v{prev['major']}.{prev['minor']}.{prev['patch']}-n4k.nirmata.{new_n4k}-dirty1"

    # Should be unreachable
    raise ValueError(f"Unhandled release type: {curr_type!r}")


def main() -> None:
    curr_type = (
        os.environ.get("RELEASE_TYPE")
        or (sys.argv[1] if len(sys.argv) > 1 else "")
    ).lower().strip()

    base_version = (
        os.environ.get("BASE_VERSION")
        or (sys.argv[2] if len(sys.argv) > 2 else "")
    ).strip() or None

    if not curr_type:
        print(
            "ERROR: release type is required.\n"
            "Usage: determine <ga|rc|dirty> [base_version]\n"
            "       or set RELEASE_TYPE env var.",
            file=sys.stderr,
        )
        sys.exit(1)

    prev = find_prev_tag()

    if prev is None:
        if base_version:
            prev = parse_tag(base_version)
            if prev is None:
                print(
                    f"ERROR: BASE_VERSION {base_version!r} does not match expected format "
                    "v{{major}}.{{minor}}.{{patch}}-n4k.nirmata.{{n}}",
                    file=sys.stderr,
                )
                sys.exit(1)
        else:
            print(
                "ERROR: No previous n4k.nirmata tag found reachable from HEAD.\n"
                "Provide BASE_VERSION (e.g. v1.16.3-n4k.nirmata.0) to bootstrap.",
                file=sys.stderr,
            )
            sys.exit(1)

    try:
        new_tag = compute_next(prev, curr_type)
    except ValueError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        sys.exit(1)

    print(new_tag)

    gh_output = os.environ.get("GITHUB_OUTPUT")
    if gh_output:
        with open(gh_output, "a") as fh:
            fh.write(f"tag={new_tag}\n")


if __name__ == "__main__":
    main()
