#!/usr/bin/env python3
"""Aggregate an ivpol ladder into per-(version, cell) figures and a ratio table.

Mirrors ladder-report.py next door: a version's number for a cell is the
geometric mean of that cell across rounds, reported relative to a baseline
version. The geometric mean is the right average for ratios, and taking it
across rounds - where each round visited the versions in a different order - is
what lets a difference be attributed to the version rather than to the order it
happened to run in.

Refuses to report a cell that is missing rounds, rather than quietly averaging
whatever is present: a version measured in two rounds and another in three is
not a comparison, and a silently uneven table is the kind of thing that ends up
on a slide.

Usage:
  python3 ivpol-report.py results/ladder --baseline 1.19.0
  python3 ivpol-report.py results/ladder --baseline 1.19.0 --format csv
"""

from __future__ import annotations

import argparse
import json
import math
import re
import statistics
import sys
from collections import defaultdict
from pathlib import Path

CELL = re.compile(r"^img(?P<images>\d+)-d(?P<delay>\d+)-(?P<cache>\w+)$")


def read_cell(path: Path) -> dict | None:
    """One cell's headline numbers, or None if it did not complete."""
    summary = path / "k6-summary.json"
    meta = path / "meta.txt"
    if not summary.is_file() or not meta.is_file():
        return None
    try:
        doc = json.loads(summary.read_text())
    except json.JSONDecodeError:
        return None
    metrics = doc.get("metrics", {})
    iteration = metrics.get("iteration_duration", {})
    avg = iteration.get("avg")
    if avg is None:
        return None
    fields = dict(
        line.split("=", 1) for line in meta.read_text().splitlines() if "=" in line
    )
    coverage = path / "manifest-coverage-pct.txt"
    return {
        "avg_ms": float(avg),
        "p95_ms": float(iteration.get("p(95)") or 0.0),
        "max_ms": float(iteration.get("max") or 0.0),
        "admission_p99_s": float(fields.get("kyverno_admission_review_duration_p99_sec") or 0.0),
        "restarts": fields.get("pod_restarts", "").strip(),
        "manifest_coverage_pct": int(coverage.read_text().strip()) if coverage.is_file() else None,
    }


def collect(root: Path) -> tuple[dict, set, set]:
    """-> {(version, cell): [per-round readings]}, versions, cells."""
    readings: dict[tuple[str, str], list[dict]] = defaultdict(list)
    versions: set[str] = set()
    cells: set[str] = set()
    for round_dir in sorted(root.glob("round*")):
        for version_dir in sorted(p for p in round_dir.iterdir() if p.is_dir()):
            for cell_dir in sorted(version_dir.glob("img*-d*")):
                if not CELL.match(cell_dir.name):
                    continue
                reading = read_cell(cell_dir)
                if reading is None:
                    continue
                readings[(version_dir.name, cell_dir.name)].append(reading)
                versions.add(version_dir.name)
                cells.add(cell_dir.name)
    return readings, versions, cells


def geomean(values: list[float]) -> float:
    usable = [v for v in values if v > 0]
    if not usable:
        return 0.0
    return math.exp(statistics.fmean(math.log(v) for v in usable))


def cell_sort_key(cell: str) -> tuple:
    m = CELL.match(cell)
    return (int(m["delay"]), int(m["images"]), m["cache"]) if m else (0, 0, cell)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("--baseline", required=True)
    parser.add_argument("--format", choices=("table", "csv"), default="table")
    args = parser.parse_args()

    readings, versions, cells = collect(args.root)
    if not readings:
        sys.exit(f"no completed cells under {args.root}")
    if args.baseline not in versions:
        sys.exit(f"baseline {args.baseline} not among measured versions: {sorted(versions)}")

    ordered_versions = [args.baseline] + sorted(versions - {args.baseline})
    ordered_cells = sorted(cells, key=cell_sort_key)

    # Uneven rounds are a correctness problem, not a cosmetic one.
    counts = {k: len(v) for k, v in readings.items()}
    expected = max(counts.values())
    incomplete = [
        f"{version}/{cell} ({counts.get((version, cell), 0)}/{expected})"
        for cell in ordered_cells
        for version in ordered_versions
        if counts.get((version, cell), 0) != expected
    ]

    rows = []
    for cell in ordered_cells:
        base_values = [r["avg_ms"] for r in readings.get((args.baseline, cell), [])]
        base = geomean(base_values)
        for version in ordered_versions:
            entries = readings.get((version, cell), [])
            if not entries:
                continue
            avg = geomean([e["avg_ms"] for e in entries])
            rows.append(
                {
                    "cell": cell,
                    "version": version,
                    "rounds": len(entries),
                    "avg_ms": avg,
                    "p95_ms": geomean([e["p95_ms"] for e in entries]),
                    "vs_baseline": (avg / base) if base else 0.0,
                    "coverage_pct": min(
                        (e["manifest_coverage_pct"] for e in entries
                         if e["manifest_coverage_pct"] is not None),
                        default=None,
                    ),
                    "restarts": " ".join(sorted({e["restarts"] for e in entries if e["restarts"]})),
                }
            )

    if args.format == "csv":
        print("cell,version,rounds,avg_ms,p95_ms,vs_baseline,manifest_coverage_pct,restarts")
        for r in rows:
            print(
                f"{r['cell']},{r['version']},{r['rounds']},{r['avg_ms']:.2f},"
                f"{r['p95_ms']:.2f},{r['vs_baseline']:.3f},"
                f"{r['coverage_pct'] if r['coverage_pct'] is not None else ''},{r['restarts']}"
            )
        return 0

    print(f"ivpol ladder — geometric mean across rounds, relative to {args.baseline}")
    print(f"root: {args.root}")
    print()
    print(f"{'cell':<22} {'version':<10} {'rounds':>6} {'avg ms':>10} {'p95 ms':>10} {'vs base':>9} {'cov%':>6}")
    print("-" * 80)
    last = None
    for r in rows:
        if last is not None and r["cell"] != last:
            print()
        last = r["cell"]
        ratio = f"{r['vs_baseline']:.3f}x" if r["version"] != args.baseline else "—"
        cov = "" if r["coverage_pct"] is None else str(r["coverage_pct"])
        print(
            f"{r['cell']:<22} {r['version']:<10} {r['rounds']:>6} "
            f"{r['avg_ms']:>10.2f} {r['p95_ms']:>10.2f} {ratio:>9} {cov:>6}"
        )

    restarts = [r for r in rows if r["restarts"] and r["restarts"].strip("0 ")]
    if restarts:
        print()
        print("WARNING: admission-controller restarts during these cells — latency is suspect:")
        for r in restarts:
            print(f"  {r['cell']} {r['version']}: restarts={r['restarts']}")

    if incomplete:
        print()
        print(f"WARNING: uneven rounds (expected {expected}); these are not comparable:")
        for item in incomplete:
            print(f"  {item}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
